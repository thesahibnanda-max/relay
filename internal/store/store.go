// Package store is Relay's durable registry: sessions, agents and structured
// events, in SQLite (WAL) via the pure-Go modernc driver.
//
// Only the daemon opens the database, so there is one writer process; the
// writer pool has a single connection (serialised, IMMEDIATE transactions) and
// readers use their own pool, which WAL lets run concurrently with it.
package store

import (
	"context"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"embed"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/thesahibnanda-max/relay/internal/ids"
	"github.com/thesahibnanda-max/relay/internal/naming"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

var (
	ErrNotFound       = errors.New("not found")
	ErrSessionEnded   = errors.New("session has ended")
	ErrNameTaken      = errors.New("name already used in this session")
	ErrBadName        = errors.New("invalid name")
	ErrBadToken       = errors.New("invalid resume token")
	ErrAgentLive      = errors.New("agent is currently connected")
	ErrNameGenExhaust = errors.New("could not find a free name")
)

type Store struct {
	w, r *sql.DB
}

// Open creates (0600) and migrates the database at path.
func Open(path string) (*Store, error) {
	// Create the file privately first: SQLite gives -wal/-shm the same mode.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	f.Close()
	_ = os.Chmod(path, 0o600)

	dsn := func(extra string) string {
		return "file:" + path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)&_pragma=synchronous(NORMAL)" + extra
	}
	w, err := sql.Open("sqlite", dsn("&_txlock=immediate"))
	if err != nil {
		return nil, err
	}
	w.SetMaxOpenConns(1)
	r, err := sql.Open("sqlite", dsn(""))
	if err != nil {
		w.Close()
		return nil, err
	}
	r.SetMaxOpenConns(8)
	s := &Store{w: w, r: r}
	if err := s.migrate(); err != nil {
		s.Close()
		return nil, fmt.Errorf("migrate: %w", err)
	}
	return s, nil
}

func (s *Store) Close() error {
	e1, e2 := s.w.Close(), s.r.Close()
	if e1 != nil {
		return e1
	}
	return e2
}

func (s *Store) migrate() error {
	var cur int
	if err := s.w.QueryRow(`PRAGMA user_version`).Scan(&cur); err != nil {
		return err
	}
	entries, err := migrationsFS.ReadDir("migrations")
	if err != nil {
		return err
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		names = append(names, e.Name())
	}
	sort.Strings(names)
	for i, name := range names {
		version := i + 1
		if version <= cur {
			continue
		}
		body, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			return err
		}
		tx, err := s.w.Begin()
		if err != nil {
			return err
		}
		if _, err := tx.Exec(string(body)); err != nil {
			tx.Rollback()
			return fmt.Errorf("%s: %w", name, err)
		}
		if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, version)); err != nil {
			tx.Rollback()
			return err
		}
		if err := tx.Commit(); err != nil {
			return err
		}
	}
	return nil
}

func ms(t time.Time) int64 { return t.UnixMilli() }
func fromMS(v int64) time.Time {
	return time.UnixMilli(v)
}

func hashToken(tok string) string {
	sum := sha256.Sum256([]byte(tok))
	return hex.EncodeToString(sum[:])
}

func isUnique(err error) bool {
	return err != nil && strings.Contains(err.Error(), "UNIQUE constraint failed")
}

// ---- sessions -------------------------------------------------------------

type Session struct {
	ID        string
	Name      string
	Kind      string // "shared" or "solo"
	Status    string // "active" or "ended"
	CreatedAt time.Time
	EndedAt   time.Time // zero unless ended
	Agents    int       // live (non-exited) agents; filled by ListSessions
}

func (s *Store) CreateSession(ctx context.Context, kind, name string) (Session, error) {
	if kind != "shared" && kind != "solo" {
		return Session{}, fmt.Errorf("bad session kind %q", kind)
	}
	now := time.Now()
	sess := Session{ID: ids.New(), Name: name, Kind: kind, Status: "active", CreatedAt: now}
	_, err := s.w.ExecContext(ctx, `INSERT INTO sessions(id,name,kind,status,created_at) VALUES(?,?,?,?,?)`,
		sess.ID, name, kind, "active", ms(now))
	return sess, err
}

const sessionCols = `id, name, kind, status, created_at, COALESCE(ended_at,0)`

func scanSession(sc interface{ Scan(...any) error }) (Session, error) {
	var x Session
	var c, e int64
	if err := sc.Scan(&x.ID, &x.Name, &x.Kind, &x.Status, &c, &e); err != nil {
		return Session{}, err
	}
	x.CreatedAt = fromMS(c)
	if e != 0 {
		x.EndedAt = fromMS(e)
	}
	return x, nil
}

func (s *Store) GetSession(ctx context.Context, id string) (Session, error) {
	x, err := scanSession(s.r.QueryRowContext(ctx, `SELECT `+sessionCols+` FROM sessions WHERE id=?`, ids.Normalize(id)))
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, ErrNotFound
	}
	return x, err
}

// ListSessions returns sessions newest first. Without all, only active shared
// sessions are returned (solo/ended ones are usually noise).
func (s *Store) ListSessions(ctx context.Context, all bool) ([]Session, error) {
	q := `SELECT ` + sessionCols + `,
	        (SELECT COUNT(*) FROM agents a WHERE a.session_id = sessions.id AND a.status != 'exited')
	      FROM sessions`
	if !all {
		q += ` WHERE status='active' AND kind='shared'`
	}
	q += ` ORDER BY id DESC`
	rows, err := s.r.QueryContext(ctx, q)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Session
	for rows.Next() {
		var x Session
		var c, e int64
		if err := rows.Scan(&x.ID, &x.Name, &x.Kind, &x.Status, &c, &e, &x.Agents); err != nil {
			return nil, err
		}
		x.CreatedAt = fromMS(c)
		if e != 0 {
			x.EndedAt = fromMS(e)
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// EndSession marks a session ended (idempotent). Agents keep running; they
// just can no longer be joined by new ones.
func (s *Store) EndSession(ctx context.Context, id string) error {
	res, err := s.w.ExecContext(ctx, `UPDATE sessions SET status='ended', ended_at=COALESCE(ended_at,?) WHERE id=?`, ms(time.Now()), ids.Normalize(id))
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// ---- agents ---------------------------------------------------------------

type Agent struct {
	ID             string
	SessionID      string
	Name           string
	Tool           string
	Role           string
	RoleSource     string
	Status         string // connected | disconnected | exited
	ApproveInbound bool
	PID            int
	Cwd            string
	CreatedAt      time.Time
	LastSeenAt     time.Time
	ExitCode       *int
	AckedSeq       uint64 // highest event seq durably stored (structured + raw)
}

const agentCols = `id, session_id, name, tool, role, role_source, status, approve_inbound, pid, cwd, created_at, last_seen_at, exit_code, acked_seq`

func scanAgent(sc interface{ Scan(...any) error }) (Agent, error) {
	var a Agent
	var appr int
	var c, l int64
	var ec sql.NullInt64
	var acked int64
	if err := sc.Scan(&a.ID, &a.SessionID, &a.Name, &a.Tool, &a.Role, &a.RoleSource, &a.Status, &appr, &a.PID, &a.Cwd, &c, &l, &ec, &acked); err != nil {
		return Agent{}, err
	}
	a.AckedSeq = uint64(acked)
	a.ApproveInbound = appr != 0
	a.CreatedAt, a.LastSeenAt = fromMS(c), fromMS(l)
	if ec.Valid {
		v := int(ec.Int64)
		a.ExitCode = &v
	}
	return a, nil
}

type RegisterParams struct {
	SessionID      string
	Tool           string
	Role           string
	RoleSource     string
	Name           string // optional; generated if empty
	ApproveInbound bool
	PID            int
	Cwd            string
}

// RegisterAgent adds an agent to an active session and returns it with a
// fresh resume token (only its hash is stored). An explicit Name that is
// invalid or taken is an error; an empty Name is generated, retrying until the
// database's UNIQUE constraint accepts one.
func (s *Store) RegisterAgent(ctx context.Context, p RegisterParams, namer *naming.Namer) (Agent, string, error) {
	sess, err := s.GetSession(ctx, p.SessionID)
	if err != nil {
		return Agent{}, "", err
	}
	if sess.Status != "active" {
		return Agent{}, "", ErrSessionEnded
	}
	if p.Name != "" {
		if ok, why := naming.Validate(p.Name); !ok {
			return Agent{}, "", fmt.Errorf("%w: %s", ErrBadName, why)
		}
	}
	if namer == nil {
		namer = naming.New()
	}

	token := ids.Token()
	now := time.Now()
	a := Agent{ID: ids.New(), SessionID: sess.ID, Tool: p.Tool, Role: p.Role, RoleSource: p.RoleSource,
		Status: "connected", ApproveInbound: p.ApproveInbound, PID: p.PID, Cwd: p.Cwd, CreatedAt: now, LastSeenAt: now}

	const maxAttempts = 60
	for attempt := 0; ; attempt++ {
		a.Name = p.Name
		if a.Name == "" {
			if attempt >= maxAttempts {
				return Agent{}, "", ErrNameGenExhaust
			}
			a.Name = namer.Candidate(attempt)
		}
		appr := 0
		if a.ApproveInbound {
			appr = 1
		}
		_, err := s.w.ExecContext(ctx, `INSERT INTO agents(id,session_id,name,tool,role,role_source,token_hash,status,approve_inbound,pid,cwd,created_at,last_seen_at)
		                                 VALUES(?,?,?,?,?,?,?,?,?,?,?,?,?)`,
			a.ID, a.SessionID, a.Name, a.Tool, a.Role, a.RoleSource, hashToken(token), a.Status, appr, a.PID, a.Cwd, ms(now), ms(now))
		if err == nil {
			return a, token, nil
		}
		if !isUnique(err) {
			return Agent{}, "", err
		}
		if p.Name != "" {
			return Agent{}, "", ErrNameTaken
		}
		// generated name collided: loop with the next candidate
	}
}

// ResumeAgent re-attaches a previously registered agent, proving identity
// with its token. The agent must not currently be connected.
func (s *Store) ResumeAgent(ctx context.Context, sessionID, name, token string) (Agent, error) {
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return Agent{}, err
	}
	defer tx.Rollback()
	row := tx.QueryRowContext(ctx, `SELECT `+agentCols+`, token_hash FROM agents WHERE session_id=? AND name=? COLLATE NOCASE`, ids.Normalize(sessionID), name)
	var a Agent
	var hash string
	var appr int
	var c, l int64
	var ec sql.NullInt64
	var acked int64
	if err := row.Scan(&a.ID, &a.SessionID, &a.Name, &a.Tool, &a.Role, &a.RoleSource, &a.Status, &appr, &a.PID, &a.Cwd, &c, &l, &ec, &acked, &hash); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return Agent{}, ErrNotFound
		}
		return Agent{}, err
	}
	if subtle.ConstantTimeCompare([]byte(hash), []byte(hashToken(token))) != 1 {
		return Agent{}, ErrBadToken
	}
	sess, err := s.getSessionTx(ctx, tx, a.SessionID)
	if err != nil {
		return Agent{}, err
	}
	if sess.Status != "active" {
		return Agent{}, ErrSessionEnded
	}
	if a.Status == "connected" {
		return Agent{}, ErrAgentLive
	}
	now := ms(time.Now())
	if _, err := tx.ExecContext(ctx, `UPDATE agents SET status='connected', last_seen_at=?, exit_code=NULL WHERE id=?`, now, a.ID); err != nil {
		return Agent{}, err
	}
	if err := tx.Commit(); err != nil {
		return Agent{}, err
	}
	a.ApproveInbound, a.CreatedAt, a.LastSeenAt, a.Status, a.ExitCode, a.AckedSeq = appr != 0, fromMS(c), fromMS(now), "connected", nil, uint64(acked)
	return a, nil
}

func (s *Store) getSessionTx(ctx context.Context, tx *sql.Tx, id string) (Session, error) {
	x, err := scanSession(tx.QueryRowContext(ctx, `SELECT `+sessionCols+` FROM sessions WHERE id=?`, id))
	if errors.Is(err, sql.ErrNoRows) {
		return Session{}, ErrNotFound
	}
	return x, err
}

func (s *Store) SetAgentStatus(ctx context.Context, agentID, status string, exitCode *int) error {
	var ec any
	if exitCode != nil {
		ec = *exitCode
	}
	_, err := s.w.ExecContext(ctx, `UPDATE agents SET status=?, last_seen_at=?, exit_code=COALESCE(?, exit_code) WHERE id=?`, status, ms(time.Now()), ec, agentID)
	return err
}

func (s *Store) Touch(ctx context.Context, agentID string) error {
	_, err := s.w.ExecContext(ctx, `UPDATE agents SET last_seen_at=? WHERE id=?`, ms(time.Now()), agentID)
	return err
}

func (s *Store) GetAgent(ctx context.Context, id string) (Agent, error) {
	a, err := scanAgent(s.r.QueryRowContext(ctx, `SELECT `+agentCols+` FROM agents WHERE id=?`, ids.Normalize(id)))
	if errors.Is(err, sql.ErrNoRows) {
		return Agent{}, ErrNotFound
	}
	return a, err
}

func (s *Store) FindAgent(ctx context.Context, sessionID, name string) (Agent, error) {
	a, err := scanAgent(s.r.QueryRowContext(ctx, `SELECT `+agentCols+` FROM agents WHERE session_id=? AND name=? COLLATE NOCASE`, ids.Normalize(sessionID), name))
	if errors.Is(err, sql.ErrNoRows) {
		return Agent{}, ErrNotFound
	}
	return a, err
}

// ListAgents returns a session's agents in join order. Exited agents are
// included only if withExited.
func (s *Store) ListAgents(ctx context.Context, sessionID string, withExited bool) ([]Agent, error) {
	q := `SELECT ` + agentCols + ` FROM agents WHERE session_id=?`
	if !withExited {
		q += ` AND status != 'exited'`
	}
	q += ` ORDER BY id`
	rows, err := s.r.QueryContext(ctx, q, ids.Normalize(sessionID))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Agent
	for rows.Next() {
		a, err := scanAgent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// MarkAllDisconnected is run at daemon start: any agent still marked
// connected belonged to a previous daemon process and is not connected now.
func (s *Store) MarkAllDisconnected(ctx context.Context) error {
	// last_seen is refreshed: their silence so far was the daemon's downtime,
	// not theirs, and they get the full grace period to reconnect.
	_, err := s.w.ExecContext(ctx, `UPDATE agents SET status='disconnected', last_seen_at=? WHERE status='connected'`, ms(time.Now()))
	return err
}

// StaleDisconnected returns agents that have been disconnected since before cutoff.
func (s *Store) StaleDisconnected(ctx context.Context, cutoff time.Time) ([]Agent, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT `+agentCols+` FROM agents WHERE status='disconnected' AND last_seen_at<?`, ms(cutoff))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Agent
	for rows.Next() {
		a, err := scanAgent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// ---- events ---------------------------------------------------------------

type EventRow struct {
	Seq     uint64
	TS      time.Time
	Type    string
	Payload string // JSON
}

// AppendEvents stores structured events idempotently (a resent seq is
// ignored) and records ack as the agent's highest durably-stored seq (raw
// events, which are not in this table, count towards it). It returns the
// agent's acked seq after the update.
func (s *Store) AppendEvents(ctx context.Context, agentID string, evs []EventRow, ack uint64) (uint64, error) {
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback()
	if len(evs) > 0 {
		st, err := tx.PrepareContext(ctx, `INSERT OR IGNORE INTO events(agent_id,seq,ts,type,payload) VALUES(?,?,?,?,?)`)
		if err != nil {
			return 0, err
		}
		defer st.Close()
		for _, e := range evs {
			if _, err := st.ExecContext(ctx, agentID, int64(e.Seq), ms(e.TS), e.Type, e.Payload); err != nil {
				return 0, err
			}
			if e.Seq > ack {
				ack = e.Seq
			}
		}
	}
	res, err := tx.ExecContext(ctx, `UPDATE agents SET acked_seq = MAX(acked_seq, ?), last_seen_at = ? WHERE id = ?`, int64(ack), ms(time.Now()), agentID)
	if err != nil {
		return 0, err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return 0, ErrNotFound
	}
	var acked int64
	if err := tx.QueryRowContext(ctx, `SELECT acked_seq FROM agents WHERE id=?`, agentID).Scan(&acked); err != nil {
		return 0, err
	}
	if err := tx.Commit(); err != nil {
		return 0, err
	}
	return uint64(acked), nil
}

// Events returns an agent's stored structured events, oldest first.
func (s *Store) Events(ctx context.Context, agentID string) ([]EventRow, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT seq, ts, type, payload FROM events WHERE agent_id=? ORDER BY seq`, agentID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EventRow
	for rows.Next() {
		var e EventRow
		var seq, ts int64
		if err := rows.Scan(&seq, &ts, &e.Type, &e.Payload); err != nil {
			return nil, err
		}
		e.Seq, e.TS = uint64(seq), fromMS(ts)
		out = append(out, e)
	}
	return out, rows.Err()
}

// TurnQuery narrows Turns.
type TurnQuery struct {
	Since    time.Time // only turns at or after this time (zero: any)
	Contains string    // only turns whose text contains this (case-insensitive)
	Role     string    // only this role
	Limit    int       // newest N (default 10)
}

// Turns returns an agent's stored conversation turns, oldest first, choosing
// the newest Limit that match.
func (s *Store) Turns(ctx context.Context, agentID string, q TurnQuery) ([]EventRow, error) {
	where := []string{"agent_id=?", "type='turn'"}
	args := []any{agentID}
	if !q.Since.IsZero() {
		where = append(where, "ts>=?")
		args = append(args, ms(q.Since))
	}
	if q.Contains != "" {
		esc := strings.NewReplacer(`\`, `\\`, `%`, `\%`, `_`, `\_`).Replace(q.Contains)
		where = append(where, `json_extract(payload,'$.text') LIKE ? ESCAPE '\'`)
		args = append(args, "%"+esc+"%")
	}
	if q.Role != "" {
		where = append(where, `json_extract(payload,'$.role')=?`)
		args = append(args, q.Role)
	}
	limit := q.Limit
	if limit <= 0 || limit > 500 {
		limit = 10
	}
	rows, err := s.r.QueryContext(ctx, fmt.Sprintf(`SELECT seq, ts, type, payload FROM events WHERE %s ORDER BY seq DESC LIMIT %d`, strings.Join(where, " AND "), limit), args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EventRow
	for rows.Next() {
		var e EventRow
		var seq, ts int64
		if err := rows.Scan(&seq, &ts, &e.Type, &e.Payload); err != nil {
			return nil, err
		}
		e.Seq, e.TS = uint64(seq), fromMS(ts)
		out = append(out, e)
	}
	for i, j := 0, len(out)-1; i < j; i, j = i+1, j-1 { // oldest first
		out[i], out[j] = out[j], out[i]
	}
	return out, rows.Err()
}

// IdleSessions returns sessions with no connected agent whose last activity
// (an agent's last sign of life, or the session's creation) is before cutoff.
func (s *Store) IdleSessions(ctx context.Context, cutoff time.Time) ([]Session, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT `+sessionCols+` FROM sessions
		WHERE NOT EXISTS (SELECT 1 FROM agents a WHERE a.session_id=sessions.id AND a.status='connected')
		  AND COALESCE((SELECT MAX(last_seen_at) FROM agents a WHERE a.session_id=sessions.id), created_at) < ?
		ORDER BY id`, ms(cutoff))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Session
	for rows.Next() {
		x, err := scanSession(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, x)
	}
	return out, rows.Err()
}

// SessionCounts is what deleting a session removes.
type SessionCounts struct{ Agents, Messages, Events int }

// CountSession reports how much data a session holds.
func (s *Store) CountSession(ctx context.Context, id string) (SessionCounts, error) {
	var c SessionCounts
	err := s.r.QueryRowContext(ctx, `SELECT
		(SELECT COUNT(*) FROM agents WHERE session_id=?1),
		(SELECT COUNT(*) FROM messages WHERE session_id=?1),
		(SELECT COUNT(*) FROM events WHERE agent_id IN (SELECT id FROM agents WHERE session_id=?1))`, id).
		Scan(&c.Agents, &c.Messages, &c.Events)
	return c, err
}

// DeleteSession removes a session and everything under it, atomically.
func (s *Store) DeleteSession(ctx context.Context, id string) error {
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for _, q := range []string{
		`DELETE FROM messages WHERE session_id=?1`,
		`DELETE FROM events WHERE agent_id IN (SELECT id FROM agents WHERE session_id=?1)`,
		`DELETE FROM agents WHERE session_id=?1`,
		`DELETE FROM mesh_peers WHERE session_id=?1`,
		`DELETE FROM mesh_sessions WHERE session_id=?1`,
		`DELETE FROM sessions WHERE id=?1`,
	} {
		if _, err := tx.ExecContext(ctx, q, id); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// QuickCheck opens the database read-only (safe while the daemon has it open:
// it is WAL) and runs SQLite's quick integrity check. It returns "ok" or the
// first problem found.
func QuickCheck(path string) (string, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro&_pragma=busy_timeout(3000)")
	if err != nil {
		return "", err
	}
	defer db.Close()
	var res string
	if err := db.QueryRow(`PRAGMA quick_check`).Scan(&res); err != nil {
		return "", err
	}
	return res, nil
}

// SchemaVersion is the number of migrations this build knows about.
func SchemaVersion() int {
	entries, _ := migrationsFS.ReadDir("migrations")
	return len(entries)
}

// StoredSchemaVersion reads the version recorded in an existing database.
func StoredSchemaVersion(path string) (int, error) {
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro&_pragma=busy_timeout(3000)")
	if err != nil {
		return 0, err
	}
	defer db.Close()
	var v int
	err = db.QueryRow(`PRAGMA user_version`).Scan(&v)
	return v, err
}
