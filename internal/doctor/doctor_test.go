package doctor

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
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
	if runtime.GOOS == "windows" {
		// os.Chmod cannot loosen a directory's real access on Windows (confirmed
		// elsewhere: it only toggles the read-only attribute) - there is no way
		// to simulate "someone widened this by hand" here without directly
		// manipulating the ACL, which doctor_windows.go's own true-positive
		// path is for. checkHome's Windows logic is exercised by every other
		// test in this file that expects it to report ok.
		t.Skip("os.Chmod cannot create an insecure ACL to detect on Windows")
	}
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
	// make its owner "dead" - json.Marshal, not a hand-built string: env.Paths.Root
	// contains backslashes on Windows, which broke this as raw JSON (confirmed
	// live: it silently made the directory look ownerless-but-fresh instead of
	// stale, so doctor correctly found nothing to warn about).
	info, _ := json.Marshal(relayhome.RunInfo{AgentID: "x", PID: 2147483646, Root: env.Paths.Root})
	os.WriteFile(filepath.Join(dir, "agent.json"), info, 0o600)
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
	os.MkdirAll(filepath.Join(home, ".copilot"), 0o755)
	os.MkdirAll(env.Cwd, 0o755)
	os.WriteFile(filepath.Join(home, ".claude", "settings.json"), []byte(`{"theme":"dark"}`), 0o644)
	os.WriteFile(filepath.Join(home, ".codex", "config.toml"), []byte("[projects.\"/x/relay\"]\ntrust_level=\"trusted\"\n"), 0o644) // a path that merely contains "relay"
	os.WriteFile(filepath.Join(home, ".copilot", "mcp-config.json"), []byte(`{"mcpServers":{}}`), 0o644)
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

func TestAgyRegistrationStates(t *testing.T) {
	env, home := testEnv(t)
	cfg := filepath.Join(home, ".gemini", "config", "mcp_config.json")
	os.MkdirAll(filepath.Dir(cfg), 0o755)
	write := func(s string) { os.WriteFile(cfg, []byte(s), 0o644) }
	ours := `{"mcpServers":{"relay":{"command":"/opt/relay/relay","args":["mcp","--from-env"]}}}`

	write(`{"mcpServers":{}}`)
	if c := find(Run(env), "agy registration"); c.Detail != "MISSING" {
		t.Fatalf("nothing registered must report nothing: %+v", c)
	}
	if c := find(Run(env), "zero footprint"); c.Status != OK {
		t.Fatalf("%+v", c)
	}

	write(`{"mcpServers":{"relay-01M2XRVN1PVQZQC09VYHGYKKMQ":{"command":"/opt/relay/relay","args":["mcp","--dir","/x"]}}}`)
	if c := find(Run(env), "agy registration"); c.Status != Warn || !strings.Contains(c.Fix, "relay gc") || !strings.Contains(c.Detail, "relay-01M2XRVN1PVQZQC09VYHGYKKMQ") {
		t.Fatalf("an older relay's entry must point at relay gc: %+v", c)
	}

	write(ours) // relay's entry, nobody running: a crash leftover
	if c := find(Run(env), "agy registration"); c.Status != Warn || !strings.Contains(c.Fix, "relay gc") {
		t.Fatalf("a leftover entry must point at relay gc: %+v", c)
	}

	leases := filepath.Join(home, ".gemini", "config", ".relay-agy", "leases")
	os.MkdirAll(leases, 0o700)
	os.WriteFile(filepath.Join(leases, "01K6AAAAAAAAAAAAAAAAAAAAAA.json"), []byte(fmt.Sprintf(`{"agent_id":"01K6AAAAAAAAAAAAAAAAAAAAAA","pid":%d}`, os.Getpid())), 0o600)
	if c := find(Run(env), "agy registration"); c.Status != OK || !strings.Contains(c.Detail, "in use by 1") {
		t.Fatalf("a live agent's entry is not a problem: %+v", c)
	}
	if c := find(Run(env), "zero footprint"); c.Status != OK {
		t.Fatalf("a live agy registration must not be flagged as a stray footprint: %+v", c)
	}

	write("{ // comment\n}")
	if c := find(Run(env), "agy registration"); c.Status != Warn || !strings.Contains(c.Fix, "agy mcp list") {
		t.Fatalf("an unparseable agy config must be reported: %+v", c)
	}
}

func TestFootprintScanCoversCopilotHooksGlob(t *testing.T) {
	env, home := testEnv(t)
	os.MkdirAll(filepath.Join(home, ".copilot", "hooks"), 0o755)
	os.WriteFile(filepath.Join(home, ".copilot", "hooks", "custom.json"), []byte(`{"hooks":{"preToolUse":[{"type":"command","bash":"echo hi"}]}}`), 0o644)
	if c := find(Run(env), "zero footprint"); c.Status != OK {
		t.Fatalf("an unrelated hook file must not be flagged: %+v", c)
	}
	os.WriteFile(filepath.Join(home, ".copilot", "hooks", "custom.json"),
		[]byte(`{"hooks":{"sessionStart":[{"type":"command","bash":"relay mcp --dir /x"}]}}`), 0o644)
	c := find(Run(env), "zero footprint")
	if c.Status != Warn || !strings.Contains(c.Detail, filepath.Join(home, ".copilot", "hooks", "custom.json")) {
		t.Fatalf("a glob-matched hooks file registering relay must be flagged: %+v", c)
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
	if find(cs, "tool: codex").Status != Info || find(cs, "tool: claude").Status != OK || find(cs, "tool: copilot").Status != OK {
		t.Fatalf("%+v", cs)
	}
	env.GOOS = "windows"
	if c := find(Run(env), "platform"); c.Status != OK {
		t.Fatalf("windows is a supported platform: %+v", c)
	}
	env.GOOS = "plan9"
	if c := find(Run(env), "platform"); c.Status != Fail {
		t.Fatalf("%+v", c)
	}
	env.GOOS = "linux"
	env.Paths.Root = "/mnt/c/Users/x/.relay"
	if c := find(checkPlatform(env), "storage location"); c.Status != Warn {
		t.Fatalf("relay data on a Windows drive under WSL is a hazard: %+v", c)
	}
}

// agy is checked against the versions relay was verified with.
func TestAgyVersionOutsideTheTestedRangeWarns(t *testing.T) {
	env, _ := testEnv(t)
	for ver, want := range map[string]Status{"9.9.9": Warn, "1.2.14": OK, "unknown": OK} {
		env.ToolVersion = func(b string) (string, string, error) { return "/x/" + b, ver, nil }
		if c := find(Run(env), "tool: agy"); c.Status != want {
			t.Fatalf("agy %s: %+v, want %v", ver, c, want)
		}
	}
}

// agy's own files are scanned too. Its settings may carry the permission
// rule a user allowed at relay's offer: that alone is not a stray footprint.
func TestFootprintCoversAgy(t *testing.T) {
	env, home := testEnv(t)
	settings := filepath.Join(home, ".gemini", "antigravity-cli", "settings.json")
	os.MkdirAll(filepath.Dir(settings), 0o755)
	os.WriteFile(settings, []byte(`{"permissions":{"allow":["mcp(relay/*)"]}}`), 0o644)
	if c := find(Run(env), "zero footprint"); c.Status != OK {
		t.Fatalf("the allowed permission rule was flagged: %+v", c)
	}
	gem := filepath.Join(home, ".gemini", "GEMINI.md")
	os.WriteFile(gem, []byte("You are \"bob\", working in a Relay session (id X)"), 0o644)
	if c := find(Run(env), "zero footprint"); c.Status != Warn || !strings.Contains(c.Detail, gem) {
		t.Fatalf("a briefing pasted into agy's GEMINI.md was not flagged: %+v", c)
	}
}
