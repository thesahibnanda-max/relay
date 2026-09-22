-- Gossiped, read-only-here cache of agents owned by OTHER daemons in a mesh
-- session (see MEMORY.md section 15). The authoritative `agents` table is
-- completely untouched by this: it still holds only this daemon's own
-- agents. Nobody but this daemon ever writes a row here, and it only ever
-- writes what a peer told it about *its own* agents.
--
-- No UNIQUE(session_id, name) constraint: two daemons can briefly register
-- the same name before gossip crosses between them, and enforcing
-- uniqueness at insert time would make the outcome depend on arrival order,
-- which isn't deterministic across daemons. Name collisions are instead
-- resolved by application logic (internal/federation) comparing agent_id
-- (a ULID: lexicographically smaller = created first) - every daemon that
-- learns of both entries computes the same winner independently, and only
-- the loser's own owning daemon ever renames its own agent.
CREATE TABLE mesh_agents (
    agent_id      TEXT PRIMARY KEY,
    session_id    TEXT NOT NULL REFERENCES mesh_sessions(session_id),
    owner_peer    TEXT NOT NULL,
    name          TEXT NOT NULL,
    tool          TEXT NOT NULL,
    role          TEXT NOT NULL,
    status        TEXT NOT NULL,
    can_interrupt INTEGER NOT NULL DEFAULT 0,
    can_broadcast INTEGER NOT NULL DEFAULT 0,
    last_seen_at  INTEGER NOT NULL,
    version       INTEGER NOT NULL,
    tombstoned    INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX mesh_agents_by_session ON mesh_agents(session_id);
CREATE INDEX mesh_agents_by_session_name ON mesh_agents(session_id, name COLLATE NOCASE);
