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

// Message is one inter-agent message. This is the bare v1 shape - a single
// Delivered flag, not the local daemon's full held/queued/dispatched/...
// state machine (see the plan's explicitly-deferred follow-up scope).
type Message struct {
	ID          string         `bson:"_id"`
	SessionID   string         `bson:"session_id"`
	FromAgentID string         `bson:"from_agent_id"`
	ToAgentID   string         `bson:"to_agent_id"`
	Body        string         `bson:"body"`
	Delivered   bool           `bson:"delivered"`
	Metadata    map[string]any `bson:"metadata"`
	CreatedAt   time.Time      `bson:"created_at"`
	UpdatedAt   time.Time      `bson:"updated_at"`
}
