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
	ID        string         `bson:"_id"`
	SessionID string         `bson:"session_id"`
	Name      string         `bson:"name"`
	Tool      string         `bson:"tool"`
	Role      string         `bson:"role"`
	Status    string         `bson:"status"` // "connected" | "disconnected"
	TokenHash string         `bson:"token_hash"`
	Metadata  map[string]any `bson:"metadata"`
	CreatedAt time.Time      `bson:"created_at"`
	UpdatedAt time.Time      `bson:"updated_at"`
}

// Message delivery states - Phase 1's forward-only, 3-state model (queued ->
// dispatched -> acknowledged). This is intentionally smaller than the local
// daemon's 9-state machine (held/rejected/expired/undeliverable and the
// injected state are all deferred, see the project plan's Phase 2 table),
// but it is at-least-once and idempotent: re-asserting the current state is
// always safe, and PendingFor replays anything still short of acknowledged
// on every reconnect.
const (
	MessageStateQueued       = "queued"
	MessageStateDispatched   = "dispatched"
	MessageStateAcknowledged = "acknowledged"
)

// DefaultMessageKind is used when a sender doesn't specify one - Phase 1
// stores Kind/Priority/Hops but doesn't yet enforce anything based on them
// (see the project plan's Phase 2 table for what reads these fields next).
const (
	DefaultMessageKind     = "task"
	DefaultMessagePriority = 2 // mirrors the local daemon's P2 "normal"
)

// Message is one inter-agent message.
type Message struct {
	ID          string         `bson:"_id"`
	SessionID   string         `bson:"session_id"`
	FromAgentID string         `bson:"from_agent_id"`
	ToAgentID   string         `bson:"to_agent_id"`
	Kind        string         `bson:"kind"`
	Priority    int            `bson:"priority"`
	ReplyTo     string         `bson:"reply_to,omitempty"`
	Body        string         `bson:"body"`
	Hops        int            `bson:"hops"`
	State       string         `bson:"state"`
	Metadata    map[string]any `bson:"metadata"`
	CreatedAt   time.Time      `bson:"created_at"`
	UpdatedAt   time.Time      `bson:"updated_at"`
}
