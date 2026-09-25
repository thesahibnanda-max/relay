// Package ws is the WebSocket-facing layer: this module's own wire protocol
// (deliberately not shared with internal/proto - different module, and
// Go's internal/ visibility rule wouldn't allow the import anyway), the
// connection hub, and the HTTP/CORS wiring that accepts connections.
package ws

import (
	"encoding/json"
	"time"
)

// Version is this protocol's own version number - independent of the local
// daemon's proto.Version, since this is a different wire format entirely.
const Version = 1

// Envelope frame types - one JSON envelope per WebSocket text frame,
// mirroring the shape (not the code) the local daemon already uses. rpc/
// rpc_result is a single generic request/reply pair covering every RPC
// operation (see the Op constants below) - deliberately consolidated from
// four bespoke pairs so a Phase 2 operation (approve/reject/...) can be
// added later as just a new Op string, with zero new frame types.
const (
	TypeHello     = "hello"
	TypeWelcome   = "welcome"
	TypeRPC       = "rpc"
	TypeRPCResult = "rpc_result"
	TypeDeliver   = "deliver"
	TypeAck       = "ack"    // client -> server: confirms a deliver was handled
	TypeNotice    = "notice" // server -> client: this agent's held-message count changed
	TypeError     = "error"
)

// RPC operation names (the Op field of RPC/RPCResult).
const (
	OpSend       = "send"
	OpListAgents = "list_agents"
	OpWait       = "wait"
	OpContext    = "get_context"
	OpApprove    = "approve"     // release a held message (--approve-inbound / hop-limit)
	OpReject     = "reject"      // permanently refuse a held message
	OpListHeld   = "list_held"   // list messages held for the caller - no id param, always self-scoped
	OpMsgState   = "msg_state"   // report injected/acknowledged/done for a message
	OpAgentState = "agent_state" // report the caller's own live tool state (idle/busy/dialog/...)
)

// Envelope is the one shape every frame takes.
type Envelope struct {
	V       int             `json:"v"`
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

func marshal(typ string, payload any) (Envelope, error) {
	b, err := json.Marshal(payload)
	if err != nil {
		return Envelope{}, err
	}
	return Envelope{V: Version, Type: typ, Payload: b}, nil
}

// Hello is the first frame a connecting agent must send.
type Hello struct {
	Session string `json:"session"` // "", "NEW", or an existing session's ULID
	Name    string `json:"name"`
	Token   string `json:"token,omitempty"` // non-empty: resume this exact agent
	Tool    string `json:"tool"`
	Role    string `json:"role"`
	// Role policy, refreshed on every join/resume in case the role changed
	// between runs - mirrors the local daemon's own Hello fields exactly.
	ApproveInbound bool `json:"approve_inbound,omitempty"`
	CanInterrupt   bool `json:"can_interrupt,omitempty"`
	CanBroadcast   bool `json:"can_broadcast,omitempty"`
}

// Welcome is the reply to a successful Hello.
type Welcome struct {
	SessionID string `json:"session_id"`
	AgentID   string `json:"agent_id"`
	Name      string `json:"name"`
	Token     string `json:"token,omitempty"` // set only on first registration
	Resumed   bool   `json:"resumed"`
}

// RPC is a request frame; ID is chosen by the caller and echoed back
// verbatim in the matching RPCResult so a client with more than one
// request in flight (e.g. a slow "wait" alongside a "list_agents") can
// route replies correctly.
type RPC struct {
	ID   string          `json:"id"`
	Op   string          `json:"op"`
	Args json.RawMessage `json:"args,omitempty"`
}

// RPCResult is an RPC's reply.
type RPCResult struct {
	ID     string          `json:"id"`
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *Error          `json:"error,omitempty"`
}

// MessageView is the shape a message takes over the wire, in a deliver push
// or a wait/get_context result. It carries Kind/Priority/ReplyTo/Hops/State
// even though Phase 1 doesn't enforce any of them yet - see the project
// plan's Phase 2 table for what reads these fields next. From/To are
// display names (not ids), matching the local daemon's own Deliver
// convention.
type MessageView struct {
	ID        string    `json:"id"`
	From      string    `json:"from"`
	FromID    string    `json:"from_id"`
	To        string    `json:"to"`
	ToID      string    `json:"to_id"`
	Kind      string    `json:"kind"`
	Priority  int       `json:"priority"`
	Thread    string    `json:"thread,omitempty"`
	ReplyTo   string    `json:"reply_to,omitempty"`
	Body      string    `json:"body"`
	Hops      int       `json:"hops,omitempty"`
	State     string    `json:"state,omitempty"`
	Detail    string    `json:"detail,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

// Deliver is pushed to a connected agent, either live (another agent just
// sent it something) or replayed at connect/resume time (it was sent while
// this agent was offline).
type Deliver struct {
	Message MessageView `json:"message"`
}

// Ack is sent by the client back to the server once a Deliver has been
// handled, advancing the message to mongodb.MessageStateAcknowledged.
type Ack struct {
	ID string `json:"id"`
}

// Notice is pushed to an agent whenever its held-message count changes - a
// new message becomes held for it, or one of its held messages leaves held.
type Notice struct {
	Held int `json:"held"`
}

// SendArgs is the OpSend request payload.
type SendArgs struct {
	To       string `json:"to"`
	Body     string `json:"body"`
	Kind     string `json:"kind,omitempty"`
	Priority string `json:"priority,omitempty"` // parsed/defaulted server-side; unenforced in Phase 1
	ReplyTo  string `json:"reply_to,omitempty"`
}

// SendResult is OpSend's reply. Kind/Priority echo back the resolved
// (post-default) values so a caller can tell what was actually stored, not
// just what it asked for - found missing during the first live two-terminal
// verification (the model correctly noticed an empty kind/priority where it
// expected task/normal defaults). Note explains a non-obvious outcome, such
// as "duplicate of a recent identical message".
type SendResult struct {
	ID       string `json:"id"`
	State    string `json:"state"`
	Kind     string `json:"kind"`
	Priority int    `json:"priority"`
	Note     string `json:"note,omitempty"`
}

// ListAgentsResult is OpListAgents' reply (the request payload is empty).
type ListAgentsResult struct {
	Agents []AgentInfo `json:"agents"`
}

// AgentInfo is one entry in a ListAgentsResult. Status is the persisted
// connection status (connected|disconnected|exited); State is the live,
// never-persisted tool state (idle|busy|dialog|...), "unknown" if the agent
// has never reported one (including right after a server restart, since
// this is intentionally not durable). LastActive is the agent document's own
// UpdatedAt (join/resume, a status change, or being reaped) - not refined by
// any more-recent message activity the way the local daemon's equivalent is.
type AgentInfo struct {
	ID         string    `json:"id"`
	Name       string    `json:"name"`
	Tool       string    `json:"tool"`
	Role       string    `json:"role"`
	Status     string    `json:"status"`
	State      string    `json:"state,omitempty"`
	LastActive time.Time `json:"last_active"`
}

// ApproveArgs is the OpApprove/OpReject request payload. ID empty picks the
// oldest held message addressed to the caller.
type ApproveArgs struct {
	ID string `json:"id,omitempty"`
}

// ApproveResult is OpApprove/OpReject's reply.
type ApproveResult struct {
	ID    string `json:"id"`
	State string `json:"state"`
}

// ListHeldResult is OpListHeld's reply (the request payload is empty) -
// always scoped to messages held for the calling agent.
type ListHeldResult struct {
	Messages []MessageView `json:"messages"`
}

// MsgStateArgs is the OpMsgState request payload, reporting the caller's
// progress handling a message it received (injected/acknowledged/done).
type MsgStateArgs struct {
	ID    string `json:"id"`
	State string `json:"state"`
}

// AgentStateArgs is the OpAgentState request payload, reporting the
// caller's own live tool state - never persisted, see AgentInfo.State.
type AgentStateArgs struct {
	State    string `json:"state"`
	PlanMode bool   `json:"plan_mode,omitempty"`
}

// WaitArgs is the OpWait request payload.
type WaitArgs struct {
	ID       string  `json:"id"`
	TimeoutS float64 `json:"timeout_s"`
}

// WaitResult is OpWait's reply.
type WaitResult struct {
	ID       string       `json:"id"`
	State    string       `json:"state"`
	Reply    *MessageView `json:"reply,omitempty"`
	TimedOut bool         `json:"timed_out"`
}

// ContextArgs is the OpContext request payload. Unlike the local daemon's
// richer Mode/Query/Since options, Phase 1's get_context is always "recent
// message history involving this agent" - the server never receives raw
// terminal transcripts to search over.
type ContextArgs struct {
	Agent string `json:"agent"`
	N     int    `json:"n,omitempty"`
}

// GetContextResult is OpContext's reply.
type GetContextResult struct {
	Agent    string        `json:"agent"`
	Messages []MessageView `json:"messages"`
}

// Error is sent before closing a connection that violated the protocol, or
// as an RPCResult's Error field when an individual RPC fails.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}
