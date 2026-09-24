// Package ws is the WebSocket-facing layer: this module's own wire protocol
// (deliberately not shared with internal/proto - different module, and
// Go's internal/ visibility rule wouldn't allow the import anyway), the
// connection hub, and the HTTP/CORS wiring that accepts connections.
package ws

import "encoding/json"

// Version is this protocol's own version number - independent of the local
// daemon's proto.Version, since this is a different wire format entirely.
const Version = 1

// Envelope frame types - one JSON envelope per WebSocket text frame,
// mirroring the shape (not the code) the local daemon already uses.
const (
	TypeHello      = "hello"
	TypeWelcome    = "welcome"
	TypeSend       = "send"
	TypeSendResult = "send_result"
	TypeListAgents = "list_agents"
	TypeAgentsList = "agents_list"
	TypeDeliver    = "deliver"
	TypeError      = "error"
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
}

// Welcome is the reply to a successful Hello.
type Welcome struct {
	SessionID string `json:"session_id"`
	AgentID   string `json:"agent_id"`
	Name      string `json:"name"`
	Token     string `json:"token,omitempty"` // set only on first registration
	Resumed   bool   `json:"resumed"`
}

// Send asks the server to hand Body to the agent named To in the same session.
type Send struct {
	To   string `json:"to"`
	Body string `json:"body"`
}

// SendResult is Send's synchronous reply.
type SendResult struct {
	ID string `json:"id"`
}

// ListAgentsResult answers a (payload-less) ListAgents request.
type ListAgentsResult struct {
	Agents []AgentInfo `json:"agents"`
}

// AgentInfo is one entry in a ListAgentsResult.
type AgentInfo struct {
	ID     string `json:"id"`
	Name   string `json:"name"`
	Tool   string `json:"tool"`
	Role   string `json:"role"`
	Status string `json:"status"`
}

// Deliver is pushed to a connected agent when another agent sends it a message.
type Deliver struct {
	ID   string `json:"id"`
	From string `json:"from"`
	Body string `json:"body"`
}

// Error is sent before closing a connection that violated the protocol or
// whose request failed.
type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}
