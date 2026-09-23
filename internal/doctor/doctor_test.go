package doctor

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/thesahibnanda-max/relay/internal/proto"
	"github.com/thesahibnanda-max/relay/internal/relayhome"
	"github.com/thesahibnanda-max/relay/internal/store"
)

func testEnv(t *testing.T) (Env, string) {
	t.Helper()
	root := t.TempDir()
	home := filepath.Join(root, "home")
	os.MkdirAll(home, 0o755)
	paths := relayhome.Paths{Root: filepath.Join(root, "r")}
	if err := paths.Ensure(); err != nil {
		t.Fatal(err)
	}
	return Env{
		Paths: paths, Home: home, Cwd: filepath.Join(root, "proj"), GOOS: "linux",
		Getenv:      func(string) string { return "" },
		DaemonQuery: func(relayhome.Paths) (*proto.Status, error) { return nil, errors.New("down") },
		ToolVersion: func(b string) (string, string, error) { return "/usr/bin/" + b, b + " 1.0", nil },
		Version:     "v1",
	}, home
}

func find(cs []Check, name string) Check {
	for _, c := range cs {
		if c.Name == name {
			return c
		}
	}
	return Check{Name: name, Detail: "MISSING"}
}

func TestHealthyInstallation(t *testing.T) {
	env, _ := testEnv(t)
	cs := Run(env)
	if w := Worst(cs); w > Info {
		var b bytes.Buffer
		Render(&b, cs)
		t.Fatalf("a clean install should have no warnings:\n%s", b.String())
	}
	if find(cs, "relay home").Status != OK || find(cs, "zero footprint").Status != OK || find(cs, "daemon").Status != Info {
		t.Fatalf("%+v", cs)
	}
	var b bytes.Buffer
	Render(&b, cs)
	if !strings.Contains(b.String(), "0 warning(s), 0 failure(s)") {
		t.Fatal(b.String())
	}
}

func TestOpenPermissionsAreAFailure(t *testing.T) {
	env, _ := testEnv(t)
	os.Chmod(env.Paths.DataDir(), 0o755)
	os.WriteFile(env.Paths.DaemonLog(), []byte("x"), 0o644)
	c := find(Run(env), "relay home")
	if c.Status != Fail || !strings.Contains(c.Detail, "data") || !strings.Contains(c.Detail, "relayd.log") || c.Fix == "" {
		t.Fatalf("%+v", c)
	}
}

func TestDaemonVersionMismatchIsAFailureWithAFix(t *testing.T) {
	env, _ := testEnv(t)
	env.DaemonQuery = func(relayhome.Paths) (*proto.Status, error) {
		return &proto.Status{PID: 42, Proto: proto.Version - 1, Version: "old", StartedAt: time.Now()}, nil
	}
	if c := find(Run(env), "daemon"); c.Status != Fail || !strings.Contains(c.Fix, "relay daemon stop") {
		t.Fatalf("%+v", c)
	}
	env.DaemonQuery = func(relayhome.Paths) (*proto.Status, error) {
		return &proto.Status{PID: 42, Proto: proto.Version, Version: "v0", StartedAt: time.Now()}, nil
	}
	if c := find(Run(env), "daemon"); c.Status != Warn || !strings.Contains(c.Detail, "this binary is v1") {
		t.Fatalf("an older daemon build is worth a warning: %+v", c)
	}
	env.DaemonQuery = func(relayhome.Paths) (*proto.Status, error) {
		return &proto.Status{PID: 42, Proto: proto.Version, Version: "v1", StartedAt: time.Now()}, nil
	}
	if c := find(Run(env), "daemon"); c.Status != OK {
		t.Fatalf("%+v", c)
	}
}

func TestDatabaseChecks(t *testing.T) {
	env, _ := testEnv(t)
	s, err := store.Open(env.Paths.DBPath())
	if err != nil {
		t.Fatal(err)
	}
	if c := find(Run(env), "database"); c.Status != OK || !strings.Contains(c.Detail, "schema v") {
		t.Fatalf("%+v", c)
	}
	s.Close()
	// corrupt it
	os.Remove(env.Paths.DBPath() + "-wal")
	os.Remove(env.Paths.DBPath() + "-shm")
	data, _ := os.ReadFile(env.Paths.DBPath())
	for i := 4096; i < len(data) && i < 4096+2048; i++ {
		data[i] = 0xAB
	}
	os.WriteFile(env.Paths.DBPath(), data, 0o600)
	if c := find(Run(env), "database"); c.Status < Warn {
		t.Fatalf("a damaged database must not pass: %+v", c)
	}
}

func TestStaleRunDirsAreReportedNotRemoved(t *testing.T) {
	env, _ := testEnv(t)
	dir, err := env.Paths.CreateAgentDir("01M2XRVN1PVQZQC09VYHGYKKMQ")
	if err != nil {
		t.Fatal(err)
	}
	// make its owner "dead"
	os.WriteFile(filepath.Join(dir, "agent.json"), []byte(`{"agent_id":"x","pid":2147483646,"root":"`+env.Paths.Root+`"}`), 0o600)
	c := find(Run(env), "leftovers")
	if c.Status != Warn || c.Fix != "relay gc" {
		t.Fatalf("%+v", c)
	}
	if _, err := os.Stat(dir); err != nil {
		t.Fatal("doctor must only look")
	}
}

func TestFootprintScanFindsRelayRegistrations(t *testing.T) {
	env, home := testEnv(t)
	os.MkdirAll(filepath.Join(home, ".claude"), 0o755)
	os.MkdirAll(filepath.Join(home, ".codex"), 0o755)
	os.MkdirAll(env.Cwd, 0o755)
	os.WriteFile(filepath.Join(home, ".claude", "settings.json"), []byte(`{"theme":"dark"}`), 0o644)
	os.WriteFile(filepath.Join(home, ".codex", "config.toml"), []byte("[projects.\"/x/relay\"]\ntrust_level=\"trusted\"\n"), 0o644) // a path that merely contains "relay"
	if c := find(Run(env), "zero footprint"); c.Status != OK {
		t.Fatalf("innocent files must not be flagged: %+v", c)
	}
	os.WriteFile(filepath.Join(home, ".codex", "config.toml"), []byte("[mcp_servers.relay]\ncommand=\"/usr/bin/relay\"\n"), 0o644)
	os.WriteFile(filepath.Join(env.Cwd, ".mcp.json"), []byte(`{"mcpServers":{"relay":{"command":"relay","args":["mcp","--dir","/x"]}}}`), 0o644)
	os.WriteFile(filepath.Join(env.Cwd, "CLAUDE.md"), []byte("Use relay_send to talk to other agents"), 0o644)
	c := find(Run(env), "zero footprint")
	if c.Status != Warn || !strings.Contains(c.Detail, "config.toml") || !strings.Contains(c.Detail, ".mcp.json") || !strings.Contains(c.Detail, "CLAUDE.md") {
		t.Fatalf("%+v", c)
	}
}

func TestMissingToolsAndUnsupportedPlatform(t *testing.T) {
	env, _ := testEnv(t)
	env.ToolVersion = func(b string) (string, string, error) {
		if b == "codex" {
			return "", "", errors.New("nope")
		}
		return "/x/" + b, "", nil
	}
	cs := Run(env)
	if find(cs, "tool: codex").Status != Info || find(cs, "tool: claude").Status != OK {
		t.Fatalf("%+v", cs)
	}
	env.GOOS = "windows"
	if c := find(Run(env), "platform"); c.Status != Fail {
		t.Fatalf("%+v", c)
	}
	env.GOOS = "linux"
	env.Paths.Root = "/mnt/c/Users/x/.relay"
	if c := find(checkPlatform(env), "storage location"); c.Status != Warn {
		t.Fatalf("relay data on a Windows drive under WSL is a hazard: %+v", c)
	}
}
