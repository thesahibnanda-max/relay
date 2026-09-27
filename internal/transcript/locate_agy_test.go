package transcript

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

func TestHasPathPrefix(t *testing.T) {
	cases := []struct {
		candidate, want string
		ok              bool
	}{
		{"/mnt/c/Users/x/relay", "/mnt/c/Users/x/relay", true},
		{"/mnt/c/Users/x/relayz", "/mnt/c/Users/x/relay", true}, // trailing printable framing noise (fallback only)
		{"/mnt/c/Users/x/rela", "/mnt/c/Users/x/relay", false},
		{"/other", "/mnt/c/Users/x/relay", false},
	}
	for _, c := range cases {
		if got := hasPathPrefix(c.candidate, c.want); got != c.ok {
			t.Errorf("hasPathPrefix(%q, %q) = %v, want %v", c.candidate, c.want, got, c.ok)
		}
	}
}

func TestFileURIToPath(t *testing.T) {
	if got := fileURIToPath("/mnt/c/Users/x/relay"); got != filepath.FromSlash("/mnt/c/Users/x/relay") {
		t.Errorf("unix path: got %q", got)
	}
	if got := fileURIToPath("/C:/Users/x/relay"); got != filepath.FromSlash("C:/Users/x/relay") {
		t.Errorf("windows drive path: got %q", got)
	}
}

// seedTrajectoryBlob creates a minimal database with the one table/row
// agyDBMatchesCwd reads (trajectory_metadata_blob, id="main"), embedding a
// file:// URI exactly as confirmed live against a real agy conversation
// database - including the trailing protobuf-framing noise that scan must
// tolerate (see hasPathPrefix's own doc comment). cwd is an OS-native path
// (as os.Getwd() would return it); it is converted to a forward-slash URI
// here, exactly as the real agy binary does, so this round-trips correctly
// through agyDBMatchesCwd's own filepath.FromSlash on every platform - a
// bare "/unix/style" literal embedded without this conversion previously
// passed on Unix by coincidence (FromSlash is a no-op there) but failed for
// real on Windows, where FromSlash actually rewrites the separators.
func seedTrajectoryBlob(t *testing.T, path, cwd string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err := db.Exec(`CREATE TABLE trajectory_metadata_blob (id TEXT PRIMARY KEY, data BLOB)`); err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`CREATE TABLE steps (idx INTEGER PRIMARY KEY, step_type INTEGER, status INTEGER, step_payload BLOB)`); err != nil {
		t.Fatal(err)
	}
	blob := append([]byte("\x12\x02\x01\x02\"$some-uuid-noise"), []byte("file://"+filepath.ToSlash(cwd)+"z\x94\x03\x88\x9c")...)
	if _, err := db.Exec(`INSERT INTO trajectory_metadata_blob (id, data) VALUES ('main', ?)`, blob); err != nil {
		t.Fatal(err)
	}
}

func seedUserMessage(t *testing.T, path, text string) {
	t.Helper()
	db, err := sql.Open("sqlite", "file:"+path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	payload := append([]byte("\x0a\x24noise-prefix\x12"), []byte(text)...)
	if _, err := db.Exec(`INSERT INTO steps (idx, step_type, status, step_payload) VALUES (0, ?, 3, ?)`, agyStepUserMessage, payload); err != nil {
		t.Fatal(err)
	}
}

func TestAgyDBMatchesCwd(t *testing.T) {
	dbPath := filepath.Join(t.TempDir(), "conv.db")
	wantCwd := filepath.Join(t.TempDir(), "relay") // an OS-native path, e.g. backslashes on Windows
	seedTrajectoryBlob(t, dbPath, wantCwd)

	if !agyDBMatchesCwd(dbPath, wantCwd) {
		t.Fatal("expected a match on the real cwd")
	}
	if agyDBMatchesCwd(dbPath, wantCwd+"2") {
		t.Fatal("must not match a longer, different directory name")
	}
	if agyDBMatchesCwd(dbPath, filepath.Join(t.TempDir(), "else")) {
		t.Fatal("must not match an unrelated cwd")
	}
}

func TestLocateAgyDisambiguatesByCwdAndBriefing(t *testing.T) {
	root := t.TempDir()
	wantCwd := filepath.Join(t.TempDir(), "project")

	// One candidate sharing the cwd, no name given: unambiguous.
	only := filepath.Join(root, "only.db")
	seedTrajectoryBlob(t, only, wantCwd)
	seedUserMessage(t, only, "hello")

	got := LocateAgy(AgyQuery{Root: root, Cwd: wantCwd})
	if got != only {
		t.Fatalf("got %q, want %q", got, only)
	}

	// A second agent joins the same cwd: now ambiguous without a name/session,
	// but the briefing text (containing the agent's name and session id)
	// disambiguates it.
	other := filepath.Join(root, "other.db")
	seedTrajectoryBlob(t, other, wantCwd)
	seedUserMessage(t, other, "Relay session S1, welcome \"fox\"")

	got = LocateAgy(AgyQuery{Root: root, Cwd: wantCwd, Name: "fox", Session: "S1"})
	if got != other {
		t.Fatalf("got %q, want %q (the one whose briefing names fox/S1)", got, other)
	}

	// Without disambiguating info and more than one candidate, refuse to guess.
	got = LocateAgy(AgyQuery{Root: root, Cwd: wantCwd})
	if got != "" {
		t.Fatalf("expected no match with 2 ambiguous candidates and no name, got %q", got)
	}
}

func TestLocateAgyIgnoresOldFiles(t *testing.T) {
	root := t.TempDir()
	cwd := filepath.Join(t.TempDir(), "x")
	old := filepath.Join(root, "old.db")
	seedTrajectoryBlob(t, old, cwd)
	seedUserMessage(t, old, "hi")

	got := LocateAgy(AgyQuery{Root: root, Cwd: cwd, Since: time.Now().Add(time.Hour)})
	if got != "" {
		t.Fatalf("a file older than Since must be ignored, got %q", got)
	}
}
