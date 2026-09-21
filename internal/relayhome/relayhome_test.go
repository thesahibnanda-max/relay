package relayhome

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thesahibnanda-max/relay/internal/ids"
)

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
	for _, d := range []string{p.RunDir(), p.DataDir(), p.RawDir(), p.LogDir(), p.SessionsDir()} {
		st, err := os.Stat(d)
		if err != nil || !st.IsDir() || st.Mode().Perm() != 0o700 {
			t.Errorf("%s: %v %v", d, st, err)
		}
	}
}

func TestSocketPathFallsBackWhenTooLong(t *testing.T) {
	short := Paths{Root: "/tmp/rh"}
	if got := short.SocketPath(); got != "/tmp/rh/run/relayd.sock" {
		t.Errorf("short: %s", got)
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
	if st, _ := os.Stat(ld); st.Mode().Perm() != 0o700 {
		t.Fatalf("run dir mode %v", st.Mode().Perm())
	}
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
	if st, _ := os.Stat(real); st.Mode().Perm() != 0o700 {
		t.Fatalf("mode %v", st.Mode().Perm())
	}
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
	if st, err := os.Stat(filepath.Dir(sock)); err != nil || st.Mode().Perm() != 0o700 {
		t.Fatalf("socket dir must be private: %v %v", err, st)
	}
}
