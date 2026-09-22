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

// MeshAgent is one agent owned by another daemon, as last gossiped to this
// one. Version is a monotonic counter (the owning daemon's own choice of
// scale - internal/federation uses that daemon's agents.last_seen_at in
// milliseconds, but this table only ever compares versions, never
// interprets them): applying a gossiped update only takes effect if its
// Version is strictly greater than what is already stored, which is what
// makes "keep the higher version" correct without needing to merge anything
// - there is exactly one daemon that can ever produce a given (AgentID,
// Version) pair.
type MeshAgent struct {
	AgentID      string
	SessionID    string
	OwnerPeer    string
	Name         string
	Tool         string
	Role         string
	Status       string
	CanInterrupt bool
	CanBroadcast bool
	LastSeenAt   time.Time
	Version      uint64
	Tombstoned   bool
}

// UpsertMeshAgentIfNewer applies a gossiped agent update, but only if it is
// newer than whatever this daemon already has for that agent id (or the
// agent is new to it). Returns applied=false if the update was stale and
// therefore ignored - the caller (internal/federation) uses this to decide
// whether a collision check and re-gossip are even necessary.
func (s *Store) UpsertMeshAgentIfNewer(ctx context.Context, a MeshAgent) (applied bool, err error) {
	tx, err := s.w.BeginTx(ctx, nil)
	if err != nil {
		return false, err
	}
	defer tx.Rollback()

	var existingVersion int64
	err = tx.QueryRowContext(ctx, `SELECT version FROM mesh_agents WHERE agent_id=?`, a.AgentID).Scan(&existingVersion)
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return false, err
	}
	if err == nil && uint64(existingVersion) >= a.Version {
		return false, tx.Commit() // stale or duplicate; nothing to apply
	}

	tomb := 0
	if a.Tombstoned {
		tomb = 1
	}
	appr, bcast := 0, 0
	if a.CanInterrupt {
		appr = 1
	}
	if a.CanBroadcast {
		bcast = 1
	}
	if _, err := tx.ExecContext(ctx, `
		INSERT INTO mesh_agents(agent_id, session_id, owner_peer, name, tool, role, status, can_interrupt, can_broadcast, last_seen_at, version, tombstoned)
		VALUES(?,?,?,?,?,?,?,?,?,?,?,?)
		ON CONFLICT(agent_id) DO UPDATE SET
			owner_peer=excluded.owner_peer, name=excluded.name, tool=excluded.tool, role=excluded.role,
			status=excluded.status, can_interrupt=excluded.can_interrupt, can_broadcast=excluded.can_broadcast,
			last_seen_at=excluded.last_seen_at, version=excluded.version, tombstoned=excluded.tombstoned`,
		a.AgentID, ids.Normalize(a.SessionID), a.OwnerPeer, a.Name, a.Tool, a.Role, a.Status, appr, bcast, ms(a.LastSeenAt), a.Version, tomb,
	); err != nil {
		return false, err
	}
	return true, tx.Commit()
}

// ListMeshAgents returns every non-tombstoned agent this daemon has learned
// about for a session, owned by any peer.
func (s *Store) ListMeshAgents(ctx context.Context, sessionID string) ([]MeshAgent, error) {
	rows, err := s.r.QueryContext(ctx, `
		SELECT agent_id, session_id, owner_peer, name, tool, role, status, can_interrupt, can_broadcast, last_seen_at, version, tombstoned
		FROM mesh_agents WHERE session_id=? AND tombstoned=0 ORDER BY name COLLATE NOCASE`, ids.Normalize(sessionID))
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []MeshAgent
	for rows.Next() {
		a, err := scanMeshAgent(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// GetMeshAgent returns what this daemon has recorded for one gossiped
// agent id, including a tombstoned one (callers that need to tell
// tombstoned from unknown use this rather than ListMeshAgents).
func (s *Store) GetMeshAgent(ctx context.Context, agentID string) (MeshAgent, error) {
	row := s.r.QueryRowContext(ctx, `
		SELECT agent_id, session_id, owner_peer, name, tool, role, status, can_interrupt, can_broadcast, last_seen_at, version, tombstoned
		FROM mesh_agents WHERE agent_id=?`, agentID)
	a, err := scanMeshAgent(row)
	if errors.Is(err, sql.ErrNoRows) {
		return MeshAgent{}, ErrNotFound
	}
	return a, err
}

// FindMeshAgentByName returns a non-tombstoned gossiped agent by exact
// (case-insensitive) name in a session - the remote-target fallback
// routeSend uses once a local name lookup has already failed (see
// internal/daemon's resolveTargets, M-mesh-4).
func (s *Store) FindMeshAgentByName(ctx context.Context, sessionID, name string) (MeshAgent, error) {
	row := s.r.QueryRowContext(ctx, `
		SELECT agent_id, session_id, owner_peer, name, tool, role, status, can_interrupt, can_broadcast, last_seen_at, version, tombstoned
		FROM mesh_agents WHERE session_id=? AND name=? COLLATE NOCASE AND tombstoned=0`, ids.Normalize(sessionID), name)
	a, err := scanMeshAgent(row)
	if errors.Is(err, sql.ErrNoRows) {
		return MeshAgent{}, ErrNotFound
	}
	return a, err
}

func scanMeshAgent(sc interface{ Scan(...any) error }) (MeshAgent, error) {
	var a MeshAgent
	var appr, bcast, tomb int
	var ls int64
	var version int64
	if err := sc.Scan(&a.AgentID, &a.SessionID, &a.OwnerPeer, &a.Name, &a.Tool, &a.Role, &a.Status, &appr, &bcast, &ls, &version, &tomb); err != nil {
		return MeshAgent{}, err
	}
	a.CanInterrupt, a.CanBroadcast, a.Tombstoned = appr != 0, bcast != 0, tomb != 0
	a.LastSeenAt = fromMS(ls)
	a.Version = uint64(version)
	return a, nil
}
