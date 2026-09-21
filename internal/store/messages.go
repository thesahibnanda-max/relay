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
	ToAgent   string
	ToName    string
	Kind      string
	Priority  int
	Thread    string
	ReplyTo   string
	Body      string
	Hops      int
	State     string
	Detail    string
	Attempts  int
	CreatedAt time.Time
	UpdatedAt time.Time
	ExpiresAt time.Time
}

const messageCols = `id, session_id, COALESCE(from_agent,''), from_name, from_role, to_agent, to_name, kind, priority, thread, reply_to, body, hops, state, detail, attempts, created_at, updated_at, expires_at`

func scanMessage(sc interface{ Scan(...any) error }) (Message, error) {
	var m Message
	var c, u, e int64
	if err := sc.Scan(&m.ID, &m.SessionID, &m.FromAgent, &m.FromName, &m.FromRole, &m.ToAgent, &m.ToName, &m.Kind,
		&m.Priority, &m.Thread, &m.ReplyTo, &m.Body, &m.Hops, &m.State, &m.Detail, &m.Attempts, &c, &u, &e); err != nil {
		return Message{}, err
	}
	m.CreatedAt, m.UpdatedAt, m.ExpiresAt = fromMS(c), fromMS(u), fromMS(e)
	return m, nil
}

// CreateMessage stores a new message. ID, Thread (defaults to the ID), the
// timestamps and State (defaults to queued) are filled in when empty.
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
	_, err := s.w.ExecContext(ctx, `INSERT INTO messages(id, session_id, from_agent, from_name, from_role, to_agent, to_name, kind, priority, thread, reply_to, body, hops, state, detail, attempts, created_at, updated_at, expires_at) VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?,?)`,
		m.ID, m.SessionID, from, m.FromName, m.FromRole, m.ToAgent, m.ToName, m.Kind, m.Priority, m.Thread, m.ReplyTo,
		m.Body, m.Hops, m.State, m.Detail, m.Attempts, ms(m.CreatedAt), ms(m.UpdatedAt), ms(m.ExpiresAt))
	return m, err
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
	if _, err := tx.ExecContext(ctx, `UPDATE messages SET state=?, detail=?, attempts=?, updated_at=? WHERE id=?`,
		to, detail, attempts, ms(now), m.ID); err != nil {
		return Message{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return Message{}, false, err
	}
	m.State, m.Detail, m.Attempts, m.UpdatedAt = to, detail, attempts, now
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
		if _, err := tx.ExecContext(ctx, `UPDATE messages SET state=?, detail=?, updated_at=? WHERE id=?`, state, detail, ms(now), out[i].ID); err != nil {
			return nil, err
		}
		out[i].State, out[i].Detail, out[i].UpdatedAt = state, detail, now
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
