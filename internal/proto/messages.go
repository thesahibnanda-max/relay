package proto

import (
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

// MaxBodyBytes bounds one message body.
const MaxBodyBytes = 32 << 10

// RPC operations (agent -> daemon).
const (
	OpSend       = "send"        // SendArgs -> SendResult
	OpListAgents = "list_agents" // -> ListAgentsResult
	OpApprove    = "approve"     // ApproveArgs -> ApproveResult (release a held message)
	OpReject     = "reject"      // ApproveArgs -> ApproveResult
	OpWait       = "wait"        // WaitArgs -> WaitResult
	OpMsgState   = "msg_state"   // MsgStateArgs -> {} (agent reports what it did with a message)
	OpAgentState = "agent_state" // AgentStateArgs -> {} (agent reports what its tool is doing)
	OpContext    = "context"     // ContextArgs -> ContextResult (read another agent's conversation)
)

// RPC is an agent's request. The daemon answers with a Result carrying the same ID.
type RPC struct {
	ID   string          `json:"id"`
	Op   string          `json:"op"`
	Args json.RawMessage `json:"args,omitempty"`
}

type Result struct {
	ID     string          `json:"id"`
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *Error          `json:"error,omitempty"`
}

// Message kinds a caller may send.
var Kinds = []string{"task", "question", "answer", "notify"}

// ValidKind reports whether k is a kind agents may send ("" means default).
func ValidKind(k string) bool {
	for _, v := range Kinds {
		if k == v {
			return true
		}
	}
	return false
}

// ParsePriority accepts p0..p3 or interrupt|high|normal|low ("" = normal).
func ParsePriority(s string) (int, error) {
	switch strings.ToLower(strings.TrimSpace(s)) {
	case "p0", "0", "interrupt", "urgent":
		return 0, nil
	case "p1", "1", "high":
		return 1, nil
	case "", "p2", "2", "normal", "default":
		return 2, nil
	case "p3", "3", "low", "fyi":
		return 3, nil
	}
	return 0, fmt.Errorf("priority %q: use interrupt (p0), high (p1), normal (p2) or low (p3)", s)
}

// PriorityName is the human name of a priority level.
func PriorityName(p int) string {
	switch p {
	case 0:
		return "interrupt"
	case 1:
		return "high"
	case 2:
		return "normal"
	case 3:
		return "low"
	}
	return fmt.Sprint(p)
}

type SendArgs struct {
	To       string `json:"to"` // exact agent name, "role:<role>" (if unique), or "all"
	Body     string `json:"body"`
	Kind     string `json:"kind,omitempty"`     // default task; replies default to answer
	Priority string `json:"priority,omitempty"` // default normal
	ReplyTo  string `json:"reply_to,omitempty"` // message this answers
}

type SendResult struct {
	ID       string   `json:"msg_id"`
	IDs      []string `json:"msg_ids,omitempty"` // for "all": one per recipient
	To       []string `json:"to"`
	Kind     string   `json:"kind"`
	Priority string   `json:"priority"`
	State    string   `json:"state"`
	Note     string   `json:"note,omitempty"` // downgrades, holds, duplicates
}

// PeerInfo describes an agent in the caller's session.
type PeerInfo struct {
	Name       string    `json:"name"`
	Role       string    `json:"role"`
	Tool       string    `json:"tool"`
	Status     string    `json:"status"`          // connected | disconnected | exited
	State      string    `json:"state,omitempty"` // what its tool is doing: idle | busy | dialog | starting | unknown
	LastActive time.Time `json:"last_active"`
	Self       bool      `json:"self,omitempty"`
	// Remote/Peer describe an agent gossiped in from another daemon; both
	// are zero for a local agent. Mirrors AgentInfo's fields of the same name.
	Remote bool   `json:"remote,omitempty"`
	Peer   string `json:"peer,omitempty"`
}

type ListAgentsResult struct {
	Session string     `json:"session"`
	Agents  []PeerInfo `json:"agents"`
}

type ApproveArgs struct {
	ID string `json:"id,omitempty"` // empty: the oldest message held for the caller
}

type ApproveResult struct {
	ID    string `json:"id"`
	State string `json:"state"`
}

type WaitArgs struct {
	ID       string  `json:"id"`
	TimeoutS float64 `json:"timeout_s"`
}

type WaitResult struct {
	ID       string       `json:"msg_id"`
	State    string       `json:"state"`
	Detail   string       `json:"detail,omitempty"`
	Reply    *MessageView `json:"reply,omitempty"`
	TimedOut bool         `json:"timed_out,omitempty"`
}

type MsgStateArgs struct {
	ID    string `json:"id"`
	State string `json:"state"` // injected | acknowledged | done
}

type AgentStateArgs struct {
	State    string `json:"state"`
	PlanMode bool   `json:"plan_mode,omitempty"`
}

// MessageView is a message as agents and the CLI see it.
type MessageView struct {
	ID        string    `json:"id"`
	Session   string    `json:"session,omitempty"`
	From      string    `json:"from"`
	FromRole  string    `json:"from_role,omitempty"`
	To        string    `json:"to"`
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

// Deliver is the payload of a TypeDeliver frame.
type Deliver struct {
	Message MessageView `json:"message"`
}

// ---- admin API ---------------------------------------------------------------

// AdminSend is a message from the human user (`relay send`).
type AdminSend struct {
	Session  string `json:"session,omitempty"` // optional if exactly one shared session is active
	To       string `json:"to"`
	Body     string `json:"body"`
	Kind     string `json:"kind,omitempty"`
	Priority string `json:"priority,omitempty"`
}

// Notice is the payload of a TypeNotice frame.
type Notice struct {
	Held int `json:"held"` // messages currently held for this agent, waiting for a human to approve
}

// EventTurn is the Event.Type of a conversation turn read from the tool's
// transcript (Meta carries the JSON of a TurnView without Seq).
const EventTurn = "turn"

// Context modes.
const (
	ContextTail       = "tail"        // the last N turns
	ContextLastAnswer = "last_answer" // the assistant's latest reply
	ContextSince      = "since"       // turns since a duration ("10m") or RFC 3339 time
	ContextSearch     = "search"      // turns containing Query
)

type ContextArgs struct {
	Agent string `json:"agent"`
	Mode  string `json:"mode,omitempty"` // default tail
	N     int    `json:"n,omitempty"`    // default 10, max 50
	Query string `json:"query,omitempty"`
	Since string `json:"since,omitempty"`
}

// TurnView is one turn of an agent's conversation.
type TurnView struct {
	Seq  uint64    `json:"seq,omitempty"`
	ID   string    `json:"id,omitempty"`
	TS   time.Time `json:"ts"`
	Role string    `json:"role"` // user | assistant | tool_call | tool_result | system
	Text string    `json:"text"`
	Tool string    `json:"tool,omitempty"`
}

type ContextResult struct {
	Agent     string     `json:"agent"`
	Mode      string     `json:"mode"`
	Turns     []TurnView `json:"turns"`
	Truncated bool       `json:"truncated,omitempty"` // older turns were left out to fit the size cap
	Note      string     `json:"note,omitempty"`
}

// MaxContextBytes bounds the text returned by one context request.
const MaxContextBytes = 24 << 10
