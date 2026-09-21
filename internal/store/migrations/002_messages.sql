-- Inter-agent messages. The daemon is the durable router: a message lives
-- here from the moment it is accepted until it reaches a terminal state.
CREATE TABLE messages (
    id           TEXT PRIMARY KEY,
    session_id   TEXT NOT NULL REFERENCES sessions(id),
    from_agent   TEXT,                       -- agent id; NULL for the human user or Relay itself
    from_name    TEXT NOT NULL,              -- display name at send time ("user", "relay", or an agent name)
    from_role    TEXT NOT NULL DEFAULT '',
    to_agent     TEXT NOT NULL REFERENCES agents(id),
    to_name      TEXT NOT NULL,
    kind         TEXT NOT NULL,
    priority     INTEGER NOT NULL CHECK (priority BETWEEN 0 AND 3),
    thread       TEXT NOT NULL,
    reply_to     TEXT NOT NULL DEFAULT '',
    body         TEXT NOT NULL,
    hops         INTEGER NOT NULL DEFAULT 0,
    state        TEXT NOT NULL,
    detail       TEXT NOT NULL DEFAULT '',   -- why held / rejected / expired / undeliverable
    attempts     INTEGER NOT NULL DEFAULT 0, -- times pushed to the target agent
    created_at   INTEGER NOT NULL,
    updated_at   INTEGER NOT NULL,
    expires_at   INTEGER NOT NULL
);
CREATE INDEX messages_pending ON messages(to_agent, state);
CREATE INDEX messages_by_session ON messages(session_id, id);
CREATE INDEX messages_by_reply ON messages(reply_to) WHERE reply_to != '';
