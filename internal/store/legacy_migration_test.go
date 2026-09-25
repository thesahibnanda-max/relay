package store

import (
	"database/sql"
	"path/filepath"
	"testing"
)

func TestMigrationVersion(t *testing.T) {
	cases := map[string]int{
		"001_init.sql":                    1,
		"002_messages.sql":                2,
		"006_drop_legacy_mesh_tables.sql": 6,
	}
	for name, want := range cases {
		got, err := migrationVersion(name)
		if err != nil || got != want {
			t.Errorf("migrationVersion(%q) = %d, %v; want %d, nil", name, got, err, want)
		}
	}
	if _, err := migrationVersion("nounderscore.sql"); err == nil {
		t.Error("expected an error for a filename with no _ separator")
	}
	if _, err := migrationVersion("abc_bad.sql"); err == nil {
		t.Error("expected an error for a non-numeric prefix")
	}
}

func TestSchemaVersion_IsTheHighestFilenamePrefixNotTheFileCount(t *testing.T) {
	// This project's migrations skip 3-5 (retired mesh-feature numbers), so
	// the count of files (2) must never be confused with the real version (6).
	if got := SchemaVersion(); got != 6 {
		t.Fatalf("SchemaVersion() = %d, want 6 (bump this test if a new migration is added)", got)
	}
}

func TestOpen_FreshDatabaseEndsUpAtCurrentSchemaVersion(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	v, err := StoredSchemaVersion(path)
	if err != nil {
		t.Fatal(err)
	}
	if v != SchemaVersion() {
		t.Fatalf("stored version = %d, want %d", v, SchemaVersion())
	}
}

// legacy mesh-era table definitions, exactly as they existed in the deleted
// 003_mesh.sql/004_mesh_agents.sql before the mesh feature was reverted
// (commit 10f2b8a) - reproduced here to build a database that faithfully
// matches a real one that lived through that era.
const legacyMeshSchema = `
CREATE TABLE mesh_sessions (
    session_id    TEXT PRIMARY KEY REFERENCES sessions(id),
    join_secret   TEXT NOT NULL,
    self_peer_id  TEXT NOT NULL,
    created_at    INTEGER NOT NULL
);
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
`

// buildLegacyMeshDatabase creates a SQLite file at path that reproduces
// exactly the field-observed state: schema from 001_init.sql/002_messages.sql
// plus the (since-deleted) mesh tables, PRAGMA user_version left at 5 (as a
// real database that ran the old 003-005 migrations would be), and one
// session with a mesh_sessions/mesh_peers row referencing it - the leftover
// FK chain that made relay gc fail with a FOREIGN KEY constraint error.
func buildLegacyMeshDatabase(t *testing.T, path, sessionID string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()

	for _, name := range []string{"001_init.sql", "002_messages.sql"} {
		body, err := migrationsFS.ReadFile("migrations/" + name)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := db.Exec(string(body)); err != nil {
			t.Fatalf("applying %s: %v", name, err)
		}
	}
	if _, err := db.Exec(legacyMeshSchema); err != nil {
		t.Fatalf("applying legacy mesh schema: %v", err)
	}
	if _, err := db.Exec(`PRAGMA user_version = 5`); err != nil {
		t.Fatal(err)
	}

	if _, err := db.Exec(`INSERT INTO sessions(id, name, kind, status, created_at) VALUES (?, 'demo', 'shared', 'active', 0)`, sessionID); err != nil {
		t.Fatalf("inserting session: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO mesh_sessions(session_id, join_secret, self_peer_id, created_at) VALUES (?, 'secret', 'peer-self', 0)`, sessionID); err != nil {
		t.Fatalf("inserting mesh_sessions row: %v", err)
	}
	if _, err := db.Exec(`INSERT INTO mesh_peers(session_id, peer_id, addr, first_seen, last_seen, status) VALUES (?, 'peer-other', '10.0.0.1:5555', 0, 0, 'linked')`, sessionID); err != nil {
		t.Fatalf("inserting mesh_peers row: %v", err)
	}
}

func tableExists(t *testing.T, path, name string) bool {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path+"?mode=ro")
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var got string
	err = db.QueryRow(`SELECT name FROM sqlite_master WHERE type='table' AND name=?`, name).Scan(&got)
	if err == sql.ErrNoRows {
		return false
	}
	if err != nil {
		t.Fatal(err)
	}
	return true
}

// TestOpen_DropsLegacyMeshTablesAndFixesDeleteSession is the core regression
// test for the reported "gc failed: constraint failed: FOREIGN KEY
// constraint failed (787)" bug: a database that lived through the reverted
// mesh feature must be healed automatically the next time it's opened, and a
// session that used to be un-deletable because of it must become deletable.
func TestOpen_DropsLegacyMeshTablesAndFixesDeleteSession(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.db")
	const sessionID = "01TESTLEGACYMESHSESSION00"
	buildLegacyMeshDatabase(t, path, sessionID)

	for _, name := range []string{"mesh_sessions", "mesh_peers", "mesh_agents"} {
		if !tableExists(t, path, name) {
			t.Fatalf("setup bug: %s should exist before Open", name)
		}
	}

	s, err := Open(path)
	if err != nil {
		t.Fatalf("Open on a legacy mesh-era database: %v", err)
	}
	defer s.Close()

	for _, name := range []string{"mesh_sessions", "mesh_peers", "mesh_agents"} {
		if tableExists(t, path, name) {
			t.Errorf("%s should have been dropped by migration 6", name)
		}
	}
	v, err := StoredSchemaVersion(path)
	if err != nil {
		t.Fatal(err)
	}
	if v != SchemaVersion() {
		t.Fatalf("stored version after healing = %d, want %d", v, SchemaVersion())
	}

	// This is the exact call that used to fail with:
	// "constraint failed: FOREIGN KEY constraint failed (787)"
	if err := s.DeleteSession(bg, sessionID); err != nil {
		t.Fatalf("DeleteSession after the legacy mesh tables were dropped: %v", err)
	}
	if _, err := s.GetSession(bg, sessionID); err == nil {
		t.Error("session should be gone after DeleteSession")
	}
}
