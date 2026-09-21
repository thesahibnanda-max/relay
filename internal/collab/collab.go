// Package collab is the agent-side glue of a Relay session: it owns the local
// message bus, feeds it from the daemon, types messages into the tool through
// the running agent's handle, and serves the MCP shim's requests over the
// control socket.
package collab

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"sync/atomic"
	"time"

	"github.com/thesahibnanda-max/relay/internal/agent"
	"github.com/thesahibnanda-max/relay/internal/bus"
	"github.com/thesahibnanda-max/relay/internal/link"
	"github.com/thesahibnanda-max/relay/internal/proto"
	"github.com/thesahibnanda-max/relay/internal/state"
)

// ChordPrefix is the reserved prefix key (Ctrl+\) for approving held messages
// in-terminal; only active on agents started with --approve-inbound.
const ChordPrefix = 0x1c

// Session is one agent's collaboration state.
type Session struct {
	bus *bus.Bus
	env *sessionEnv

	ident  proto.Welcome
	tool   string
	role   string
	lk     *link.Client
	handle atomic.Pointer[agent.Handle]

	cancel context.CancelFunc
	wg     sync.WaitGroup

	held atomic.Int32 // messages held for a human on this agent
	nat  native
}

// New creates the session before the daemon connection exists (its Deliver and
// Notice methods are the link callbacks). Call Bind once connected.
func New() *Session {
	s := &Session{}
	s.env = &sessionEnv{s: s}
	s.bus = bus.New(s.env)
	return s
}

// Bind attaches the daemon link (nil for a solo agent, which has no peers).
func (s *Session) Bind(lk *link.Client, tool, role string) {
	s.lk, s.tool, s.role = lk, tool, role
	if lk != nil {
		s.ident = lk.Identity()
	}
}

// Deliver is the link's OnDeliver callback.
func (s *Session) Deliver(m proto.MessageView) { s.bus.Add(m) }

// Notice is the link's OnNotice callback: a message is being held for a human.
func (s *Session) Notice(held int) {
	prev := int(s.held.Swap(int32(held)))
	if held > prev {
		if h := s.handle.Load(); h != nil {
			h.Bell()
		}
	}
}

// Bus exposes the local bus (bootstrap, tests).
func (s *Session) Bus() *bus.Bus { return s.bus }

// Attach starts scheduling once the tool is running.
func (s *Session) Attach(h *agent.Handle, approveInbound bool) {
	s.handle.Store(h)
	ctx, cancel := context.WithCancel(context.Background())
	s.cancel = cancel
	s.wg.Add(2)
	go func() { defer s.wg.Done(); s.bus.Run(ctx) }()
	go func() { defer s.wg.Done(); s.reportState(ctx, h) }()
	s.startNative(ctx)
	if approveInbound && s.lk != nil {
		h.SetChord(ChordPrefix, s.chord)
	}
}

// Stop ends scheduling; queued messages remain in the daemon.
func (s *Session) Stop() {
	if s.cancel != nil {
		s.cancel()
	}
	s.wg.Wait()
}

func (s *Session) chord(key byte) {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	switch key {
	case 'a':
		_ = s.lk.Call(ctx, proto.OpApprove, proto.ApproveArgs{}, nil)
	case 'r':
		_ = s.lk.Call(ctx, proto.OpReject, proto.ApproveArgs{}, nil)
	case 'i':
		if s.held.Load() > 0 {
			if h := s.handle.Load(); h != nil {
				h.Bell()
			}
		}
	}
}

// reportState tells the daemon what the tool is doing (for relay_list_agents)
// and drops the draft estimate once a dialog closes (keys typed into a dialog
// were answers, not draft text).
func (s *Session) reportState(ctx context.Context, h *agent.Handle) {
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	var last string
	var lastPlan, wasDialog bool
	for {
		snap := h.Snapshot()
		if wasDialog && snap.State != state.Dialog {
			h.ClearDraft()
		}
		wasDialog = snap.State == state.Dialog
		name := snap.State.String()
		if snap.Desynced {
			name = "unknown"
		}
		if s.lk != nil && (name != last || snap.PlanMode != lastPlan) {
			last, lastPlan = name, snap.PlanMode
			cctx, cancel := context.WithTimeout(ctx, 3*time.Second)
			_ = s.lk.Call(cctx, proto.OpAgentState, proto.AgentStateArgs{State: name, PlanMode: snap.PlanMode}, nil)
			cancel()
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// ---- bus environment -------------------------------------------------------------

type sessionEnv struct{ s *Session }

func (e *sessionEnv) handle() *agent.Handle { return e.s.handle.Load() }

func (e *sessionEnv) Snapshot() state.Snapshot {
	if h := e.handle(); h != nil {
		return h.Snapshot()
	}
	return state.Snapshot{State: state.Starting}
}

func (e *sessionEnv) UserQuietFor(d time.Duration) bool {
	if h := e.handle(); h != nil {
		return h.UserQuietFor(d)
	}
	return true
}

func (e *sessionEnv) DraftDirty() bool {
	if h := e.handle(); h != nil {
		return h.DraftDirty()
	}
	return false
}

func (e *sessionEnv) Inject(ctx context.Context, text string) error {
	h := e.handle()
	if h == nil {
		return errors.New("tool not running")
	}
	return h.Inject(ctx, text, agent.InjectOptions{Paste: true, Submit: true})
}

func (e *sessionEnv) Interrupt() error {
	h := e.handle()
	if h == nil {
		return errors.New("tool not running")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	err := h.Interrupt(ctx)
	if err == nil {
		// Esc cuts the turn short and Claude fires no Stop hook for that: drop
		// the "busy" the hook / transcript announced.
		h.ClearSignal("hook")
		h.ClearSignal("rollout")
	}
	return err
}

// Report tells the daemon what became of a message. Failures that retrying
// cannot fix (the daemon does not know the message) are swallowed.
func (e *sessionEnv) Report(ctx context.Context, id, st string) error {
	if e.s.lk == nil {
		return nil
	}
	err := e.s.lk.Call(ctx, proto.OpMsgState, proto.MsgStateArgs{ID: id, State: st}, nil)
	var pe *proto.Error
	if errors.As(err, &pe) && pe.Code != proto.CodeOffline && pe.Code != proto.CodeInternal {
		return nil
	}
	return err
}

// ---- control socket (MCP shim) ------------------------------------------------------

// tool-facing argument shapes (what the model sends)
type inboxArgs struct {
	Limit int  `json:"limit"`
	Peek  bool `json:"peek"`
}

type ackArgs struct {
	MsgID string `json:"msg_id"`
}

type waitArgs struct {
	MsgID    string  `json:"msg_id"`
	TimeoutS float64 `json:"timeout_s"`
}

type inboxMessage struct {
	ID        string    `json:"msg_id"`
	From      string    `json:"from"`
	FromRole  string    `json:"from_role,omitempty"`
	Kind      string    `json:"kind"`
	Priority  string    `json:"priority"`
	ReplyTo   string    `json:"reply_to,omitempty"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
}

// HandleCtl serves the MCP shim's operations for this agent.
func (s *Session) HandleCtl(ctx context.Context, op string, args json.RawMessage) (any, int, *proto.Error) {
	pending := s.bus.Pending()
	fail := func(err error) (any, int, *proto.Error) {
		var pe *proto.Error
		if errors.As(err, &pe) {
			return nil, pending, pe
		}
		return nil, pending, &proto.Error{Code: proto.CodeInternal, Message: err.Error()}
	}
	switch op {
	case "whoami", "list_agents":
		var l proto.ListAgentsResult
		if err := s.lk.Call(ctx, proto.OpListAgents, struct{}{}, &l); err != nil {
			return fail(err)
		}
		if op == "list_agents" {
			return l, pending, nil
		}
		return map[string]any{
			"name": s.ident.Agent.Name, "role": s.ident.Agent.Role, "tool": s.ident.Agent.Tool,
			"session_id": s.ident.Session.ID, "agents": l.Agents,
		}, pending, nil
	case "send":
		var a proto.SendArgs
		if json.Unmarshal(args, &a) != nil {
			return nil, pending, &proto.Error{Code: proto.CodeBadRequest, Message: "bad arguments for relay_send"}
		}
		var r proto.SendResult
		if err := s.lk.Call(ctx, proto.OpSend, a, &r); err != nil {
			return fail(err)
		}
		return r, pending, nil
	case "inbox":
		var a inboxArgs
		_ = json.Unmarshal(args, &a)
		msgs := s.bus.Inbox(a.Limit, a.Peek)
		out := make([]inboxMessage, 0, len(msgs))
		for _, m := range msgs {
			out = append(out, inboxMessage{ID: m.ID, From: m.From, FromRole: m.FromRole, Kind: m.Kind, Priority: proto.PriorityName(m.Priority), ReplyTo: m.ReplyTo, Body: m.Body, CreatedAt: m.CreatedAt})
		}
		return map[string]any{"messages": out, "count": len(out)}, s.bus.Pending(), nil
	case "hook":
		return map[string]string{"output": s.HandleHook(args)}, pending, nil
	case "get_context":
		var a proto.ContextArgs
		if json.Unmarshal(args, &a) != nil {
			return nil, pending, &proto.Error{Code: proto.CodeBadRequest, Message: "bad arguments for relay_get_context"}
		}
		var r proto.ContextResult
		if err := s.lk.Call(ctx, proto.OpContext, a, &r); err != nil {
			return fail(err)
		}
		return r, pending, nil
	case "ack":
		var a ackArgs
		if json.Unmarshal(args, &a) != nil || a.MsgID == "" {
			return nil, pending, &proto.Error{Code: proto.CodeBadRequest, Message: "msg_id is required"}
		}
		s.bus.Ack(a.MsgID)
		return map[string]any{"acknowledged": a.MsgID}, s.bus.Pending(), nil
	case "wait":
		var a waitArgs
		if json.Unmarshal(args, &a) != nil || a.MsgID == "" {
			return nil, pending, &proto.Error{Code: proto.CodeBadRequest, Message: "msg_id is required"}
		}
		if a.TimeoutS <= 0 {
			a.TimeoutS = 20
		}
		var r proto.WaitResult
		wctx, cancel := context.WithTimeout(ctx, time.Duration(a.TimeoutS*float64(time.Second))+10*time.Second)
		defer cancel()
		if err := s.lk.Call(wctx, proto.OpWait, proto.WaitArgs{ID: a.MsgID, TimeoutS: a.TimeoutS}, &r); err != nil {
			return fail(err)
		}
		if r.Reply != nil {
			s.bus.Ack(r.Reply.ID) // the model has the answer now: do not type it into the terminal as well
		}
		return r, s.bus.Pending(), nil
	}
	return nil, pending, &proto.Error{Code: proto.CodeBadRequest, Message: "unknown operation " + op}
}
