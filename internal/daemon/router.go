package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/coder/websocket"

	"github.com/thesahibnanda-max/relay/internal/proto"
	"github.com/thesahibnanda-max/relay/internal/store"
)

// Loop and storm guards (see the plan's "Loop/storm guards").
const (
	// MaxHops: a reply chain this deep is held for a human, and again at every
	// further multiple, so two agents can never chat unattended forever.
	MaxHops = 8

	pairLimit    = 20 // messages per sender->target pair per window
	senderLimit  = 60 // messages per sender per window
	limitWindow  = time.Minute
	dupWindow    = 30 * time.Second
	maxWaitSecs  = 45
	maxRPCFlight = 16 // concurrent RPCs per agent connection
)

// ---- per-connection state ------------------------------------------------------

// liveState is what an agent last said its tool is doing (not persisted).
type liveState struct {
	State    string
	PlanMode bool
	At       time.Time
}

// slidingWindow counts events in the last window.
type slidingWindow struct {
	mu sync.Mutex
	at map[string][]time.Time
}

func (w *slidingWindow) allow(key string, limit int, window time.Duration, now time.Time) (ok bool, retryAfter time.Duration) {
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.at == nil {
		w.at = map[string][]time.Time{}
	}
	cut := now.Add(-window)
	kept := w.at[key][:0]
	for _, t := range w.at[key] {
		if t.After(cut) {
			kept = append(kept, t)
		}
	}
	if len(kept) >= limit {
		w.at[key] = kept
		return false, kept[0].Add(window).Sub(now)
	}
	w.at[key] = append(kept, now)
	return true, 0
}

// sender is who a message comes from: an agent, the human, or Relay itself.
type sender struct {
	id           string // agent id; "" for user/relay
	name         string
	role         string
	canInterrupt bool
	canBroadcast bool
	human        bool // the user (relay send / approve): not rate limited, may interrupt
}

func (s *Server) agentSender(c *agentConn) sender {
	return sender{id: c.id, name: c.name, role: c.role, canInterrupt: c.canInterrupt, canBroadcast: c.canBroadcast}
}

func rpcErr(code, format string, a ...any) *proto.Error {
	return &proto.Error{Code: code, Message: fmt.Sprintf(format, a...)}
}

// ---- RPC dispatch --------------------------------------------------------------

func (s *Server) handleRPC(ctx context.Context, me *agentConn, req proto.RPC) {
	res := proto.Result{ID: req.ID}
	out, perr := s.dispatchRPC(ctx, me, req)
	if ctx.Err() != nil {
		return // the connection is gone: nobody is waiting for an answer
	}
	if perr != nil && perr.Code == proto.CodeInternal && s.isClosing() {
		// the store went away under us because the daemon is stopping: tell the
		// agent to retry (its client does, on the next daemon) rather than fail
		perr = rpcErr(proto.CodeOffline, "the relay daemon is restarting")
	}
	if perr != nil {
		res.Error = perr
	} else {
		res.OK = true
		if out != nil {
			b, err := json.Marshal(out)
			if err != nil {
				res.OK, res.Error = false, rpcErr(proto.CodeInternal, "internal error")
			} else {
				res.Result = b
			}
		}
	}
	if err := s.sendJSON(ctx, me.ws, proto.TypeResult, res); err != nil {
		me.ws.CloseNow()
	}
}

func (s *Server) dispatchRPC(ctx context.Context, me *agentConn, req proto.RPC) (any, *proto.Error) {
	if ok, wait := s.rpcs.allow(me.id, s.opt.RPCLimit, s.opt.RPCWindow, time.Now()); !ok {
		return nil, rpcErr(proto.CodeRateLimited, "too many requests; slow down and retry in %ds", int(wait.Seconds())+1)
	}
	switch req.Op {
	case proto.OpSend:
		var a proto.SendArgs
		if json.Unmarshal(req.Args, &a) != nil {
			return nil, rpcErr(proto.CodeBadRequest, "bad send arguments")
		}
		return s.routeSend(ctx, me.sessionID, s.agentSender(me), a)
	case proto.OpListAgents:
		return s.listPeers(ctx, me)
	case proto.OpApprove, proto.OpReject:
		var a proto.ApproveArgs
		_ = json.Unmarshal(req.Args, &a)
		return s.decideHeld(ctx, me.sessionID, me.id, a.ID, req.Op == proto.OpApprove)
	case proto.OpWait:
		var a proto.WaitArgs
		if json.Unmarshal(req.Args, &a) != nil {
			return nil, rpcErr(proto.CodeBadRequest, "bad wait arguments")
		}
		return s.waitMessage(ctx, me, a)
	case proto.OpContext:
		var a proto.ContextArgs
		if json.Unmarshal(req.Args, &a) != nil {
			return nil, rpcErr(proto.CodeBadRequest, "bad context arguments")
		}
		return s.getContext(ctx, me, a)
	case proto.OpMsgState:
		var a proto.MsgStateArgs
		if json.Unmarshal(req.Args, &a) != nil {
			return nil, rpcErr(proto.CodeBadRequest, "bad msg_state arguments")
		}
		return s.reportMsgState(ctx, me, a)
	case proto.OpAgentState:
		var a proto.AgentStateArgs
		if json.Unmarshal(req.Args, &a) != nil {
			return nil, rpcErr(proto.CodeBadRequest, "bad agent_state arguments")
		}
		s.mu.Lock()
		s.live[me.id] = liveState{State: a.State, PlanMode: a.PlanMode, At: time.Now()}
		s.mu.Unlock()
		return struct{}{}, nil
	}
	return nil, rpcErr(proto.CodeBadRequest, "unknown operation %q", req.Op)
}

// ---- sending -------------------------------------------------------------------

func view(m store.Message) proto.MessageView {
	return proto.MessageView{
		ID: m.ID, Session: m.SessionID, From: m.FromName, FromRole: m.FromRole, To: m.ToName, Kind: m.Kind,
		Priority: m.Priority, Thread: m.Thread, ReplyTo: m.ReplyTo, Body: m.Body, Hops: m.Hops,
		State: m.State, Detail: m.Detail, CreatedAt: m.CreatedAt,
	}
}

func (s *Server) peers(ctx context.Context, sessionID, selfID string) ([]proto.PeerInfo, error) {
	agents, err := s.st.ListAgents(ctx, sessionID, false)
	if err != nil {
		return nil, err
	}
	out := make([]proto.PeerInfo, 0, len(agents))
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, a := range agents {
		p := proto.PeerInfo{Name: a.Name, Role: a.Role, Tool: a.Tool, Status: a.Status, LastActive: a.LastSeenAt, Self: a.ID == selfID}
		if _, ok := s.conns[a.ID]; ok {
			p.Status = "connected"
			if l, ok := s.live[a.ID]; ok {
				p.State = l.State
				if l.At.After(p.LastActive) {
					p.LastActive = l.At
				}
			} else {
				p.State = "unknown"
			}
		}
		out = append(out, p)
	}
	return out, nil
}

func (s *Server) listPeers(ctx context.Context, me *agentConn) (any, *proto.Error) {
	ps, err := s.peers(ctx, me.sessionID, me.id)
	if err != nil {
		return nil, rpcErr(proto.CodeInternal, "internal error")
	}
	return proto.ListAgentsResult{Session: me.sessionID, Agents: ps}, nil
}

// sendTarget is one recipient of routeSend: either a real local agent (the
// single-machine path, unchanged since before the mesh) or one gossiped in
// from another daemon's mesh_agents. A remote target's hold policy (hop
// limit, approve-inbound) is decided by its OWNING daemon once the handoff
// reaches it - never guessed at here - so this carries only what routeSend
// needs to create a mirror row and hand it off.
type sendTarget struct {
	id, name       string
	ownerPeer      string // "" for a local agent
	approveInbound bool
	exited         bool
}

func (t sendTarget) remote() bool { return t.ownerPeer != "" }

func localTarget(a store.Agent) sendTarget {
	return sendTarget{id: a.ID, name: a.Name, approveInbound: a.ApproveInbound, exited: a.Status == "exited"}
}

// resolveTargets turns a "to" string into recipients. "all"/"role:" stay
// local-only for now: fanning a broadcast out across an arbitrary number of
// mesh peers is more naturally M-mesh-5's fan-out concern than this
// milestone's "make an already-known remote name addressable" scope. An
// exact name is checked against local agents first, then - only if nothing
// local matches - against this session's gossiped mesh_agents, which is
// what makes a remote agent addressable by name at all (see M-mesh-4).
func (s *Server) resolveTargets(ctx context.Context, sessionID string, from sender, to string) ([]sendTarget, *proto.Error) {
	all, err := s.st.ListAgents(ctx, sessionID, true)
	if err != nil {
		return nil, rpcErr(proto.CodeInternal, "internal error")
	}
	var agents []store.Agent // the ones that can still receive (exited ones only matter for an exact name)
	for _, a := range all {
		if a.Status != "exited" {
			agents = append(agents, a)
		}
	}
	listing := func(code, format string, a ...any) *proto.Error {
		e := rpcErr(code, format, a...)
		e.Agents, _ = s.peers(ctx, sessionID, from.id)
		return e
	}
	to = strings.TrimSpace(to)
	var out []sendTarget
	switch {
	case strings.EqualFold(to, "all"):
		if !from.canBroadcast && !from.human {
			return nil, rpcErr(proto.CodeForbidden, "your role may not broadcast; name one agent in \"to\"")
		}
		for _, a := range agents {
			if a.ID != from.id {
				out = append(out, localTarget(a))
			}
		}
		if len(out) == 0 {
			return nil, rpcErr(proto.CodeUnknownAgent, "there is nobody else in this session to send to")
		}
		return out, nil
	case strings.HasPrefix(strings.ToLower(to), "role:"):
		role := strings.ToLower(strings.TrimSpace(to[len("role:"):]))
		for _, a := range agents {
			if a.ID != from.id && strings.EqualFold(a.Role, role) {
				out = append(out, localTarget(a))
			}
		}
		switch len(out) {
		case 0:
			return nil, listing(proto.CodeUnknownAgent, "no agent with role %q in this session", role)
		case 1:
			return out, nil
		}
		return nil, listing(proto.CodeAmbiguousAgent, "%d agents have role %q; address one by exact name", len(out), role)
	case to == "":
		return nil, listing(proto.CodeUnknownAgent, "\"to\" is required: an exact agent name")
	}
	for _, a := range all {
		if strings.EqualFold(a.Name, to) {
			if a.ID == from.id {
				return nil, rpcErr(proto.CodeSelfSend, "you cannot send a message to yourself")
			}
			return []sendTarget{localTarget(a)}, nil
		}
	}
	if strings.EqualFold(to, "user") {
		return nil, rpcErr(proto.CodeUnknownAgent, "the user is not an addressable agent: put what they need to know in your normal reply to them")
	}
	if remote, err := s.st.FindMeshAgentByName(ctx, sessionID, to); err == nil {
		return []sendTarget{{id: remote.AgentID, name: remote.Name, ownerPeer: remote.OwnerPeer}}, nil
	}
	return nil, listing(proto.CodeUnknownAgent, "no agent named %q in this session", to)
}

// markParentDone marks a reply's parent message answered, on whichever
// daemon happens to hold it (a local authoritative row if the parent
// originated here, or the authoritative row for an inbound handoff this
// daemon owns) - and, if that parent's sender lives on another daemon,
// pushes it a receipt so its mirror row learns "answered" without waiting
// for the next Resync.
func (s *Server) markParentDone(ctx context.Context, parent *store.Message, replyID string) {
	if parent == nil {
		return
	}
	upd, changed, err := s.st.Advance(ctx, parent.ID, store.MsgDone, "answered by "+replyID)
	if err != nil || !changed {
		return
	}
	if upd.FromPeer != "" {
		s.spawn(func() { s.pushMeshReceipt(upd) })
	}
}

// routeSend validates and stores a message (one per recipient) and pushes it.
func (s *Server) routeSend(ctx context.Context, sessionID string, from sender, a proto.SendArgs) (*proto.SendResult, *proto.Error) {
	sess, err := s.st.GetSession(ctx, sessionID)
	if err != nil {
		return nil, rpcErr(proto.CodeSessionNotFound, "no such session")
	}
	if sess.Status != "active" {
		return nil, rpcErr(proto.CodeSessionEnded, "this session has ended")
	}
	body := strings.TrimSpace(a.Body)
	switch {
	case body == "":
		return nil, rpcErr(proto.CodeBadRequest, "\"body\" is empty")
	case len(a.Body) > proto.MaxBodyBytes:
		return nil, rpcErr(proto.CodeTooLarge, "message is %d bytes; the limit is %d (send a summary, or point at a file path)", len(a.Body), proto.MaxBodyBytes)
	case !utf8.ValidString(a.Body):
		return nil, rpcErr(proto.CodeBadRequest, "\"body\" is not valid UTF-8 text")
	}
	prio, perr := proto.ParsePriority(a.Priority)
	if perr != nil {
		return nil, rpcErr(proto.CodeBadPriority, "%v", perr)
	}

	// A reply carries its parent: it defaults the recipient, kind, thread and hop count.
	var parent *store.Message
	if a.ReplyTo != "" {
		p, err := s.st.GetMessage(ctx, a.ReplyTo)
		if err != nil || p.SessionID != sess.ID || (from.id != "" && p.ToAgent != from.id && p.FromAgent != from.id) {
			return nil, rpcErr(proto.CodeNotFound, "reply_to %q is not a message you received or sent in this session", a.ReplyTo)
		}
		parent = &p
		if strings.TrimSpace(a.To) == "" {
			if p.FromAgent == "" || p.FromAgent == from.id {
				return nil, rpcErr(proto.CodeUnknownAgent, "cannot infer a recipient for this reply; set \"to\"")
			}
			a.To = p.FromName
		}
	}
	kind := a.Kind
	if kind == "" {
		kind = store.KindTask
		if parent != nil {
			kind = store.KindAnswer
		}
	}
	if !proto.ValidKind(kind) {
		return nil, rpcErr(proto.CodeBadKind, "kind %q: use one of %s", kind, strings.Join(proto.Kinds, ", "))
	}

	targets, terr := s.resolveTargets(ctx, sess.ID, from, a.To)
	if terr != nil {
		return nil, terr
	}
	for _, t := range targets {
		if t.exited {
			return nil, rpcErr(proto.CodeTargetGone, "agent %q has exited and can no longer receive messages", t.name)
		}
	}

	var notes []string
	if prio == store.P0 && !from.canInterrupt && !from.human {
		prio = store.P1
		notes = append(notes, "priority lowered from interrupt to high: your role may not interrupt")
	}
	hops, thread := 0, ""
	if parent != nil {
		hops, thread = parent.Hops+1, parent.Thread
	}

	now := time.Now()
	res := &proto.SendResult{Kind: kind, Priority: proto.PriorityName(prio)}
	for _, t := range targets {
		if dup, found, err := s.st.FindDuplicate(ctx, from.id, t.id, kind, a.Body, now.Add(-dupWindow)); err == nil && found {
			res.IDs = append(res.IDs, dup.ID)
			res.To = append(res.To, t.name)
			res.State = dup.State
			notes = append(notes, fmt.Sprintf("identical message to %s already sent %ds ago (%s); not sent again", t.name, int(now.Sub(dup.CreatedAt).Seconds()), dup.ID))
			continue
		}

		if !from.human && from.id != "" {
			if ok, wait := s.pairs.allow(from.id+">"+t.id, s.opt.PairLimit, limitWindow, now); !ok {
				return nil, rpcErr(proto.CodeRateLimited, "too many messages from you to %s (limit %d/min); wait %ds or combine them", t.name, s.opt.PairLimit, int(wait.Seconds())+1)
			}
			if ok, wait := s.senders.allow(from.id, s.opt.SenderLimit, limitWindow, now); !ok {
				return nil, rpcErr(proto.CodeRateLimited, "you are sending too many messages (limit %d/min); wait %ds", s.opt.SenderLimit, int(wait.Seconds())+1)
			}
		}

		if t.remote() {
			// The recipient's owning daemon decides the actual hold policy
			// (hop limit, its own approve-inbound flag) when the handoff
			// reaches it - this mirror row's state is provisional until its
			// first receipt corrects it, which usually arrives within the
			// same RPC's round trip to that daemon, just not this one.
			m, err := s.st.CreateMessage(ctx, store.Message{
				SessionID: sess.ID, FromAgent: from.id, FromName: from.name, FromRole: from.role,
				ToAgent: t.id, ToName: t.name, ToPeer: t.ownerPeer, Kind: kind, Priority: prio, Thread: thread,
				ReplyTo: a.ReplyTo, Body: a.Body, Hops: hops, State: store.MsgQueued,
				Origin: store.OriginMirror, ExpiresAt: now.Add(s.opt.MessageTTL),
			})
			if err != nil {
				s.log.Error("create mirror message", "err", err)
				return nil, rpcErr(proto.CodeInternal, "internal error")
			}
			res.IDs = append(res.IDs, m.ID)
			res.To = append(res.To, t.name)
			res.State = m.State
			notes = append(notes, fmt.Sprintf("%s is on another machine: delivery is confirmed once it reaches them", t.name))
			s.log.Info("message handed off to mesh peer", "id", m.ID, "from", from.name, "to", t.name, "peer", t.ownerPeer)
			s.markParentDone(ctx, parent, m.ID)
			s.spawn(func() { s.sendMeshHandoff(m) })
			continue
		}

		state, detail := store.MsgQueued, ""
		switch {
		case hops >= MaxHops && hops%MaxHops == 0:
			state, detail = store.MsgHeld, fmt.Sprintf("hop limit: this reply chain is %d messages deep; a human must approve it to continue", hops)
		case t.approveInbound && !from.human:
			state, detail = store.MsgHeld, "awaiting human approval (target runs with --approve-inbound)"
		}
		m, err := s.st.CreateMessage(ctx, store.Message{
			SessionID: sess.ID, FromAgent: from.id, FromName: from.name, FromRole: from.role,
			ToAgent: t.id, ToName: t.name, Kind: kind, Priority: prio, Thread: thread, ReplyTo: a.ReplyTo,
			Body: a.Body, Hops: hops, State: state, Detail: detail, ExpiresAt: now.Add(s.opt.MessageTTL),
		})
		if err != nil {
			s.log.Error("create message", "err", err)
			return nil, rpcErr(proto.CodeInternal, "internal error")
		}
		res.IDs = append(res.IDs, m.ID)
		res.To = append(res.To, t.name)
		res.State = m.State
		if !s.connected(t.id) {
			notes = append(notes, fmt.Sprintf("%s is not connected right now: the message is queued and delivered if it comes back (it expires after %s)", t.name, s.opt.MessageTTL.Round(time.Minute)))
		}
		if state == store.MsgHeld {
			notes = append(notes, fmt.Sprintf("held for %s: %s", t.name, detail))
		}
		s.log.Info("message accepted", "id", m.ID, "from", from.name, "to", t.name, "kind", kind, "priority", prio, "state", m.State)
		s.markParentDone(ctx, parent, m.ID)
		s.spawn(func() { s.flush(t.id, false) })
		if m.State == store.MsgHeld {
			s.spawn(func() { s.notifyHeld(t.id, false) })
		}
	}
	res.ID = res.IDs[0]
	if len(res.IDs) == 1 {
		res.IDs = nil
	}
	res.Note = strings.Join(notes, "; ")
	return res, nil
}

// ---- delivery to the target agent ------------------------------------------------

// flush pushes an agent's pending messages to its scheduler. With resend, ones
// that were dispatched earlier but never confirmed go again (the agent
// de-duplicates by id): that is the at-least-once part, used on (re)connect.
func (s *Server) flush(agentID string, resend bool) {
	s.mu.Lock()
	c := s.conns[agentID]
	s.mu.Unlock()
	if c == nil || !c.ready.Load() {
		return
	}
	c.flushMu.Lock()
	defer c.flushMu.Unlock()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	msgs, err := s.st.Deliverable(ctx, agentID)
	if err != nil {
		s.log.Error("list deliverable", "err", err, "agent", agentID)
		return
	}
	now := time.Now()
	for _, m := range msgs {
		if m.State == store.MsgDispatched && !resend {
			continue
		}
		if !m.ExpiresAt.After(now) {
			continue // the expiry sweep will report it
		}
		if err := s.sendJSON(ctx, c.ws, proto.TypeDeliver, proto.Deliver{Message: view(m)}); err != nil {
			c.ws.CloseNow()
			return
		}
		if m.State == store.MsgDispatched {
			_ = s.st.Redispatch(ctx, m.ID)
		} else if _, _, err := s.st.Advance(ctx, m.ID, store.MsgDispatched, ""); err != nil && !errors.Is(err, store.ErrBadTransition) {
			s.log.Error("mark dispatched", "err", err, "id", m.ID)
		}
	}
}

func (s *Server) reportMsgState(ctx context.Context, me *agentConn, a proto.MsgStateArgs) (any, *proto.Error) {
	switch a.State {
	case store.MsgInjected, store.MsgAcknowledged, store.MsgDone:
	default:
		return nil, rpcErr(proto.CodeBadRequest, "state %q cannot be reported by an agent", a.State)
	}
	m, err := s.st.GetMessage(ctx, a.ID)
	if err != nil || m.ToAgent != me.id {
		return nil, rpcErr(proto.CodeNotFound, "no such message addressed to you")
	}
	upd, changed, err := s.st.Advance(ctx, m.ID, a.State, "")
	if err != nil && !errors.Is(err, store.ErrBadTransition) {
		return nil, rpcErr(proto.CodeInternal, "internal error")
	}
	if changed && upd.FromPeer != "" {
		s.spawn(func() { s.pushMeshReceipt(upd) })
	}
	return struct{}{}, nil
}

// ---- approval ---------------------------------------------------------------------

// decideHeld releases (approve) or refuses a held message. agentID limits it to
// messages addressed to that agent (an agent approving its own inbox); the
// human passes "". With no id it picks the oldest held message.
func (s *Server) decideHeld(ctx context.Context, sessionID, agentID, id string, approve bool) (*proto.ApproveResult, *proto.Error) {
	var m store.Message
	if id == "" {
		held, err := s.st.ListMessages(ctx, store.MessageFilter{Session: sessionID, To: agentID, States: []string{store.MsgHeld}, Limit: 1000})
		if err != nil {
			return nil, rpcErr(proto.CodeInternal, "internal error")
		}
		if len(held) == 0 {
			return nil, rpcErr(proto.CodeNotFound, "no held messages")
		}
		m = held[len(held)-1] // ListMessages is newest first
	} else {
		got, err := s.st.GetMessage(ctx, id)
		if err != nil || (sessionID != "" && got.SessionID != sessionID) || (agentID != "" && got.ToAgent != agentID) {
			return nil, rpcErr(proto.CodeNotFound, "no such held message")
		}
		m = got
	}
	if m.State != store.MsgHeld {
		return nil, rpcErr(proto.CodeNotFound, "message %s is %s, not held", m.ID, m.State)
	}
	to, detail := store.MsgQueued, "approved by a human"
	if !approve {
		to, detail = store.MsgRejected, "rejected by a human"
	}
	upd, _, err := s.st.Advance(ctx, m.ID, to, detail)
	if err != nil {
		return nil, rpcErr(proto.CodeInternal, "internal error")
	}
	s.log.Info("held message decided", "id", m.ID, "approved", approve)
	if upd.FromPeer != "" {
		s.spawn(func() { s.pushMeshReceipt(upd) })
	}
	s.spawn(func() { s.notifyHeld(upd.ToAgent, false) })
	if approve {
		s.spawn(func() { s.flush(upd.ToAgent, false) })
	} else {
		s.notifySender(upd, "was rejected by the user")
	}
	return &proto.ApproveResult{ID: upd.ID, State: upd.State}, nil
}

// ---- waiting ------------------------------------------------------------------------

func (s *Server) waitMessage(ctx context.Context, me *agentConn, a proto.WaitArgs) (any, *proto.Error) {
	m, err := s.st.GetMessage(ctx, a.ID)
	if err != nil || (m.FromAgent != me.id && m.ToAgent != me.id) {
		return nil, rpcErr(proto.CodeNotFound, "no such message sent by or to you")
	}
	timeout := time.Duration(a.TimeoutS * float64(time.Second))
	if timeout <= 0 {
		timeout = 10 * time.Second
	}
	if timeout > maxWaitSecs*time.Second {
		timeout = maxWaitSecs * time.Second // tool calls have their own timeouts; poll again for longer
	}
	deadline := time.NewTimer(timeout)
	defer deadline.Stop()
	tick := time.NewTicker(200 * time.Millisecond)
	defer tick.Stop()
	for {
		m, err = s.st.GetMessage(ctx, a.ID)
		if err != nil {
			return nil, rpcErr(proto.CodeInternal, "internal error")
		}
		res := proto.WaitResult{ID: m.ID, State: m.State, Detail: m.Detail}
		if replies, _ := s.st.Replies(ctx, m.ID); len(replies) > 0 {
			v := view(replies[0])
			res.Reply = &v
			return res, nil
		}
		if store.IsTerminal(m.State) || m.State == store.MsgAcknowledged {
			return res, nil
		}
		select {
		case <-ctx.Done():
			return nil, rpcErr(proto.CodeOffline, "connection closed")
		case <-deadline.C:
			res.TimedOut = true
			return res, nil
		case <-tick.C:
		}
	}
}

// ---- notices, expiry --------------------------------------------------------------

// notifySender tells the author of an agent-sent message that it failed.
// Notices about notices, or about messages from the user or Relay, are never sent.
func (s *Server) notifySender(m store.Message, why string) {
	if m.FromAgent == "" || m.Kind == store.KindNotify {
		return
	}
	snippet := strings.Join(strings.Fields(m.Body), " ")
	if len(snippet) > 60 {
		snippet = snippet[:57] + "..."
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	n, err := s.st.CreateMessage(ctx, store.Message{
		SessionID: m.SessionID, FromName: "relay", ToAgent: m.FromAgent, ToName: m.FromName,
		Kind: store.KindNotify, Priority: store.P2, ReplyTo: "",
		Body: fmt.Sprintf("Your message %s to %s (%q) %s.", m.ID, m.ToName, snippet, why),
	})
	if err != nil {
		s.log.Error("notify sender", "err", err)
		return
	}
	s.spawn(func() { s.flush(n.ToAgent, false) })
}

func (s *Server) expireLoop(ctx context.Context) {
	t := time.NewTicker(s.opt.ExpireEvery)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.sweep(ctx)
		}
	}
}

func (s *Server) sweep(ctx context.Context) {
	s.reapGone(ctx)
	s.resyncAllMeshSessions(ctx)
	expired, err := s.st.ExpireDue(ctx, time.Now())
	if err != nil {
		s.log.Error("expire messages", "err", err)
		return
	}
	for _, m := range expired {
		s.log.Info("message expired", "id", m.ID, "to", m.ToName)
		// A local authoritative row whose sender lives on another daemon has
		// no local agent to notify (FromAgent is that daemon's, not ours) -
		// tell its owning peer instead. A local-to-local message, or a
		// mirror row we gave up waiting on (its own FromAgent is always
		// ours), still gets the ordinary local notice.
		if m.FromPeer != "" {
			s.spawn(func() { s.pushMeshReceipt(m) })
			continue
		}
		s.notifySender(m, "expired before it was delivered")
	}
}

// failFor marks everything still waiting for a departed agent undeliverable.
func (s *Server) failFor(ctx context.Context, agentID string) {
	failed, err := s.st.FailPending(ctx, agentID, store.MsgUndeliverable, "the target agent exited")
	if err != nil {
		s.log.Error("fail pending messages", "err", err)
		return
	}
	for _, m := range failed {
		if m.FromPeer != "" {
			s.spawn(func() { s.pushMeshReceipt(m) })
			continue
		}
		s.notifySender(m, "could not be delivered: "+m.ToName+" exited")
	}
}

// ---- helpers used by the connection loop -------------------------------------------

func (s *Server) newConn(id, name, role string, h proto.Hello, sessionID string, ws *websocket.Conn) *agentConn {
	return &agentConn{id: id, name: name, role: role, sessionID: sessionID, ws: ws, canInterrupt: h.CanInterrupt, canBroadcast: h.CanBroadcast,
		rpcSlots: make(chan struct{}, maxRPCFlight)}
}

// notifyHeld tells an agent how many messages are held for its human, so its
// terminal can ring the bell (approval is only possible with a human's help).
func (s *Server) notifyHeld(agentID string, onlyIfAny bool) {
	s.mu.Lock()
	c := s.conns[agentID]
	s.mu.Unlock()
	if c == nil || !c.ready.Load() {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	held, err := s.st.ListMessages(ctx, store.MessageFilter{To: agentID, States: []string{store.MsgHeld}, Limit: 1000})
	if err != nil || (onlyIfAny && len(held) == 0) {
		return
	}
	if err := s.sendJSON(ctx, c.ws, proto.TypeNotice, proto.Notice{Held: len(held)}); err != nil {
		c.ws.CloseNow()
	}
}

// reapGone treats agents that have been disconnected longer than the grace
// period as exited (their relay process was killed: nothing will reconnect),
// so senders learn about it instead of waiting for TTLs.
func (s *Server) reapGone(ctx context.Context) {
	stale, err := s.st.StaleDisconnected(ctx, time.Now().Add(-s.opt.DisconnectGrace))
	if err != nil {
		s.log.Error("find stale agents", "err", err)
		return
	}
	for _, a := range stale {
		if s.connected(a.ID) {
			continue
		}
		s.log.Info("agent never came back; marking it exited", "agent", a.Name, "agent_id", a.ID)
		_ = s.st.SetAgentStatus(ctx, a.ID, "exited", nil)
		s.failFor(ctx, a.ID)
		s.gossipRoster(a.SessionID)
		if sess, err := s.st.GetSession(ctx, a.SessionID); err == nil && sess.Kind == "solo" {
			_ = s.st.EndSession(ctx, sess.ID)
		}
	}
}

func (s *Server) isClosing() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.closing
}
