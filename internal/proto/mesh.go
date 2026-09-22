package proto

import (
	"encoding/json"
	"fmt"
	"time"
)

// MeshVersion is the daemon-to-daemon mesh protocol version, deliberately
// separate from Version (the agent<->daemon protocol). No agent process
// ever sees a mesh frame, so a mismatch here refuses only the one peer link
// that disagrees, without affecting any local session, any agent, or any
// other peer link this daemon already has. A daemon that never mints or
// joins a --join blob never opens a mesh listener at all.
const MeshVersion = 1

// Mesh message types (MeshEnvelope.Type).
const (
	MeshTypeHello       = "mesh_hello"        // dialer -> acceptor, first frame on every new link
	MeshTypeWelcome     = "mesh_welcome"      // acceptor -> dialer, reply: a full resync
	MeshTypeError       = "mesh_error"        // either direction, fatal for this link
	MeshTypePeerList    = "mesh_peer_list"    // incremental: a peer this daemon learned about
	MeshTypeAgentRoster = "mesh_agent_roster" // incremental: one MeshAgentInfo changed
	MeshTypeMsgHandoff  = "mesh_msg"          // a message crossing from the sender's daemon to the recipient's
	MeshTypeMsgAck      = "mesh_msg_ack"      // transport-level: link-sequence acknowledgement (dedupe/resend)
	MeshTypeMsgReceipt  = "mesh_msg_receipt"  // application-level: the owning daemon's state for a message changed
	MeshTypePing        = "mesh_ping"
	MeshTypePong        = "mesh_pong"
)

// Mesh error codes, mirroring the naming of Code* above.
const (
	MeshCodeBadSecret       = "mesh_bad_secret"       // join secret didn't match mesh_sessions.join_secret
	MeshCodeVersionMismatch = "mesh_version_mismatch" // MeshVersion disagreement; refuses only this link
	MeshCodeUnknownSession  = "mesh_unknown_session"  // this daemon has no mesh_sessions row for the session
	MeshCodeKeyChanged      = "mesh_key_changed"      // TOFU pin violation: the peer's key differs from mesh_peers
)

// MeshEnvelope is the mesh link's wire frame, structurally identical to
// Envelope so both protocols are easy to reason about side by side, but
// its own type: an agent's Envelope{V: Version} and a mesh link's
// MeshEnvelope{V: MeshVersion} are never interchangeable, and a daemon that
// accidentally received one on the wrong socket would fail a struct decode
// (different field name) rather than silently misinterpreting a version
// number from the wrong space.
type MeshEnvelope struct {
	V       int             `json:"v"`
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

func MeshMarshal(typ string, payload any) ([]byte, error) {
	var raw json.RawMessage
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		raw = b
	}
	return json.Marshal(MeshEnvelope{V: MeshVersion, Type: typ, Payload: raw})
}

func MeshUnmarshal(data []byte) (MeshEnvelope, error) {
	var e MeshEnvelope
	if err := json.Unmarshal(data, &e); err != nil {
		return e, err
	}
	if e.Type == "" {
		return e, fmt.Errorf("mesh message without type")
	}
	return e, nil
}

// MeshHello is the first frame on every new mesh link (not just the first
// ever join: every reconnect re-presents Secret, since transport-level
// tailcat auth alone only proves "this is PeerID's private key," not
// "PeerID is still allowed into this session").
type MeshHello struct {
	MeshVersion int    `json:"mesh_version"`
	Session     string `json:"session"`
	Secret      string `json:"secret"`       // mesh_sessions.join_secret for Session
	PeerID      string `json:"peer_id"`      // hex(dialer's node public key) - see meshnet.Identity.PeerID
	Addr        string `json:"addr"`         // dialer's own redialable meshnet.Addr
	DaemonBuild string `json:"daemon_build"` // informational, like Hello.Client
}

// MeshWelcome answers a valid MeshHello with a full resync: everything the
// new link's peer needs to reach every *other* peer directly (the seed for
// one join is a one-time bootstrap aid, never an ongoing dependency - see
// the mesh plan's walkthrough (c)).
type MeshWelcome struct {
	PeerID string          `json:"peer_id"`
	Addr   string          `json:"addr"`
	Peers  []MeshPeerInfo  `json:"peers"`
	Agents []MeshAgentInfo `json:"agents"`
}

// MeshPeerInfo is one other daemon in a mesh session, as last observed by
// whichever daemon sent it. Never merged across daemons: each daemon's own
// mesh_peers table is a local cache of what *it* has seen, not a shared fact.
type MeshPeerInfo struct {
	PeerID   string    `json:"peer_id"`
	Addr     string    `json:"addr"`
	LastSeen time.Time `json:"last_seen"`
}

// MeshAgentInfo is one agent's gossiped roster entry, owned exclusively by
// OwnerPeer. A receiver applies it only if Version is greater than what it
// already has for AgentID - since exactly one daemon can ever produce a
// given (AgentID, Version) pair, "keep the higher version" is correct by
// construction, not an approximation: there is no concurrent write to this
// key to reconcile.
type MeshAgentInfo struct {
	AgentID      string    `json:"agent_id"`
	OwnerPeer    string    `json:"owner_peer"`
	Name         string    `json:"name"`
	Tool         string    `json:"tool"`
	Role         string    `json:"role"`
	Status       string    `json:"status"`
	CanInterrupt bool      `json:"can_interrupt,omitempty"`
	CanBroadcast bool      `json:"can_broadcast,omitempty"`
	LastSeenAt   time.Time `json:"last_seen_at"`
	Version      uint64    `json:"version"`
	Tombstoned   bool      `json:"tombstoned,omitempty"` // renamed/removed; kept so late gossip doesn't resurrect it
}

// MeshMsgHandoff carries one message across the daemon-to-daemon hop. ID is
// minted by the sender so both daemons' local rows (one authoritative, one
// mirror - see the mesh plan's "core insight") share a primary key.
// CreatedAt/ExpiresAt are the sender's absolute timestamps, never
// recomputed on receipt, so clock skew between two machines can't affect
// TTL behavior. Resend marks an at-least-once redelivery after a link
// reconnect, mirroring the daemon's existing single-machine flush(resend=true).
type MeshMsgHandoff struct {
	ID        string    `json:"id"`
	SessionID string    `json:"session_id"`
	FromAgent string    `json:"from_agent"`
	FromName  string    `json:"from_name"`
	FromRole  string    `json:"from_role"`
	FromPeer  string    `json:"from_peer"`
	ToAgent   string    `json:"to_agent"`
	ToName    string    `json:"to_name"`
	Kind      string    `json:"kind"`
	Priority  int       `json:"priority"`
	Hops      int       `json:"hops"`
	Thread    string    `json:"thread"`
	ReplyTo   string    `json:"reply_to,omitempty"`
	Body      string    `json:"body"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
	Resend    bool      `json:"resend,omitempty"`
}

// MeshMsgReceipt reports the owning daemon's current state for a message,
// asynchronously, back to the daemon holding the mirror row. Rev guards
// against applying a stale receipt that arrives after a newer one (link
// reconnects can redeliver frames out of their original order relative to
// each other): a receiver drops any receipt whose Rev is not greater than
// what it already has for ID.
type MeshMsgReceipt struct {
	ID     string `json:"id"`
	State  string `json:"state"`
	Detail string `json:"detail,omitempty"`
	Rev    uint64 `json:"rev"`
}
