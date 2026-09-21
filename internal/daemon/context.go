package daemon

import (
	"context"
	"encoding/json"
	"strings"
	"time"

	"github.com/thesahibnanda-max/relay/internal/proto"
	"github.com/thesahibnanda-max/relay/internal/redact"
	"github.com/thesahibnanda-max/relay/internal/store"
)

// getContext serves relay_get_context: it reads another agent's conversation
// from the turns already in the store, so the target is never disturbed. Text
// passes the redaction filter and a size cap before it crosses to the caller.
func (s *Server) getContext(ctx context.Context, me *agentConn, a proto.ContextArgs) (any, *proto.Error) {
	name := strings.TrimSpace(a.Agent)
	if name == "" {
		e := rpcErr(proto.CodeUnknownAgent, "\"agent\" is required: the exact name of the agent whose conversation you want")
		e.Agents, _ = s.peers(ctx, me.sessionID, me.id)
		return nil, e
	}
	target, err := s.st.FindAgent(ctx, me.sessionID, name)
	if err != nil {
		e := rpcErr(proto.CodeUnknownAgent, "no agent named %q in this session", name)
		e.Agents, _ = s.peers(ctx, me.sessionID, me.id)
		return nil, e
	}
	mode := a.Mode
	if mode == "" {
		mode = proto.ContextTail
	}
	n := a.N
	if n <= 0 {
		n = 10
	}
	if n > 50 {
		n = 50
	}
	q := store.TurnQuery{Limit: n}
	switch mode {
	case proto.ContextTail:
	case proto.ContextLastAnswer:
		q.Limit, q.Role = 1, "assistant"
	case proto.ContextSearch:
		if strings.TrimSpace(a.Query) == "" {
			return nil, rpcErr(proto.CodeBadRequest, "mode \"search\" needs a \"query\"")
		}
		q.Contains = a.Query
	case proto.ContextSince:
		since, ok := parseSince(a.Since, time.Now())
		if !ok {
			return nil, rpcErr(proto.CodeBadRequest, "mode \"since\" needs \"since\": a duration like \"10m\" or an RFC 3339 time")
		}
		q.Since, q.Limit = since, 200
	default:
		return nil, rpcErr(proto.CodeBadRequest, "mode %q: use tail, last_answer, since or search", mode)
	}

	rows, err := s.st.Turns(ctx, target.ID, q)
	if err != nil {
		s.log.Error("read turns", "err", err)
		return nil, rpcErr(proto.CodeInternal, "internal error")
	}
	res := &proto.ContextResult{Agent: target.Name, Mode: mode, Turns: []proto.TurnView{}}
	budget := proto.MaxContextBytes
	for i := len(rows) - 1; i >= 0; i-- { // newest first, until the budget is spent
		var t proto.TurnView
		if json.Unmarshal([]byte(rows[i].Payload), &t) != nil {
			continue
		}
		t.Seq = rows[i].Seq
		if t.TS.IsZero() {
			t.TS = rows[i].TS
		}
		t.Text = redact.Text(t.Text)
		if len(t.Text) > budget {
			res.Truncated = true
			if budget > 200 {
				t.Text = t.Text[:budget] + "…[truncated]"
				res.Turns = append([]proto.TurnView{t}, res.Turns...)
			}
			break
		}
		budget -= len(t.Text)
		res.Turns = append([]proto.TurnView{t}, res.Turns...)
	}
	if len(res.Turns) == 0 {
		res.Note = noTurnsNote(mode, target)
	}
	return res, nil
}

func noTurnsNote(mode string, t store.Agent) string {
	base := "nothing to show"
	switch mode {
	case proto.ContextSearch:
		base = "no matching turns"
	case proto.ContextSince:
		base = "no turns in that period"
	}
	if t.Status == "exited" {
		return base + " (the agent has exited)"
	}
	return base + ": the agent may not have started a conversation yet, or was started with --record=off"
}

// parseSince accepts a Go-style duration ("90s", "10m", "2h") meaning "that
// long ago", or an RFC 3339 timestamp.
func parseSince(v string, now time.Time) (time.Time, bool) {
	v = strings.TrimSpace(v)
	if d, err := time.ParseDuration(v); err == nil && d > 0 {
		return now.Add(-d), true
	}
	if t, err := time.Parse(time.RFC3339, v); err == nil {
		return t, true
	}
	return time.Time{}, false
}
