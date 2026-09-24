package globallink

import (
	"encoding/json"
	"time"
)

// This file mirrors server/package/ws/protocol.go's wire shape field-for-
// field. It is a deliberate duplication, not an import: server/ is a
// separate Go module the root module can never depend on, and vice versa.
// Keep the two in sync by hand if either changes.

// version matches server/package/ws.Version.
const version = 1

const (
	typeHello     = "hello"
	typeWelcome   = "welcome"
	typeRPC       = "rpc"
	typeRPCResult = "rpc_result"
	typeDeliver   = "deliver"
	typeAck       = "ack"
	typeError     = "error"
)

const (
	opSend       = "send"
	opListAgents = "list_agents"
	opWait       = "wait"
	opContext    = "get_context"
)

type envelope struct {
	V       int             `json:"v"`
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

func marshalEnvelope(typ string, payload any) ([]byte, error) {
	b, err := json.Marshal(payload)
	if err != nil {
		return nil, err
	}
	return json.Marshal(envelope{V: version, Type: typ, Payload: b})
}

type helloFrame struct {
	Session string `json:"session"`
	Name    string `json:"name"`
	Token   string `json:"token,omitempty"`
	Tool    string `json:"tool"`
	Role    string `json:"role"`
}

type welcomeFrame struct {
	SessionID string `json:"session_id"`
	AgentID   string `json:"agent_id"`
	Name      string `json:"name"`
	Token     string `json:"token,omitempty"`
	Resumed   bool   `json:"resumed"`
}

type rpcFrame struct {
	ID   string          `json:"id"`
	Op   string          `json:"op"`
	Args json.RawMessage `json:"args,omitempty"`
}

type rpcResultFrame struct {
	ID     string          `json:"id"`
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *wireError      `json:"error,omitempty"`
}

type messageView struct {
	ID        string    `json:"id"`
	From      string    `json:"from"`
	FromID    string    `json:"from_id"`
	To        string    `json:"to"`
	ToID      string    `json:"to_id"`
	Kind      string    `json:"kind"`
	Priority  int       `json:"priority"`
	ReplyTo   string    `json:"reply_to,omitempty"`
	Body      string    `json:"body"`
	Hops      int       `json:"hops,omitempty"`
	State     string    `json:"state,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

type deliverFrame struct {
	Message messageView `json:"message"`
}

type ackFrame struct {
	ID string `json:"id"`
}

type sendArgs struct {
	To       string `json:"to"`
	Body     string `json:"body"`
	Kind     string `json:"kind,omitempty"`
	Priority string `json:"priority,omitempty"`
	ReplyTo  string `json:"reply_to,omitempty"`
}

type sendResult struct {
	ID       string `json:"id"`
	State    string `json:"state"`
	Kind     string `json:"kind"`
	Priority int    `json:"priority"`
}

type listAgentsResult struct {
	Agents []agentInfo `json:"agents"`
}

type agentInfo struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Tool   string `json:"tool"`
	Role   string `json:"role"`
	Status string `json:"status"`
}

type waitArgs struct {
	ID       string  `json:"id"`
	TimeoutS float64 `json:"timeout_s"`
}

type waitResult struct {
	ID       string       `json:"id"`
	State    string       `json:"state"`
	Reply    *messageView `json:"reply,omitempty"`
	TimedOut bool         `json:"timed_out"`
}

type contextArgs struct {
	Agent string `json:"agent"`
	N     int    `json:"n,omitempty"`
}

type getContextResult struct {
	Agent    string        `json:"agent"`
	Messages []messageView `json:"messages"`
}

type wireError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}
