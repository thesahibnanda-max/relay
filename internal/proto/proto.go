// Package proto defines the messages exchanged between a Relay agent and the
// daemon (WebSocket, JSON envelopes) and the daemon's admin API types.
package proto

import (
	"context"
	"encoding/json"
	"fmt"
	"net"
	"net/http"
	"os"
	"regexp"
	"time"

	"github.com/coder/websocket"

	"github.com/thesahibnanda-max/relay/internal/peercred"
)

// Version is the wire protocol version. Bump on incompatible changes; the
// daemon refuses agents that speak a different one.
const Version = 2

// Message types (Envelope.Type).
const (
	TypeHello   = "hello"   // agent -> daemon, first message
	TypeWelcome = "welcome" // daemon -> agent, reply to hello
	TypeEvents  = "events"  // agent -> daemon, batch of events
	TypeAck     = "ack"     // daemon -> agent, events durably stored up to Seq
	TypeBye     = "bye"     // agent -> daemon, tool exited
	TypeError   = "error"   // daemon -> agent, fatal
	TypeRPC     = "rpc"     // agent -> daemon, request expecting an rpc_result
	TypeResult  = "rpc_result"
	TypeDeliver = "deliver" // daemon -> agent, an inbound message for its scheduler
	TypeNotice  = "notice"  // daemon -> agent, out-of-band news for the human at that terminal
)

// Error codes.
const (
	CodeProtoMismatch   = "proto_mismatch"
	CodeSessionNotFound = "session_not_found"
	CodeSessionEnded    = "session_ended"
	CodeNameTaken       = "name_taken"
	CodeBadName         = "bad_name"
	CodeBadToken        = "bad_token"
	CodeAgentLive       = "agent_live"
	CodeBadRequest      = "bad_request"
	CodeInternal        = "internal"
	CodeSessionFull     = "session_full"

	// Messaging errors (returned inside rpc_result / admin API replies).
	CodeUnknownAgent   = "unknown_agent"
	CodeAmbiguousAgent = "ambiguous_agent"
	CodeSelfSend       = "self_send"
	CodeRateLimited    = "rate_limited"
	CodeForbidden      = "forbidden"
	CodeTargetGone     = "target_gone"
	CodeTooLarge       = "too_large"
	CodeBadPriority    = "bad_priority"
	CodeBadKind        = "bad_kind"
	CodeNotFound       = "not_found"
	CodeOffline        = "offline"
)

var (
	roleRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,31}$`)
	toolRe = regexp.MustCompile(`^[a-z0-9][a-z0-9_-]{0,31}$`)
)

// ValidRole reports whether s is an acceptable role name. Roles are shown in
// message headers and listings that other agents read, so they are restricted
// to a plain charset: a role must never be able to carry a newline or a forged header.
func ValidRole(s string) bool { return roleRe.MatchString(s) }

// ValidTool reports whether s is an acceptable tool name.
func ValidTool(s string) bool { return toolRe.MatchString(s) }

// SessionNew asks the daemon to create a shared session; "" means a private solo session.
const SessionNew = "NEW"

type Envelope struct {
	V       int             `json:"v"`
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload,omitempty"`
}

func Marshal(typ string, payload any) ([]byte, error) {
	var raw json.RawMessage
	if payload != nil {
		b, err := json.Marshal(payload)
		if err != nil {
			return nil, err
		}
		raw = b
	}
	return json.Marshal(Envelope{V: Version, Type: typ, Payload: raw})
}

func Unmarshal(data []byte) (Envelope, error) {
	var e Envelope
	if err := json.Unmarshal(data, &e); err != nil {
		return e, err
	}
	if e.Type == "" {
		return e, fmt.Errorf("message without type")
	}
	return e, nil
}

// Hello is the agent's first message. To join afresh set Session to
// SessionNew, a session ULID, or "" (solo). To resume a known agent also set
// Name and Token.
type Hello struct {
	Proto          int    `json:"proto"`
	Client         string `json:"client"` // relay build version, informational
	Session        string `json:"session"`
	Name           string `json:"name,omitempty"`
	Token          string `json:"token,omitempty"`
	Tool           string `json:"tool"`
	Role           string `json:"role"`
	RoleSource     string `json:"role_source,omitempty"`
	ApproveInbound bool   `json:"approve_inbound,omitempty"`
	PID            int    `json:"pid"`
	Cwd            string `json:"cwd,omitempty"`
	// Role policy, so the daemon can enforce it on this agent's messages.
	CanInterrupt bool `json:"can_interrupt,omitempty"`
	CanBroadcast bool `json:"can_broadcast,omitempty"`
}

type SessionRef struct {
	ID   string `json:"id"`
	Name string `json:"name,omitempty"`
	Kind string `json:"kind"`
}

type AgentRef struct {
	ID   string `json:"id"`
	Name string `json:"name"`
	Tool string `json:"tool"`
	Role string `json:"role"`
}

type Welcome struct {
	Session  SessionRef `json:"session"`
	Agent    AgentRef   `json:"agent"`
	Token    string     `json:"token,omitempty"` // only on first registration
	AckedSeq uint64     `json:"acked_seq"`       // resend everything after this
	Server   string     `json:"server"`
	Resumed  bool       `json:"resumed,omitempty"`
}

// Event is one thing that happened in the agent's terminal. Seq is assigned by
// the agent, dense and increasing, so the daemon can ack and de-duplicate.
type Event struct {
	Seq  uint64          `json:"seq"`
	T    time.Time       `json:"t"`
	Type string          `json:"type"`           // start|in|out|inject|resize|exit
	B    []byte          `json:"b,omitempty"`    // raw bytes for in/out/inject
	Meta json.RawMessage `json:"meta,omitempty"` // structured fields for the other types
}

// IsRaw reports whether the event carries raw terminal bytes (stored in
// segment files rather than SQLite).
func (e Event) IsRaw() bool { return e.Type == "in" || e.Type == "out" || e.Type == "inject" }

type Events struct {
	Events []Event `json:"events"`
}

type Ack struct {
	Seq uint64 `json:"seq"`
}

type Bye struct {
	ExitCode int `json:"exit_code"`
}

type Error struct {
	Code    string `json:"code"`
	Message string `json:"message"`
	// Agents lists the session's agents when the problem was an unknown or
	// ambiguous recipient, so the caller can correct itself.
	Agents []PeerInfo `json:"agents,omitempty"`
}

func (e *Error) Error() string { return e.Message }

// ---- admin API (HTTP over the same socket) --------------------------------

type Status struct {
	Version   string    `json:"version"`
	Proto     int       `json:"proto"`
	PID       int       `json:"pid"`
	StartedAt time.Time `json:"started_at"`
	Sessions  int       `json:"sessions"`
	Agents    int       `json:"agents_connected"`
}

type AgentInfo struct {
	ID             string    `json:"id"`
	Name           string    `json:"name"`
	Tool           string    `json:"tool"`
	Role           string    `json:"role"`
	Status         string    `json:"status"`
	Connected      bool      `json:"connected"`
	ApproveInbound bool      `json:"approve_inbound,omitempty"`
	PID            int       `json:"pid,omitempty"`
	Cwd            string    `json:"cwd,omitempty"`
	JoinedAt       time.Time `json:"joined_at"`
	LastSeen       time.Time `json:"last_seen"`
	ExitCode       *int      `json:"exit_code,omitempty"`
	// Remote/Peer describe an agent gossiped in from another daemon (see
	// MEMORY.md section 15's mesh work); both are zero for a local agent.
	Remote bool   `json:"remote,omitempty"`
	Peer   string `json:"peer,omitempty"` // owning daemon's PeerID
}

type SessionInfo struct {
	ID        string      `json:"id"`
	Name      string      `json:"name,omitempty"`
	Kind      string      `json:"kind"`
	Status    string      `json:"status"`
	CreatedAt time.Time   `json:"created_at"`
	Agents    []AgentInfo `json:"agents"`
}

type CreateSessionRequest struct {
	Name string `json:"name,omitempty"`
}

type APIError struct {
	Error string `json:"error"`
}

// ---- dialling the daemon over its unix socket ------------------------------

// dialOwn connects to the daemon socket and checks that the process on the
// other end runs as us. A socket path in a shared directory could have been
// squatted by another user, who would then receive everything we send.
func dialOwn(ctx context.Context, socket string) (net.Conn, error) {
	var d net.Dialer
	c, err := d.DialContext(ctx, "unix", socket)
	if err != nil {
		return nil, err
	}
	if uid, err := peercred.UID(c); err == nil && uid != uint32(os.Getuid()) {
		c.Close()
		return nil, fmt.Errorf("%s is served by another user (uid %d): refusing to talk to it", socket, uid)
	}
	return c, nil
}

// HTTPClient returns an http.Client that talks to the daemon's unix socket.
func HTTPClient(socket string) *http.Client {
	return &http.Client{
		Timeout: 10 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return dialOwn(ctx, socket)
			},
			DisableKeepAlives: true,
		},
	}
}

// DialAgent opens the agent WebSocket.
func DialAgent(ctx context.Context, socket string) (*websocket.Conn, error) {
	c, _, err := websocket.Dial(ctx, "http://relay/v1/agent", &websocket.DialOptions{
		HTTPClient: &http.Client{Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				return dialOwn(ctx, socket)
			},
		}},
	})
	if err != nil {
		return nil, err
	}
	c.SetReadLimit(4 << 20)
	return c, nil
}

// GCRequest asks the daemon to prune old data and/or compress closed logs.
type GCRequest struct {
	OlderThanS int64 `json:"older_than_s,omitempty"` // forget sessions idle for longer than this (0 = prune nothing)
	Compress   bool  `json:"compress,omitempty"`     // zstd-compress closed raw log segments
	DryRun     bool  `json:"dry_run,omitempty"`      // report only
}

// GCReport says what a GC did (or, with DryRun, would do).
type GCReport struct {
	DryRun             bool  `json:"dry_run"`
	Sessions           int   `json:"sessions"`
	Agents             int   `json:"agents"`
	Messages           int   `json:"messages"`
	Events             int   `json:"events"`
	RawBytesFreed      int64 `json:"raw_bytes_freed"`
	LocalLogsRemoved   int   `json:"local_logs_removed"`
	SegmentsCompressed int   `json:"segments_compressed"`
	BytesSaved         int64 `json:"bytes_saved"`
}
