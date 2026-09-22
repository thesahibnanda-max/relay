-- Multi-machine mesh (see MEMORY.md section 15). Each daemon's SQLite stays
-- single-writer and authoritative only for the agents connected to it: there
-- is no shared or replicated database across machines. These tables hold
-- only what this one daemon needs to remember about a mesh session it
-- participates in and the other daemons it has met.

-- One row per session this daemon currently or ever joined/hosted as a mesh
-- member. Local-only sessions never get a row here. join_secret is stored in
-- the clear (unlike agents.token_hash): unlike a resume token, which is only
-- ever presented and compared, a join secret must be re-embedded in fresh
-- --join blobs minted later by any current member, which a hash can't give
-- back.
CREATE TABLE mesh_sessions (
    session_id    TEXT PRIMARY KEY REFERENCES sessions(id),
    join_secret   TEXT NOT NULL,
    self_peer_id  TEXT NOT NULL,
    created_at    INTEGER NOT NULL
);

-- Other daemons in a mesh session, as last observed by THIS daemon. Nobody
-- else ever writes into this table. peer_id is TOFU-pinned: the first time a
-- peer_id is seen for a session it is recorded here, and it never silently
-- changes to a different addr's key - see federation.Hub's handshake.
CREATE TABLE mesh_peers (
    session_id  TEXT NOT NULL REFERENCES mesh_sessions(session_id),
    peer_id     TEXT NOT NULL,
    addr        TEXT NOT NULL,
    first_seen  INTEGER NOT NULL,
    last_seen   INTEGER NOT NULL,
    status      TEXT NOT NULL CHECK (status IN ('linked','unreachable')) DEFAULT 'unreachable',
    PRIMARY KEY (session_id, peer_id)
);
CREATE INDEX mesh_peers_by_session ON mesh_peers(session_id);
