package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/thesahibnanda-max/relay/internal/ids"
)

// Message states. A message only ever moves forward:
//
//	held ──approve──▶ queued ──push──▶ dispatched ──typed──▶ injected ──▶ acknowledged ──▶ done
//	terminal exits from any live state: rejected, expired, undeliverable
const (
	MsgQueued        = "queued"        // accepted, waiting to be pushed to the target
	MsgHeld          = "held"          // waiting for a human (--approve-inbound, or a hop limit)
	MsgDispatched    = "dispatched"    // pushed to the target agent's scheduler
	MsgInjected      = "injected"      // typed into the target's terminal
	MsgAcknowledged  = "acknowledged"  // the target read it (relay_inbox / relay_ack / a reply)
	MsgDone          = "done"          // answered or otherwise finished
	MsgRejected      = "rejected"      // a human said no
	MsgExpired       = "expired"       // its TTL ran out first
	MsgUndeliverable = "undeliverable" // the target is gone for good
)

// Message kinds.
const (
	KindTask     = "task"
	KindQuestion = "question"
	KindAnswer   = "answer"
	KindNotify   = "notify"
	KindControl  = "control"
)

// Priorities: lower is more urgent.
const (
	P0 = 0 // interrupt
	P1 = 1 // high: next safe point
	P2 = 2 // normal: when idle
	P3 = 3 // low / FYI
)

var ErrBadTransition = errors.New("illegal message state transition")

// nextStates lists where each state may go.
var nextStates = map[string][]string{
	MsgHeld:         {MsgQueued, MsgRejected, MsgExpired, MsgUndeliverable},
	MsgQueued:       {MsgDispatched, MsgInjected, MsgAcknowledged, MsgDone, MsgExpired, MsgUndeliverable, MsgRejected},
	MsgDispatched:   {MsgInjected, MsgAcknowledged, MsgDone, MsgExpired, MsgUndeliverable, MsgRejected},
	MsgInjected:     {MsgAcknowledged, MsgDone},
	MsgAcknowledged: {MsgDone},
}

// IsTerminal reports whether a state is final.
func IsTerminal(state string) bool {
	switch state {
	case MsgDone, MsgRejected, MsgExpired, MsgUndeliverable:
		return true
	}
	return false
}

// CanTransition reports whether from -> to is a legal move.
func CanTransition(from, to string) bool {
	for _, n := range nextStates[from] {
		if n == to {
			return true
		}
	}
	return false
}

type Message struct {
	ID        string
	SessionID string
	FromAgent string // "" for the user or Relay itself
	FromName  string
	FromRole  string
	FromPeer  string // owner peer of FromAgent if it's remote; "" for local/user/relay
	ToAgent   string
	ToName    string
	ToPeer    string // owner peer of ToAgent if it's remote; "" for a local recipient
	Kind      string
	Priority  int
	Thread    string
	ReplyTo   string
	Body      string
	Hops      int
	State     string
	Detail    string
	Attempts  int
	Origin    string // "local" (authoritative here) or "mirror" (authoritative on ToPeer's daemon)
	Rev       uint64 // monotonic per-row version; guards a receipt against being applied out of order
	CreatedAt time.Time
	UpdatedAt time.Time
	ExpiresAt time.Time
}

// Message origins (see 005_mesh_messages.sql).
const (
	OriginLocal  = "local"
	OriginMirror = "mirror"
)

const messageCols = `id, session_id, COALESCE(from_agent,''), from_name, from_role, from_peer, to_agent, to_name, to_peer, kind, priority, thread, reply_to, body, hops, state, detail, attempts, origin, rev, created_at, updated_at, expires_at`

func scanMessage(sc interface{ Scan(...any) error }) (Message, error) {
	var m Message
	var c, u, e, rev int64
	if err := sc.Scan(&m.ID, &m.SessionID, &m.FromAgent, &m.FromName, &m.FromRole, &m.FromPeer, &m.ToAgent, &m.ToName, &m.ToPeer,
		&m.Kind, &m.Priority, &m.Thread, &m.ReplyTo, &m.Body, &m.Hops, &m.State, &m.Detail, &m.Attempts, &m.Origin, &rev, &c, &u, &e); err != nil {
		return Message{}, err
	}
	m.Rev = uint64(rev)
	m.CreatedAt, m.UpdatedAt, m.ExpiresAt = fromMS(c), fromMS(u), fromMS(e)
	return m, nil
}

// CreateMessage stores a new message. ID, Thread (defaults to the ID), the
// timestamps, State (defaults to queued) and Origin (defaults to local) are
// filled in when empty. A fresh local (authoritative) row starts at Rev=1 -
// its initial state is itself the first thing a receipt ever reports, and a
// mirror row starts at Rev=0 (correct either way: Rev only ever means
// "highest receipt applied," which is meaningless until one arrives, or
// "current state's version for the next receipt sent," which is 1 as soon
// as there is a state to report at all).
func (s *Store) CreateMessage(ctx context.Context, m Message) (Message, error) {
	now := time.Now()
	if m.ID == "" {
		m.ID = ids.New()
	}
	if m.Thread == "" {
		m.Thread = m.ID
	}
	if m.State == "" {
		m.State = MsgQueued
	}
	if m.Origin == "" {
		m.Origin = OriginLocal
	}
	if m.Rev == 0 && m.Origin != OriginMirror {
		m.Rev = 1
	}
	if m.CreatedAt.IsZero() {
		m.CreatedAt = now
	}
	m.UpdatedAt = now
	if m.ExpiresAt.IsZero() {
		m.ExpiresAt = now.Add(DefaultMessageTTL)
	}
	var from any
	if m.FromAgent != "" {
		from = m.FromAgent
	}
	_, err := s.w.ExecContext(ctx, `INSERT INTO messages(id, session_id, from_agent, from_name, from_role, from_peer, to_agent, to_name, to_peer, kind, priority, thread, reply_to, body, hops, state, detail, attempts, origin, rev, created_at, updated_at, expires_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		m.ID, m.SessionID, from, m.FromName, m.FromRole, m.FromPeer, m.ToAgent, m.ToName, m.ToPeer, m.Kind, m.Priority, m.Thread, m.ReplyTo,
		m.Body, m.Hops, m.State, m.Detail, m.Attempts, m.Origin, m.Rev, ms(m.CreatedAt), ms(m.UpdatedAt), ms(m.ExpiresAt))
	return m, err
}

// ApplyHandoff creates this daemon's authoritative row for a message handed
// off by another daemon, or - if id already exists, a resend after a
// reconnect - returns the existing row unchanged. This idempotency is what
// makes at-least-once handoff resend safe: the sender's daemon may deliver
// the exact same handoff more than once, and only the first ever creates a
// row here.
func (s *Store) ApplyHandoff(ctx context.Context, m Message) (created bool, out Message, err error) {
	existing, err := s.GetMessage(ctx, m.ID)
	if err == nil {
		return false, existing, nil
	}
	if !errors.Is(err, ErrNotFound) {
		return false, Message{}, err
	}
	out, err = s.CreateMessage(ctx, m)
	if isUnique(err) {
		// Lost a race with a concurrent resend of the same handoff.
		if existing, gerr := s.GetMessage(ctx, m.ID); gerr == nil {
			return false, existing, nil
		}
	}
	return err == nil, out, err
}

// ApplyReceipt updates a mirror row's mirrored state from the owning
// daemon's receipt, if rev is strictly greater than what is already
// recorded - the same "exactly one legitimate writer per version"
// guarantee mesh_agents gossip uses (see UpsertMeshAgentIfNewer), applied
// to messages instead of agents. Unlike Advance, this never enforces
// CanTransition: a mirror row does not run the state machine itself, it
// just reflects whatever state the authoritative daemon already legally
// reached, however many steps a resend after a reconnect skips at once.
func (s *Store) ApplyReceipt(ctx context.Context, id, state, detail string, rev uint64) (applied bool, m Message, err error) {
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return false, Message{}, err
	}
	defer tx.Rollback()
	m, err = scanMessage(tx.QueryRowContext(ctx, `SELECT `+messageCols+` FROM messages WHERE id=?`, ids.Normalize(id)))
	if errors.Is(err, sql.ErrNoRows) {
		return false, Message{}, ErrNotFound
	} else if err != nil {
		return false, Message{}, err
	}
	if m.Rev >= rev {
		return false, m, tx.Commit()
	}
	now := time.Now()
	if _, err := tx.ExecContext(ctx, `UPDATE messages SET state=?, detail=?, rev=?, updated_at=? WHERE id=?`,
		state, detail, rev, ms(now), m.ID); err != nil {
		return false, Message{}, err
	}
	if err := tx.Commit(); err != nil {
		return false, Message{}, err
	}
	m.State, m.Detail, m.Rev, m.UpdatedAt = state, detail, rev, now
	return true, m, nil
}

// PendingMirrorMessages returns this daemon's mirror-origin messages
// destined to peerID that have never had a receipt applied (Rev==0) and
// have not yet expired - handoffs that may never have reached the owning
// daemon, or whose very first receipt is still outstanding. Used to resend
// a handoff on mesh reconnect (see federation.Hub.Resync); once a receipt
// has been applied even once, no further resend from this side is needed -
// the owning daemon's own PendingReceipts takes over from there.
func (s *Store) PendingMirrorMessages(ctx context.Context, sessionID, peerID string, now time.Time) ([]Message, error) {
	return s.queryMessages(ctx, `SELECT `+messageCols+` FROM messages
		WHERE session_id=? AND origin='mirror' AND to_peer=? AND rev=0 AND expires_at>?
		ORDER BY id`, ids.Normalize(sessionID), peerID, ms(now))
}

// PendingReceipts returns this daemon's authoritative messages sent by an
// agent owned by peerID whose current state peerID might not have: still
// live, or finished recently enough that a receipt could have been missed
// across a disconnect. Used to resend a receipt on mesh reconnect - the
// same "resend full current truth" idiom mesh_agents gossip already uses
// for the roster, rather than tracking per-peer acknowledgement state.
func (s *Store) PendingReceipts(ctx context.Context, sessionID, peerID string, recentEnough time.Time) ([]Message, error) {
	return s.queryMessages(ctx, `SELECT `+messageCols+` FROM messages
		WHERE session_id=? AND origin='local' AND from_peer=? AND (state NOT IN ('done','rejected','expired','undeliverable') OR updated_at>?)
		ORDER BY id`, ids.Normalize(sessionID), peerID, ms(recentEnough))
}

// DefaultMessageTTL is how long an undelivered message stays alive.
const DefaultMessageTTL = time.Hour

func (s *Store) GetMessage(ctx context.Context, id string) (Message, error) {
	m, err := scanMessage(s.r.QueryRowContext(ctx, `SELECT `+messageCols+` FROM messages WHERE id=?`, ids.Normalize(id)))
	if errors.Is(err, sql.ErrNoRows) {
		return Message{}, ErrNotFound
	}
	return m, err
}

// Advance moves a message to a new state if that is a legal forward move.
// changed is false (and err nil) when the message is already in that state, so
// callers can be idempotent; an illegal move is ErrBadTransition. The detail
// is only replaced when non-empty.
func (s *Store) Advance(ctx context.Context, id, to, detail string) (m Message, changed bool, err error) {
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return Message{}, false, err
	}
	defer tx.Rollback()
	m, err = scanMessage(tx.QueryRowContext(ctx, `SELECT `+messageCols+` FROM messages WHERE id=?`, ids.Normalize(id)))
	if errors.Is(err, sql.ErrNoRows) {
		return Message{}, false, ErrNotFound
	} else if err != nil {
		return Message{}, false, err
	}
	if m.State == to {
		return m, false, nil
	}
	if !CanTransition(m.State, to) {
		return m, false, fmt.Errorf("%w: %s -> %s", ErrBadTransition, m.State, to)
	}
	now := time.Now()
	attempts := m.Attempts
	if to == MsgDispatched {
		attempts++
	}
	if detail == "" {
		detail = m.Detail
	}
	rev := m.Rev + 1
	if _, err := tx.ExecContext(ctx, `UPDATE messages SET state=?, detail=?, attempts=?, rev=?, updated_at=? WHERE id=?`,
		to, detail, attempts, rev, ms(now), m.ID); err != nil {
		return Message{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return Message{}, false, err
	}
	m.State, m.Detail, m.Attempts, m.Rev, m.UpdatedAt = to, detail, attempts, rev, now
	return m, true, nil
}

// Redispatch records another push of an already dispatched message.
func (s *Store) Redispatch(ctx context.Context, id string) error {
	_, err := s.w.ExecContext(ctx, `UPDATE messages SET attempts=attempts+1, updated_at=? WHERE id=? AND state='dispatched'`, ms(time.Now()), id)
	return err
}

// Deliverable returns an agent's messages that still need to reach its
// scheduler (queued, or dispatched but never confirmed), most urgent first.
func (s *Store) Deliverable(ctx context.Context, agentID string) ([]Message, error) {
	return s.queryMessages(ctx, `SELECT `+messageCols+` FROM messages WHERE to_agent=? AND state IN ('queued','dispatched') ORDER BY priority, id`, agentID)
}

// MessageFilter narrows ListMessages.
type MessageFilter struct {
	Session string
	Agent   string   // messages sent by or to this agent id
	To      string   // messages to this agent id
	States  []string // any of these states
	Limit   int      // default 100
}

// ListMessages returns matching messages, newest first.
func (s *Store) ListMessages(ctx context.Context, f MessageFilter) ([]Message, error) {
	var where []string
	var args []any
	if f.Session != "" {
		where = append(where, "session_id=?")
		args = append(args, ids.Normalize(f.Session))
	}
	if f.Agent != "" {
		where = append(where, "(to_agent=? OR from_agent=?)")
		args = append(args, f.Agent, f.Agent)
	}
	if f.To != "" {
		where = append(where, "to_agent=?")
		args = append(args, f.To)
	}
	if len(f.States) > 0 {
		where = append(where, "state IN ("+strings.TrimSuffix(strings.Repeat("?,", len(f.States)), ",")+")")
		for _, st := range f.States {
			args = append(args, st)
		}
	}
	q := `SELECT ` + messageCols + ` FROM messages`
	if len(where) > 0 {
		q += " WHERE " + strings.Join(where, " AND ")
	}
	limit := f.Limit
	if limit <= 0 || limit > 1000 {
		limit = 100
	}
	q += fmt.Sprintf(" ORDER BY id DESC LIMIT %d", limit)
	return s.queryMessages(ctx, q, args...)
}

// Replies returns the messages that answer id, oldest first.
func (s *Store) Replies(ctx context.Context, id string) ([]Message, error) {
	return s.queryMessages(ctx, `SELECT `+messageCols+` FROM messages WHERE reply_to=? ORDER BY id`, ids.Normalize(id))
}

// FindDuplicate returns a live message with the same sender, target, kind and
// body created since the cutoff (used to coalesce accidental repeats).
func (s *Store) FindDuplicate(ctx context.Context, from, to, kind, body string, since time.Time) (Message, bool, error) {
	m, err := scanMessage(s.r.QueryRowContext(ctx, `SELECT `+messageCols+` FROM messages
		WHERE COALESCE(from_agent,'')=? AND to_agent=? AND kind=? AND body=? AND created_at>=?
		  AND state IN ('held','queued','dispatched','injected')
		ORDER BY id DESC LIMIT 1`, from, to, kind, body, ms(since)))
	if errors.Is(err, sql.ErrNoRows) {
		return Message{}, false, nil
	}
	return m, err == nil, err
}

// ExpireDue moves live messages whose TTL has passed to expired and returns them.
func (s *Store) ExpireDue(ctx context.Context, now time.Time) ([]Message, error) {
	return s.failWhere(ctx, MsgExpired, "expired before it could be delivered", `expires_at<=? AND state IN ('held','queued','dispatched')`, ms(now))
}

// FailPending marks an agent's undelivered messages (never typed into its
// terminal) as terminally failed, e.g. undeliverable once it has exited.
func (s *Store) FailPending(ctx context.Context, toAgent, state, detail string) ([]Message, error) {
	return s.failWhere(ctx, state, detail, `to_agent=? AND state IN ('held','queued','dispatched')`, toAgent)
}

func (s *Store) failWhere(ctx context.Context, state, detail, where string, args ...any) ([]Message, error) {
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	rows, err := tx.QueryContext(ctx, `SELECT `+messageCols+` FROM messages WHERE `+where, args...)
	if err != nil {
		return nil, err
	}
	var out []Message
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			rows.Close()
			return nil, err
		}
		out = append(out, m)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	now := time.Now()
	for i := range out {
		rev := out[i].Rev + 1
		if _, err := tx.ExecContext(ctx, `UPDATE messages SET state=?, detail=?, rev=?, updated_at=? WHERE id=?`, state, detail, rev, ms(now), out[i].ID); err != nil {
			return nil, err
		}
		out[i].State, out[i].Detail, out[i].Rev, out[i].UpdatedAt = state, detail, rev, now
	}
	return out, tx.Commit()
}

func (s *Store) queryMessages(ctx context.Context, q string, args ...any) ([]Message, error) {
	rows, err := s.r.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Message
	for rows.Next() {
		m, err := scanMessage(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, m)
	}
	return out, rows.Err()
}
