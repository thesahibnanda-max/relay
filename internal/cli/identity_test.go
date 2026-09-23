package cli

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/thesahibnanda-max/relay/internal/relayhome"
)

func testPaths(t *testing.T) relayhome.Paths {
	t.Helper()
	return relayhome.Paths{Root: t.TempDir()}
}

func TestSaveAndLoadIdentityRoundTrips(t *testing.T) {
	paths := testPaths(t)
	saveIdentity(paths, "01ARZ3NDEKTSV4RRFFQ69G5FAV", "alice", "sekret", "claude")

	got, ok := loadIdentity(paths, "01ARZ3NDEKTSV4RRFFQ69G5FAV", "alice")
	if !ok || got.Token != "sekret" || got.Session != "01ARZ3NDEKTSV4RRFFQ69G5FAV" || got.Name != "alice" || got.Tool != "claude" {
		t.Fatalf("got %+v, ok=%v", got, ok)
	}
}

func TestLoadIdentityIsCaseInsensitiveOnName(t *testing.T) {
	paths := testPaths(t)
	saveIdentity(paths, "01ARZ3NDEKTSV4RRFFQ69G5FAV", "Alice", "sekret", "claude")
	if _, ok := loadIdentity(paths, "01ARZ3NDEKTSV4RRFFQ69G5FAV", "ALICE"); !ok {
		t.Fatal("expected a case-insensitive match")
	}
}

func TestLoadIdentityMissingFileIsNotAnError(t *testing.T) {
	paths := testPaths(t)
	if _, ok := loadIdentity(paths, "01ARZ3NDEKTSV4RRFFQ69G5FAV", "nobody"); ok {
		t.Fatal("expected no saved identity")
	}
}

func TestLoadIdentityCorruptFileIsIgnored(t *testing.T) {
	paths := testPaths(t)
	if err := os.MkdirAll(paths.IdentitiesDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(identityPath(paths, "01ARZ3NDEKTSV4RRFFQ69G5FAV", "alice"), []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, ok := loadIdentity(paths, "01ARZ3NDEKTSV4RRFFQ69G5FAV", "alice"); ok {
		t.Fatal("a corrupt file must never be treated as a valid identity")
	}
}

func TestSaveIdentityIgnoresAnEmptyToken(t *testing.T) {
	paths := testPaths(t)
	saveIdentity(paths, "01ARZ3NDEKTSV4RRFFQ69G5FAV", "alice", "", "claude")
	if _, err := os.Stat(identityPath(paths, "01ARZ3NDEKTSV4RRFFQ69G5FAV", "alice")); !os.IsNotExist(err) {
		t.Fatal("no file should be written for an empty token (a successful resume's Welcome carries none)")
	}
}

func TestDeleteIdentityRemovesTheFile(t *testing.T) {
	paths := testPaths(t)
	saveIdentity(paths, "01ARZ3NDEKTSV4RRFFQ69G5FAV", "alice", "sekret", "claude")
	deleteIdentity(paths, "01ARZ3NDEKTSV4RRFFQ69G5FAV", "alice")
	if _, ok := loadIdentity(paths, "01ARZ3NDEKTSV4RRFFQ69G5FAV", "alice"); ok {
		t.Fatal("identity should be gone after delete")
	}
	// deleting something that never existed is a harmless no-op
	deleteIdentity(paths, "01ARZ3NDEKTSV4RRFFQ69G5FAV", "nobody")
}

func TestSaveIdentityOverwritesAnExistingOne(t *testing.T) {
	paths := testPaths(t)
	saveIdentity(paths, "01ARZ3NDEKTSV4RRFFQ69G5FAV", "alice", "old", "claude")
	saveIdentity(paths, "01ARZ3NDEKTSV4RRFFQ69G5FAV", "alice", "new", "claude")
	got, ok := loadIdentity(paths, "01ARZ3NDEKTSV4RRFFQ69G5FAV", "alice")
	if !ok || got.Token != "new" {
		t.Fatalf("got %+v, ok=%v", got, ok)
	}
	if _, err := os.Stat(identityPath(paths, "01ARZ3NDEKTSV4RRFFQ69G5FAV", "alice") + ".tmp"); !os.IsNotExist(err) {
		t.Fatal("the temp file used for the atomic rename must not be left behind")
	}
}

func TestIdentityPathIsScopedByBothSessionAndName(t *testing.T) {
	paths := testPaths(t)
	saveIdentity(paths, "01ARZ3NDEKTSV4RRFFQ69G5FAV", "alice", "one", "claude")
	saveIdentity(paths, "01BRZ3NDEKTSV4RRFFQ69G5FAV", "alice", "two", "claude")
	a, _ := loadIdentity(paths, "01ARZ3NDEKTSV4RRFFQ69G5FAV", "alice")
	b, _ := loadIdentity(paths, "01BRZ3NDEKTSV4RRFFQ69G5FAV", "alice")
	if a.Token != "one" || b.Token != "two" {
		t.Fatalf("cross-session leakage: a=%+v b=%+v", a, b)
	}
	if identityPath(paths, "01ARZ3NDEKTSV4RRFFQ69G5FAV", "alice") == identityPath(paths, "01BRZ3NDEKTSV4RRFFQ69G5FAV", "alice") {
		t.Fatal("different sessions must not share a path")
	}
}

func TestIdentityPathIsUnderIdentitiesDir(t *testing.T) {
	paths := testPaths(t)
	got := identityPath(paths, "01ARZ3NDEKTSV4RRFFQ69G5FAV", "alice")
	if filepath.Dir(got) != paths.IdentitiesDir() {
		t.Fatalf("path %q is not under %q", got, paths.IdentitiesDir())
	}
}
