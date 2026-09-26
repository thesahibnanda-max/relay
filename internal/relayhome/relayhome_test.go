package relayhome

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/thesahibnanda-max/relay/internal/ids"
)

// wantPrivateDir checks the directory is owner-only via its mode bits. On
// Windows os.Chmod (and so Go's Mode()) never reflects real access - it
// always reads back 0777 for a normal directory regardless of its actual
// ACL - the real protection there is hardenMode's SetPrivateACL call, whose
// detection side is covered by doctor's own real-DACL test.
func wantPrivateDir(t *testing.T, d string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		return
	}
	st, err := os.Stat(d)
	if err != nil || !st.IsDir() || st.Mode().Perm() != 0o700 {
		t.Errorf("%s: %v %v", d, st, err)
	}
}

func TestResolveHonoursEnvAndEnsurePrivate(t *testing.T) {
	root := filepath.Join(t.TempDir(), "rh")
	t.Setenv("RELAY_HOME", root)
	p, err := Resolve()
	if err != nil || p.Root != root {
		t.Fatalf("%+v %v", p, err)
	}
	if err := p.Ensure(); err != nil {
		t.Fatal(err)
	}
	for _, d := range []string{p.RunDir(), p.DataDir(), p.RawDir(), p.LogDir(), p.SessionsDir(), p.IdentitiesDir()} {
		wantPrivateDir(t, d)
	}
}

func TestSocketPathFallsBackWhenTooLong(t *testing.T) {
	short := Paths{Root: "/tmp/rh"}
	if want, got := filepath.Join("/tmp/rh", "run", "relayd.sock"), short.SocketPath(); got != want {
		t.Errorf("short: %s, want %s", got, want)
	}
	long := Paths{Root: "/" + strings.Repeat("x", 120)}
	s := long.SocketPath()
	if len(s) > maxSocketPath || !strings.Contains(s, "relay-") {
		t.Errorf("long path not shortened: %s (%d)", s, len(s))
	}
	if long.SocketPath() != s {
		t.Error("fallback path must be stable across calls")
	}
	if (Paths{Root: "/" + strings.Repeat("y", 120)}).SocketPath() == s {
		t.Error("different roots must not share a socket")
	}
}

func TestAgentDirAndGC(t *testing.T) {
	p := Paths{Root: t.TempDir()}
	if err := p.Ensure(); err != nil {
		t.Fatal(err)
	}
	live, dead := ids.New(), ids.New()
	ld, err := p.CreateAgentDir(live)
	if err != nil {
		t.Fatal(err)
	}
	dd, _ := p.CreateAgentDir(dead)
	wantPrivateDir(t, ld)
	removed, err := p.GC(func(int) bool { return true })
	if err != nil || len(removed) != 0 {
		t.Fatalf("live processes keep their dirs: removed %v %v", removed, err)
	}
	removed, _ = p.GC(func(int) bool { return false })
	if len(removed) != 2 {
		t.Fatalf("both should go: %v", removed)
	}
	for _, d := range []string{ld, dd} {
		if _, err := os.Stat(d); err == nil {
			t.Fatalf("%s not removed", d)
		}
	}
	// other trees' dirs and unrelated files are left alone
	other := Paths{Root: t.TempDir()}
	other.Ensure()
	od, _ := other.CreateAgentDir(ids.New())
	p.GC(func(int) bool { return false })
	if _, err := os.Stat(od); err != nil {
		t.Fatal("must not touch another tree")
	}
}

func TestLongRootFallsBackToShortRunDir(t *testing.T) {
	long := filepath.Join(t.TempDir(), strings.Repeat("x", 90))
	p := Paths{Root: long}
	id := ids.New()
	d := p.AgentDir(id)
	if len(filepath.Join(d, "ctl.sock")) > maxSocketPath {
		t.Fatalf("socket path too long: %s", d)
	}
	if strings.HasPrefix(d, long) {
		t.Fatalf("expected the temp fallback, got %s", d)
	}
}

func TestVerifyPrivateDirRefusesSymlinksAndTightensMode(t *testing.T) {
	base := t.TempDir()
	real := filepath.Join(base, "real")
	os.Mkdir(real, 0o755)
	link := filepath.Join(base, "link")
	if err := os.Symlink(real, link); err != nil {
		t.Skip(err)
	}
	if err := VerifyPrivateDir(link); err == nil {
		t.Fatal("a symlink could point anywhere: refuse it")
	}
	if err := VerifyPrivateDir(real); err != nil {
		t.Fatal(err)
	}
	wantPrivateDir(t, real)
	if err := VerifyPrivateDir(filepath.Join(base, "missing")); err == nil {
		t.Fatal("missing")
	}
}

func TestLongRootSocketLivesInAPrivateDir(t *testing.T) {
	long := filepath.Join(t.TempDir(), strings.Repeat("y", 90))
	p := Paths{Root: long}
	sock := p.SocketPath()
	if strings.HasPrefix(sock, long) || len(sock) > maxSocketPath {
		t.Fatalf("unexpected fallback %s", sock)
	}
	if err := p.Ensure(); err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(filepath.Dir(sock))
	wantPrivateDir(t, filepath.Dir(sock))
}
