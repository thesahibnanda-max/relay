-- Cross-daemon message hand-off (see MEMORY.md section 15, M-mesh-4). A
-- message crossing to a remote agent gets a row on BOTH daemons sharing one
-- id: origin='local' on the recipient's daemon is fully authoritative and
-- runs the existing state machine unchanged; origin='mirror' on the
-- sender's daemon holds the same immutable fields so hop-counting, reply_to
-- lookups and `relay wait` never need a live network round trip - only its
-- state/detail/rev are ever updated later, by an asynchronous receipt from
-- the owning daemon. rev is a per-row monotonic counter (bumped by every
-- Advance/expiry on an authoritative row) that lets a receiver ignore a
-- receipt older than one it already applied, however out of order a
-- reconnect redelivers them.
--
-- to_agent's hard foreign key to agents(id) must be relaxed: a mirror row's
-- to_agent names an agent that exists only in mesh_agents, never in this
-- daemon's own agents table. SQLite cannot drop a constraint in place, so
-- the whole table is rebuilt; nothing else has a foreign key into messages,
-- so this is safe without touching foreign_keys enforcement.
CREATE TABLE messages_new (
    id           TEXT PRIMARY KEY,
    session_id   TEXT NOT NULL REFERENCES sessions(id),
    from_agent   TEXT,                       -- agent id; NULL for the human user or Relay itself
    from_name    TEXT NOT NULL,              -- display name at send time ("user", "relay", or an agent name)
    from_role    TEXT NOT NULL DEFAULT '',
    from_peer    TEXT NOT NULL DEFAULT '',   -- owner peer of from_agent if it's remote; '' for local/user/relay
    to_agent     TEXT NOT NULL,
    to_name      TEXT NOT NULL,
    to_peer      TEXT NOT NULL DEFAULT '',   -- owner peer of to_agent if it's remote; '' for a local recipient
    kind         TEXT NOT NULL,
    priority     INTEGER NOT NULL CHECK (priority BETWEEN 0 AND 3),
    thread       TEXT NOT NULL,
    reply_to     TEXT NOT NULL DEFAULT '',
    body         TEXT NOT NULL,
    hops         INTEGER NOT NULL DEFAULT 0,
    state        TEXT NOT NULL,
    detail       TEXT NOT NULL DEFAULT '',   -- why held / rejected / expired / undeliverable
    attempts     INTEGER NOT NULL DEFAULT 0, -- times pushed to the target agent's own scheduler
    origin       TEXT NOT NULL DEFAULT 'local' CHECK (origin IN ('local','mirror')),
    rev          INTEGER NOT NULL DEFAULT 0,
    created_at   INTEGER NOT NULL,
    updated_at   INTEGER NOT NULL,
    expires_at   INTEGER NOT NULL
);
INSERT INTO messages_new (id, session_id, from_agent, from_name, from_role, to_agent, to_name, kind, priority, thread, reply_to, body, hops, state, detail, attempts, created_at, updated_at, expires_at)
    SELECT id, session_id, from_agent, from_name, from_role, to_agent, to_name, kind, priority, thread, reply_to, body, hops, state, detail, attempts, created_at, updated_at, expires_at FROM messages;
DROP TABLE messages;
ALTER TABLE messages_new RENAME TO messages;

CREATE INDEX messages_pending ON messages(to_agent, state);
CREATE INDEX messages_by_session ON messages(session_id, id);
CREATE INDEX messages_by_reply ON messages(reply_to) WHERE reply_to != '';
-- This daemon's mirror rows still awaiting their first receipt from a given
-- peer (rev=0), and its authoritative rows owed a receipt back to a given
-- peer - the two queries Hub.Resync's at-least-once resend runs per peer.
CREATE INDEX messages_mesh_outbound ON messages(to_peer, rev) WHERE origin='mirror' AND to_peer != '';
CREATE INDEX messages_mesh_inbound ON messages(from_peer, state) WHERE origin='local' AND from_peer != '';
