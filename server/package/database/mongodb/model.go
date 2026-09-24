package mongodb

import "time"

// Collection names, shared by every repository so they never drift apart.
const (
	CollectionSessions = "sessions"
	CollectionAgents   = "agents"
	CollectionMessages = "messages"
)

// Every document in every collection carries the same three bookkeeping
// fields as the Postgres tables: a non-nil metadata map (defaulting to an
// empty object, never nil/absent) and created/updated timestamps.

// Session is one global (multi-machine) session's document.
type Session struct {
	ID        string         `bson:"_id"`
	Name      string         `bson:"name,omitempty"`
	Status    string         `bson:"status"` // "active" | "ended"
	Metadata  map[string]any `bson:"metadata"`
	CreatedAt time.Time      `bson:"created_at"`
	UpdatedAt time.Time      `bson:"updated_at"`
}

// Agent is one agent's document within a session.
type Agent struct {
	ID             string         `bson:"_id"`
	SessionID      string         `bson:"session_id"`
	Name           string         `bson:"name"`
	Tool           string         `bson:"tool"`
	Role           string         `bson:"role"`
	Status         string         `bson:"status"` // "connected" | "disconnected" | "exited"
	TokenHash      string         `bson:"token_hash"`
	ApproveInbound bool           `bson:"approve_inbound"`
	CanInterrupt   bool           `bson:"can_interrupt"`
	CanBroadcast   bool           `bson:"can_broadcast"`
	Metadata       map[string]any `bson:"metadata"`
	CreatedAt      time.Time      `bson:"created_at"`
	UpdatedAt      time.Time      `bson:"updated_at"`
}

// Message delivery states - the same forward-only, branching state machine
// the local daemon uses (see repository.nextStates for the transition
// table). held is the only state a message can start in (--approve-inbound,
// or a reply chain past the hop limit); everything else starts queued.
// rejected/expired/undeliverable are terminal - a human said no, the TTL ran
// out, or the recipient is gone for good.
const (
	MessageStateHeld          = "held"
	MessageStateQueued        = "queued"
	MessageStateDispatched    = "dispatched"
	MessageStateInjected      = "injected"
	MessageStateAcknowledged  = "acknowledged"
	MessageStateDone          = "done"
	MessageStateRejected      = "rejected"
	MessageStateExpired       = "expired"
	MessageStateUndeliverable = "undeliverable"
)

// DefaultMessageKind/DefaultMessagePriority are used when a sender doesn't
// specify one - "task"/P2 "normal", mirroring the local daemon exactly.
const (
	DefaultMessageKind     = "task"
	DefaultMessagePriority = 2
)

// Message is one inter-agent message.
type Message struct {
	ID          string         `bson:"_id"`
	SessionID   string         `bson:"session_id"`
	FromAgentID string         `bson:"from_agent_id"`
	ToAgentID   string         `bson:"to_agent_id"`
	Kind        string         `bson:"kind"`
	Priority    int            `bson:"priority"`
	Thread      string         `bson:"thread"` // constant across a whole reply chain; a root message's own id
	ReplyTo     string         `bson:"reply_to,omitempty"`
	Body        string         `bson:"body"`
	Hops        int            `bson:"hops"`
	State       string         `bson:"state"`
	Detail      string         `bson:"detail,omitempty"` // human-readable "why held" / "why undeliverable" etc.
	ExpiresAt   time.Time      `bson:"expires_at"`       // CreatedAt + config.Config.MessageTTL
	Metadata    map[string]any `bson:"metadata"`
	CreatedAt   time.Time      `bson:"created_at"`
	UpdatedAt   time.Time      `bson:"updated_at"`
}
