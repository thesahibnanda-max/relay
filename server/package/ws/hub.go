package ws

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/coder/websocket"

	"github.com/thesahibnanda-max/relay/server/package/config"
	"github.com/thesahibnanda-max/relay/server/package/database/mongodb"
	"github.com/thesahibnanda-max/relay/server/package/session"
)

// Timeouts mirror the local daemon's own defaults for the same events -
// values proven in production there, not re-derived from scratch.
const (
	helloTimeout = 5 * time.Second
	pingEvery    = 15 * time.Second
	pongTimeout  = 10 * time.Second
	writeTimeout = 10 * time.Second
	readLimit    = 4 << 20 // 4 MiB

	defaultWaitSeconds = 10
	maxWaitSeconds     = 45 // mirrors the local daemon's relay_wait cap
)

// Interface accepts one WebSocket connection and drives it end to end.
type Interface interface {
	Accept(w http.ResponseWriter, r *http.Request)
}

type hub struct {
	cfg     config.Config
	session session.Interface
	reg     *registry
	live    *liveStates
	// pairLimit/senderLimit gate message traffic (checked in handleSend,
	// before any DB write - an over-limit send is rejected outright, not
	// queued); rpc-level limiting is per-connection instead, see conn.
	pairLimit   *keyedLimiter
	senderLimit *keyedLimiter
}

func New(cfg config.Config, sessionService session.Interface) (Interface, error) {
	if sessionService == nil {
		return nil, errors.New("ws: session service is nil")
	}
	return hub{
		cfg:         cfg,
		session:     sessionService,
		reg:         newRegistry(),
		live:        newLiveStates(),
		pairLimit:   newKeyedLimiter(cfg.PairRateLimit, time.Minute),
		senderLimit: newKeyedLimiter(cfg.SenderRateLimit, time.Minute),
	}, nil
}

func (h hub) Accept(w http.ResponseWriter, r *http.Request) {
	// InsecureSkipVerify accepts a connection regardless of Origin - this is
	// what "allow all origins" means for a WebSocket upgrade, which plain
	// CORS response headers don't govern the same way they govern a normal
	// HTTP request (see http.go for the REST-endpoint CORS middleware).
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}
	defer c.CloseNow()
	c.SetReadLimit(readLimit)

	cn := newConn(c, h.cfg.RPCRateLimit, h.cfg.RPCRateWindow, h.cfg.MaxInFlightRPCs)
	ctx := r.Context()
	result, ok := h.handshake(ctx, cn)
	if !ok {
		return
	}

	h.reg.set(result.AgentID, cn)
	defer h.reg.remove(result.AgentID)
	defer func() {
		// ctx is already (or about to be) Done() by the time this runs -
		// this DB write needs its own short-lived context, not the
		// connection's.
		dctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = h.session.Disconnect(dctx, result.SessionID, result.AgentID)
	}()

	pingCtx, stopPing := context.WithCancel(ctx)
	defer stopPing()
	go h.pingLoop(pingCtx, cn)

	h.serve(ctx, cn, result)
	cn.ws.Close(websocket.StatusNormalClosure, "bye")
}

// handshake reads the one Hello frame a connection must open with, joins
// the session it names, replies with a Welcome, and replays anything still
// owed to this agent from before it connected. ok is false if the
// connection should be torn down (a protocol violation or a failed join,
// either way an Error frame has already been sent).
func (h hub) handshake(ctx context.Context, cn *conn) (session.JoinResult, bool) {
	hctx, cancel := context.WithTimeout(ctx, helloTimeout)
	env, err := readEnvelope(hctx, cn.ws)
	cancel()
	if err != nil {
		return session.JoinResult{}, false
	}
	if env.Type != TypeHello {
		reject(ctx, cn, "bad_request", "expected hello")
		return session.JoinResult{}, false
	}
	var hello Hello
	if err := json.Unmarshal(env.Payload, &hello); err != nil {
		reject(ctx, cn, "bad_request", "malformed hello")
		return session.JoinResult{}, false
	}

	result, err := h.session.Join(ctx, session.JoinRequest{
		SessionID: hello.Session, Name: hello.Name, Token: hello.Token, Tool: hello.Tool, Role: hello.Role,
		ApproveInbound: hello.ApproveInbound, CanInterrupt: hello.CanInterrupt, CanBroadcast: hello.CanBroadcast,
	})
	if err != nil {
		reject(ctx, cn, joinErrorCode(err), err.Error())
		return session.JoinResult{}, false
	}

	if err := cn.writeTyped(ctx, TypeWelcome, Welcome{
		SessionID: result.SessionID, AgentID: result.AgentID, Name: result.Name,
		Token: result.Token, Resumed: result.Resumed,
	}); err != nil {
		return session.JoinResult{}, false
	}

	h.replayPending(ctx, cn, result)
	return result, true
}

// replayPending pushes every message still owed to a (re)connecting agent -
// this is what makes delivery survive a crash/reconnect instead of being a
// fire-and-forget push at send time.
func (h hub) replayPending(ctx context.Context, cn *conn, me session.JoinResult) {
	pending, err := h.session.PendingFor(ctx, me.SessionID, me.AgentID)
	if err != nil || len(pending) == 0 {
		return
	}
	names := h.agentNames(ctx, me.SessionID)
	for _, m := range pending {
		view := messageView(m, names)
		if err := cn.writeTyped(ctx, TypeDeliver, Deliver{Message: view}); err == nil {
			_ = h.session.MarkDispatched(ctx, me.SessionID, m.ID)
		}
	}
}

func (h hub) agentNames(ctx context.Context, sessionID string) map[string]string {
	agents, err := h.session.ListAgents(ctx, sessionID)
	names := make(map[string]string, len(agents))
	if err != nil {
		return names
	}
	for _, a := range agents {
		names[a.ID] = a.Name
	}
	return names
}

// serve handles every frame after the handshake until the connection closes.
func (h hub) serve(ctx context.Context, cn *conn, me session.JoinResult) {
	for {
		env, err := readEnvelope(ctx, cn.ws)
		if err != nil {
			return
		}
		switch env.Type {
		case TypeRPC:
			h.handleRPC(ctx, cn, me, env.Payload)
		case TypeAck:
			h.handleAck(ctx, me, env.Payload)
		default:
			_ = cn.writeTyped(ctx, TypeError, Error{Code: "bad_request", Message: "unknown frame type"})
		}
	}
}

func (h hub) handleRPC(ctx context.Context, cn *conn, me session.JoinResult, payload json.RawMessage) {
	var rpc RPC
	if err := json.Unmarshal(payload, &rpc); err != nil {
		_ = cn.writeTyped(ctx, TypeError, Error{Code: "bad_request", Message: "malformed rpc"})
		return
	}

	if !cn.rpcLimiter.allow() {
		h.replyError(ctx, cn, rpc.ID, "rate_limited", "too many requests, slow down")
		return
	}
	select {
	case cn.inflight <- struct{}{}:
	default:
		h.replyError(ctx, cn, rpc.ID, "rate_limited", "too many concurrent requests")
		return
	}

	if rpc.Op == OpWait {
		// Up to maxWaitSeconds - must not block this connection's one
		// reader goroutine, so it runs on its own, releasing its inflight
		// slot whenever it eventually replies. cn.writeTyped's internal
		// mutex is what makes it safe for this goroutine's eventual reply to
		// interleave with any other push to the same connection.
		go func() {
			defer func() { <-cn.inflight }()
			h.handleWait(ctx, cn, me, rpc)
		}()
		return
	}
	defer func() { <-cn.inflight }()

	switch rpc.Op {
	case OpSend:
		h.handleSend(ctx, cn, me, rpc)
	case OpListAgents:
		h.handleListAgents(ctx, cn, me, rpc)
	case OpContext:
		h.handleContext(ctx, cn, me, rpc)
	case OpApprove:
		h.handleApprove(ctx, cn, me, rpc)
	case OpReject:
		h.handleReject(ctx, cn, me, rpc)
	case OpListHeld:
		h.handleListHeld(ctx, cn, me, rpc)
	case OpMsgState:
		h.handleMsgState(ctx, cn, me, rpc)
	case OpAgentState:
		h.handleAgentState(ctx, cn, me, rpc)
	default:
		h.replyError(ctx, cn, rpc.ID, "bad_request", "unknown op")
	}
}

func (h hub) handleSend(ctx context.Context, cn *conn, me session.JoinResult, rpc RPC) {
	var args SendArgs
	if err := json.Unmarshal(rpc.Args, &args); err != nil {
		h.replyError(ctx, cn, rpc.ID, "bad_request", "malformed send args")
		return
	}
	// Rejected outright, before any DB write - mirrors the local daemon's
	// own rate limiting exactly (an over-limit send never gets persisted,
	// let alone queued).
	if !h.senderLimit.allow(me.AgentID) || !h.pairLimit.allow(me.AgentID+"|"+args.To) {
		h.replyError(ctx, cn, rpc.ID, "rate_limited", "sending too fast, slow down")
		return
	}

	outcome, err := h.session.Send(ctx, me.SessionID, me.AgentID, session.SendRequest{
		To: args.To, Body: args.Body, Kind: args.Kind, ReplyTo: args.ReplyTo,
		Priority: parsePriority(args.Priority),
	})
	if err != nil {
		h.replyError(ctx, cn, rpc.ID, "send_failed", err.Error())
		return
	}
	h.replyRPC(ctx, cn, rpc.ID, SendResult{
		ID: outcome.MessageID, State: outcome.State, Kind: outcome.Kind, Priority: outcome.Priority, Note: outcome.Note,
	})

	switch outcome.State {
	case mongodb.MessageStateQueued:
		view := MessageView{
			ID: outcome.MessageID, From: me.Name, FromID: me.AgentID,
			To: args.To, ToID: outcome.TargetAgentID,
			Kind: outcome.Kind, Priority: outcome.Priority, ReplyTo: args.ReplyTo, Body: args.Body,
			State: outcome.State,
		}
		h.dispatchIfOnline(ctx, me.SessionID, view)
	case mongodb.MessageStateHeld:
		h.pushNotice(ctx, me.SessionID, outcome.TargetAgentID)
	}
}

// dispatchIfOnline pushes a message live to its recipient if currently
// connected, marking it dispatched on success. Shared by handleSend (a
// freshly created message) and handleApprove/flushPending (a message that
// just left held), so both paths use one push-and-mark-dispatched rule.
func (h hub) dispatchIfOnline(ctx context.Context, sessionID string, view MessageView) bool {
	target, online := h.reg.get(view.ToID)
	if !online {
		return false
	}
	if err := target.writeTyped(ctx, TypeDeliver, Deliver{Message: view}); err != nil {
		return false
	}
	_ = h.session.MarkDispatched(ctx, sessionID, view.ID)
	return true
}

// flushPending pushes every currently-queued message for agentID if it's
// online right now, marking each dispatched - mirrors the local daemon's own
// decideHeld -> flush behavior (approving one held message also flushes
// whatever else was already queued for the same agent, not just the one
// just approved).
func (h hub) flushPending(ctx context.Context, sessionID, agentID string) {
	if _, online := h.reg.get(agentID); !online {
		return
	}
	pending, err := h.session.PendingFor(ctx, sessionID, agentID)
	if err != nil {
		return
	}
	names := h.agentNames(ctx, sessionID)
	for _, m := range pending {
		if m.State == mongodb.MessageStateQueued {
			h.dispatchIfOnline(ctx, sessionID, messageView(m, names))
		}
	}
}

// pushNotice tells agentID (if currently connected) how many messages are
// held for it right now - fired whenever that count might have changed (a
// new message just became held for it, or one of its held messages just
// left held via approve/reject).
func (h hub) pushNotice(ctx context.Context, sessionID, agentID string) {
	cn, online := h.reg.get(agentID)
	if !online {
		return
	}
	held, err := h.session.HeldCount(ctx, sessionID, agentID)
	if err != nil {
		return
	}
	_ = cn.writeTyped(ctx, TypeNotice, Notice{Held: held})
}

func (h hub) handleListAgents(ctx context.Context, cn *conn, me session.JoinResult, rpc RPC) {
	agents, err := h.session.ListAgents(ctx, me.SessionID)
	if err != nil {
		h.replyError(ctx, cn, rpc.ID, "list_failed", err.Error())
		return
	}
	out := make([]AgentInfo, len(agents))
	for i, a := range agents {
		info := AgentInfo{ID: a.ID, Name: a.Name, Tool: a.Tool, Role: a.Role, Status: a.Status}
		// Live tool state only means anything for an agent connected right
		// now - mirrors the local daemon's own rule exactly.
		if _, online := h.reg.get(a.ID); online {
			info.Status = "connected"
			info.State = h.live.get(a.ID)
		}
		out[i] = info
	}
	h.replyRPC(ctx, cn, rpc.ID, ListAgentsResult{Agents: out})
}

func (h hub) handleContext(ctx context.Context, cn *conn, me session.JoinResult, rpc RPC) {
	var args ContextArgs
	if err := json.Unmarshal(rpc.Args, &args); err != nil {
		h.replyError(ctx, cn, rpc.ID, "bad_request", "malformed context args")
		return
	}
	messages, err := h.session.Context(ctx, me.SessionID, me.AgentID, args.Agent, args.N)
	if err != nil {
		h.replyError(ctx, cn, rpc.ID, "context_failed", err.Error())
		return
	}
	names := h.agentNames(ctx, me.SessionID)
	views := make([]MessageView, len(messages))
	for i, m := range messages {
		views[i] = messageView(m, names)
	}
	h.replyRPC(ctx, cn, rpc.ID, GetContextResult{Agent: args.Agent, Messages: views})
}

func (h hub) handleWait(ctx context.Context, cn *conn, me session.JoinResult, rpc RPC) {
	var args WaitArgs
	if err := json.Unmarshal(rpc.Args, &args); err != nil {
		h.replyError(ctx, cn, rpc.ID, "bad_request", "malformed wait args")
		return
	}
	timeout := time.Duration(args.TimeoutS * float64(time.Second))
	if timeout <= 0 {
		timeout = defaultWaitSeconds * time.Second
	}
	if timeout > maxWaitSeconds*time.Second {
		timeout = maxWaitSeconds * time.Second
	}

	outcome, err := h.session.Wait(ctx, me.SessionID, me.AgentID, args.ID, timeout)
	if err != nil {
		h.replyError(ctx, cn, rpc.ID, "wait_failed", err.Error())
		return
	}
	result := WaitResult{ID: args.ID, State: outcome.State, TimedOut: outcome.TimedOut}
	if outcome.Reply != nil {
		view := messageView(*outcome.Reply, h.agentNames(ctx, me.SessionID))
		result.Reply = &view
	}
	h.replyRPC(ctx, cn, rpc.ID, result)
}

func (h hub) handleAck(ctx context.Context, me session.JoinResult, payload json.RawMessage) {
	var ack Ack
	if err := json.Unmarshal(payload, &ack); err != nil {
		return
	}
	_ = h.session.Acknowledge(ctx, me.SessionID, me.AgentID, ack.ID)
}

// handleApprove releases a held message addressed to the caller (ID empty
// picks the oldest), then flushes it - and anything else already queued for
// the caller - live if the caller is connected right now (it always is,
// since this RPC arrived on that very connection, but the short-lived
// admin connection relay approve uses for a global session is exactly the
// same kind of connection, so no special-casing is needed).
func (h hub) handleApprove(ctx context.Context, cn *conn, me session.JoinResult, rpc RPC) {
	var args ApproveArgs
	if err := json.Unmarshal(rpc.Args, &args); err != nil {
		h.replyError(ctx, cn, rpc.ID, "bad_request", "malformed approve args")
		return
	}
	msg, err := h.session.Approve(ctx, me.SessionID, me.AgentID, args.ID)
	if err != nil {
		h.replyError(ctx, cn, rpc.ID, "approve_failed", err.Error())
		return
	}
	h.replyRPC(ctx, cn, rpc.ID, ApproveResult{ID: msg.ID, State: msg.State})
	h.pushNotice(ctx, me.SessionID, me.AgentID)
	h.flushPending(ctx, me.SessionID, me.AgentID)
}

// handleReject permanently refuses a held message (ID empty picks the
// oldest) and notifies its sender - notifySender's own message isn't
// live-pushed here even if the sender happens to be online right now; it's
// delivered the same way any other sweep-generated notify is, on that
// agent's next connect, send, or approve - a deliberate, documented
// simplification, not an oversight.
func (h hub) handleReject(ctx context.Context, cn *conn, me session.JoinResult, rpc RPC) {
	var args ApproveArgs
	if err := json.Unmarshal(rpc.Args, &args); err != nil {
		h.replyError(ctx, cn, rpc.ID, "bad_request", "malformed reject args")
		return
	}
	msg, err := h.session.Reject(ctx, me.SessionID, me.AgentID, args.ID)
	if err != nil {
		h.replyError(ctx, cn, rpc.ID, "reject_failed", err.Error())
		return
	}
	h.replyRPC(ctx, cn, rpc.ID, ApproveResult{ID: msg.ID, State: msg.State})
	h.pushNotice(ctx, me.SessionID, me.AgentID)
}

func (h hub) handleListHeld(ctx context.Context, cn *conn, me session.JoinResult, rpc RPC) {
	held, err := h.session.ListHeld(ctx, me.SessionID, me.AgentID)
	if err != nil {
		h.replyError(ctx, cn, rpc.ID, "list_held_failed", err.Error())
		return
	}
	names := h.agentNames(ctx, me.SessionID)
	views := make([]MessageView, len(held))
	for i, m := range held {
		views[i] = messageView(m, names)
	}
	h.replyRPC(ctx, cn, rpc.ID, ListHeldResult{Messages: views})
}

// handleMsgState records what the caller's own local scheduler did with a
// message it received (injected/acknowledged/done) - an out-of-order or
// duplicate report is a safe no-op, enforced by the same transition table
// SetState already applies. Mirrors the local daemon's own msg_state
// handler, including its empty {} reply.
func (h hub) handleMsgState(ctx context.Context, cn *conn, me session.JoinResult, rpc RPC) {
	var args MsgStateArgs
	if err := json.Unmarshal(rpc.Args, &args); err != nil {
		h.replyError(ctx, cn, rpc.ID, "bad_request", "malformed msg_state args")
		return
	}
	if err := h.session.ReportState(ctx, me.SessionID, me.AgentID, args.ID, args.State); err != nil {
		h.replyError(ctx, cn, rpc.ID, "msg_state_failed", err.Error())
		return
	}
	h.replyRPC(ctx, cn, rpc.ID, struct{}{})
}

// handleAgentState records the caller's own live tool state, purely
// in-memory (see liveStates) - what relay_list_agents' State field reads.
// Mirrors the local daemon's own agent_state handler, including its empty
// {} reply.
func (h hub) handleAgentState(ctx context.Context, cn *conn, me session.JoinResult, rpc RPC) {
	var args AgentStateArgs
	if err := json.Unmarshal(rpc.Args, &args); err != nil {
		h.replyError(ctx, cn, rpc.ID, "bad_request", "malformed agent_state args")
		return
	}
	h.live.set(me.AgentID, args.State)
	h.replyRPC(ctx, cn, rpc.ID, struct{}{})
}

func (h hub) replyRPC(ctx context.Context, cn *conn, id string, result any) {
	b, err := json.Marshal(result)
	if err != nil {
		h.replyError(ctx, cn, id, "internal", err.Error())
		return
	}
	_ = cn.writeTyped(ctx, TypeRPCResult, RPCResult{ID: id, OK: true, Result: b})
}

func (h hub) replyError(ctx context.Context, cn *conn, id, code, message string) {
	_ = cn.writeTyped(ctx, TypeRPCResult, RPCResult{ID: id, OK: false, Error: &Error{Code: code, Message: message}})
}

// pingLoop is the server-initiated liveness check: if a ping goes
// unanswered within pongTimeout, the connection is torn down.
func (h hub) pingLoop(ctx context.Context, cn *conn) {
	ticker := time.NewTicker(pingEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pctx, cancel := context.WithTimeout(ctx, pongTimeout)
			err := cn.ws.Ping(pctx)
			cancel()
			if err != nil {
				cn.ws.CloseNow()
				return
			}
		}
	}
}

// messageView converts a stored message into its wire shape, resolving
// agent ids to display names via a session-wide id->name map (see
// agentNames) - built once per RPC, not per message.
func messageView(m mongodb.Message, names map[string]string) MessageView {
	return MessageView{
		ID: m.ID, From: names[m.FromAgentID], FromID: m.FromAgentID,
		To: names[m.ToAgentID], ToID: m.ToAgentID,
		Kind: m.Kind, Priority: m.Priority, Thread: m.Thread, ReplyTo: m.ReplyTo, Body: m.Body,
		Hops: m.Hops, State: m.State, Detail: m.Detail, CreatedAt: m.CreatedAt,
	}
}

func readEnvelope(ctx context.Context, ws *websocket.Conn) (Envelope, error) {
	_, data, err := ws.Read(ctx)
	if err != nil {
		return Envelope{}, err
	}
	var env Envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return Envelope{}, err
	}
	return env, nil
}

// reject sends a structured error frame, then closes with PolicyViolation -
// the same two-step convention (readable error before the close frame) the
// local daemon already uses.
func reject(ctx context.Context, cn *conn, code, message string) {
	_ = cn.writeTyped(ctx, TypeError, Error{Code: code, Message: message})
	cn.ws.Close(websocket.StatusPolicyViolation, code)
}

// joinErrorCode maps a Join failure to a wire error code, mirroring the
// local daemon's proto.Code* constants so the CLI's existing friendly()
// error messages work identically for global and local sessions.
func joinErrorCode(err error) string {
	switch {
	case errors.Is(err, session.ErrAgentLive):
		return "agent_live"
	case errors.Is(err, session.ErrBadToken):
		return "bad_token"
	case errors.Is(err, session.ErrSessionNotFound):
		return "session_not_found"
	case errors.Is(err, session.ErrNameTaken):
		return "name_taken"
	default:
		return "join_failed"
	}
}

// parsePriority mirrors the local daemon's own string aliases for P0-P3 -
// stored from Phase 1 onward but not yet enforced (see the project plan's
// Phase 2 table).
func parsePriority(s string) int {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "p0", "0", "interrupt", "urgent":
		return 0
	case "p1", "1", "high":
		return 1
	case "p3", "3", "low", "fyi":
		return 3
	default:
		return 2
	}
}
