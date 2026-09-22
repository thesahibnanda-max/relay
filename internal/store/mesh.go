package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/thesahibnanda-max/relay/internal/ids"
)

// MeshSession is this daemon's record of one session it participates in as a
// mesh member: its join secret (in the clear - see 003_mesh.sql for why) and
// this daemon's own peer id for that session. There is one row per session,
// never shared with any other daemon's database.
type MeshSession struct {
	SessionID  string
	JoinSecret string
	SelfPeerID string
	CreatedAt  time.Time
}

// MeshPeer is another daemon in a mesh session, as last observed by this
// daemon. PeerID is TOFU-pinned: once seen for a session it identifies that
// daemon for the life of the session, never silently reassigned to a
// different key.
type MeshPeer struct {
	SessionID string
	PeerID    string
	Addr      string
	FirstSeen time.Time
	LastSeen  time.Time
	Status    string // "linked" or "unreachable"
}

const (
	MeshPeerLinked      = "linked"
	MeshPeerUnreachable = "unreachable"
)

// EnsureMeshSession returns this daemon's MeshSession row for sessionID,
// creating one with the given join secret and selfPeerID if none exists yet
// (an existing row's own secret/peer id always wins - this is what lets
// Invite re-embed the same secret in a fresh blob, and lets a second Join
// attempt be idempotent rather than erroring). It also creates the local
// `sessions` row itself if this daemon has never seen this session id
// before, since mesh_sessions has a foreign key to it: a joining daemon is,
// by definition, learning about the session for the first time, while a
// hosting daemon already created that row via CreateSession, making this a
// no-op there. Both inserts happen in one transaction.
func (s *Store) EnsureMeshSession(ctx context.Context, sessionID, joinSecret, selfPeerID string) (MeshSession, error) {
	sessionID = ids.Normalize(sessionID)
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return MeshSession{}, err
	}
	defer tx.Rollback()

	now := time.Now()
	if _, err := tx.ExecContext(ctx, `INSERT OR IGNORE INTO sessions(id,name,kind,status,created_at) VALUES(?,?,?,?,?)`,
		sessionID, "", "shared", "active", ms(now)); err != nil {
		return MeshSession{}, err
	}

	var x MeshSession
	var c int64
	err = tx.QueryRowContext(ctx, `SELECT session_id, join_secret, self_peer_id, created_at FROM mesh_sessions WHERE session_id=?`, sessionID).
		Scan(&x.SessionID, &x.JoinSecret, &x.SelfPeerID, &c)
	if err == nil {
		x.CreatedAt = fromMS(c)
		return x, tx.Commit()
	}
	if !errors.Is(err, sql.ErrNoRows) {
		return MeshSession{}, err
	}

	if _, err := tx.ExecContext(ctx, `INSERT INTO mesh_sessions(session_id, join_secret, self_peer_id, created_at) VALUES(?,?,?,?)`,
		sessionID, joinSecret, selfPeerID, ms(now)); err != nil {
		return MeshSession{}, err
	}
	if err := tx.Commit(); err != nil {
		return MeshSession{}, err
	}
	return MeshSession{SessionID: sessionID, JoinSecret: joinSecret, SelfPeerID: selfPeerID, CreatedAt: now}, nil
}

func (s *Store) GetMeshSession(ctx context.Context, sessionID string) (MeshSession, error) {
	var x MeshSession
	var c int64
	err := s.r.QueryRowContext(ctx, `SELECT session_id, join_secret, self_peer_id, created_at FROM mesh_sessions WHERE session_id=?`,
		ids.Normalize(sessionID)).Scan(&x.SessionID, &x.JoinSecret, &x.SelfPeerID, &c)
	if errors.Is(err, sql.ErrNoRows) {
		return MeshSession{}, ErrNotFound
	}
	if err != nil {
		return MeshSession{}, err
	}
	x.CreatedAt = fromMS(c)
	return x, nil
}

// UpsertMeshPeer records or refreshes what this daemon knows about a peer.
// The first call for a (sessionID, peerID) pair sets addr and first_seen;
// later calls only move addr/last_seen/status forward for that same peerID -
// callers that discover a *different* peerID claiming the same address are
// the TOFU-pinning decision point and must not call this to silently
// overwrite an existing peerID's row (see federation.Hub).
func (s *Store) UpsertMeshPeer(ctx context.Context, p MeshPeer) error {
	now := ms(time.Now())
	status := p.Status
	if status == "" {
		status = MeshPeerLinked
	}
	_, err := s.w.ExecContext(ctx, `
		INSERT INTO mesh_peers(session_id, peer_id, addr, first_seen, last_seen, status)
		VALUES(?,?,?,?,?,?)
		ON CONFLICT(session_id, peer_id) DO UPDATE SET addr=excluded.addr, last_seen=excluded.last_seen, status=excluded.status`,
		ids.Normalize(p.SessionID), p.PeerID, p.Addr, now, now, status)
	return err
}

// SetMeshPeerStatus updates only the status of a known peer (e.g. "unreachable"
// when a link drops), leaving addr/first_seen untouched.
func (s *Store) SetMeshPeerStatus(ctx context.Context, sessionID, peerID, status string) error {
	res, err := s.w.ExecContext(ctx, `UPDATE mesh_peers SET status=?, last_seen=? WHERE session_id=? AND peer_id=?`,
		status, ms(time.Now()), ids.Normalize(sessionID), peerID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return ErrNotFound
	}
	return nil
}

// GetMeshPeer returns what this daemon has recorded for one peer, or
// ErrNotFound if it has never seen that peer_id in this session - the check
// a TOFU pin violation is detected against.
func (s *Store) GetMeshPeer(ctx context.Context, sessionID, peerID string) (MeshPeer, error) {
	var p MeshPeer
	var fs, ls int64
	err := s.r.QueryRowContext(ctx, `SELECT session_id, peer_id, addr, first_seen, last_seen, status FROM mesh_peers WHERE session_id=? AND peer_id=?`,
		ids.Normalize(sessionID), peerID).Scan(&p.SessionID, &p.PeerID, &p.Addr, &fs, &ls, &p.Status)
	if errors.Is(err, sql.ErrNoRows) {
		return MeshPeer{}, ErrNotFound
	}
	if err != nil {
		return MeshPeer{}, err
	}
	p.FirstSeen, p.LastSeen = fromMS(fs), fromMS(ls)
	return p, nil
}

// ListMeshPeers returns every peer this daemon has recorded for a session,
// most recently seen first.
func (s *Store) ListMeshPeers(ctx context.Context, sessionID string) ([]MeshPeer, error) {
	rows, err := s.r.QueryContext(ctx, `SELECT session_id, peer_id, addr, first_seen, last_seen, status FROM mesh_peers WHERE session_id=? ORDER BY last_seen DESC`,
		ids.Normalize(sessionID))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MeshPeer
	for rows.Next() {
		var p MeshPeer
		var fs, ls int64
		if err := rows.Scan(&p.SessionID, &p.PeerID, &p.Addr, &fs, &ls, &p.Status); err != nil {
			return nil, err
		}
		p.FirstSeen, p.LastSeen = fromMS(fs), fromMS(ls)
		out = append(out, p)
	}
	return out, rows.Err()
}
