CREATE TABLE sessions (
    id          TEXT PRIMARY KEY,
    name        TEXT NOT NULL DEFAULT '',
    kind        TEXT NOT NULL CHECK (kind IN ('shared', 'solo')),
    status      TEXT NOT NULL CHECK (status IN ('active', 'ended')) DEFAULT 'active',
    created_at  INTEGER NOT NULL,
    ended_at    INTEGER
);

CREATE TABLE agents (
    id              TEXT PRIMARY KEY,
    session_id      TEXT NOT NULL REFERENCES sessions(id),
    name            TEXT NOT NULL,
    tool            TEXT NOT NULL,
    role            TEXT NOT NULL,
    role_source     TEXT NOT NULL DEFAULT '',
    token_hash      TEXT NOT NULL,
    status          TEXT NOT NULL CHECK (status IN ('connected', 'disconnected', 'exited')),
    approve_inbound INTEGER NOT NULL DEFAULT 0,
    pid             INTEGER NOT NULL DEFAULT 0,
    cwd             TEXT NOT NULL DEFAULT '',
    created_at      INTEGER NOT NULL,
    last_seen_at    INTEGER NOT NULL,
    exit_code       INTEGER,
    acked_seq       INTEGER NOT NULL DEFAULT 0,
    UNIQUE (session_id, name COLLATE NOCASE)
);
CREATE INDEX agents_by_session ON agents(session_id);

-- Structured (non-raw) events. Raw terminal bytes live in segment files.
CREATE TABLE events (
    agent_id  TEXT NOT NULL REFERENCES agents(id),
    seq       INTEGER NOT NULL,
    ts        INTEGER NOT NULL,
    type      TEXT NOT NULL,
    payload   TEXT NOT NULL DEFAULT '{}',
    PRIMARY KEY (agent_id, seq)
) WITHOUT ROWID;
