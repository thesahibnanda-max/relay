package adaptor

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"

	"github.com/thesahibnanda-max/relay/internal/adaptor/launch"
)

func spec(t *testing.T, briefing string, mcp bool, user ...string) launch.Spec {
	t.Helper()
	return launch.Spec{AgentName: "fox", Session: "S", RunDir: t.TempDir(), RelayExe: "/opt/relay bin/relay", Briefing: briefing, WithMCP: mcp, UserArgs: user}
}

// wantPrivateFile checks the file is owner-only via its mode bits. On Windows
// os.Chmod (and so Go's Mode()) never reflects real access - it always reads
// back 0666 for a normal file regardless of its actual ACL - and this test's
// RunDir is a plain t.TempDir(), never hardened via relayhome.VerifyPrivateDir
// the way a real run directory is, so there is nothing meaningful to assert
// here on Windows.
func wantPrivateFile(t *testing.T, path string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		return
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if st.Mode().Perm() != 0o600 {
		t.Fatalf("mode %v", st.Mode().Perm())
	}
}

func prepare(t *testing.T, tool string, s launch.Spec) launch.Plan {
	t.Helper()
	f := NewAdaptorFactory()
	a, _ := f.ByName(tool)
	p, err := a.Prepare(s)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

func TestClaudeLaunchIsAdditiveAndKeepsUserArgsLast(t *testing.T) {
	s := spec(t, "BRIEF", true, "--model", "sonnet", "fix the bug")
	p := prepare(t, "claude", s)
	want := []string{"--mcp-config", filepath.Join(s.RunDir, "mcp.json"), "--allowedTools", "mcp__relay", "--append-system-prompt", "BRIEF", "--model", "sonnet", "fix the bug"}
	if !reflect.DeepEqual(p.Args, want) {
		t.Fatalf("args:\n got %q\nwant %q", p.Args, want)
	}
	if !p.MCP || !p.BriefingDelivered {
		t.Fatalf("%+v", p)
	}
	// the variadic flags must be followed by another flag, never by the user's positional prompt
	last := p.Args[len(p.Args)-len(s.UserArgs)-2]
	if last != "--append-system-prompt" {
		t.Fatalf("a single-valued flag must precede the user's args, got %q", last)
	}
	data, err := os.ReadFile(filepath.Join(s.RunDir, "mcp.json"))
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Servers map[string]struct {
			Type    string   `json:"type"`
			Command string   `json:"command"`
			Args    []string `json:"args"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	r := cfg.Servers["relay"]
	if r.Type != "stdio" || r.Command != "/opt/relay bin/relay" || !reflect.DeepEqual(r.Args, []string{"mcp", "--dir", s.RunDir}) || len(cfg.Servers) != 1 {
		t.Fatalf("mcp config: %+v", cfg)
	}
	wantPrivateFile(t, filepath.Join(s.RunDir, "mcp.json"))
}

func TestClaudeSoloWithRoleOnlyAddsThePrompt(t *testing.T) {
	s := spec(t, "ROLE", false, "--resume")
	p := prepare(t, "claude", s)
	if !reflect.DeepEqual(p.Args, []string{"--append-system-prompt", "ROLE", "--resume"}) || p.MCP {
		t.Fatalf("%q %+v", p.Args, p)
	}
	if entries, _ := os.ReadDir(s.RunDir); len(entries) != 0 {
		t.Fatalf("no files needed: %v", entries)
	}
	// no session and no role: the tool runs exactly as typed
	p = prepare(t, "claude", spec(t, "", false, "a", "b"))
	if !reflect.DeepEqual(p.Args, []string{"a", "b"}) {
		t.Fatalf("%q", p.Args)
	}
}

func TestClaudeDegradesInsteadOfClobberingUserPrompt(t *testing.T) {
	p := prepare(t, "claude", spec(t, "BRIEF", true, "--append-system-prompt", "mine"))
	if p.BriefingDelivered || strings.Contains(strings.Join(p.Args, " "), "BRIEF") || len(p.Notes) == 0 {
		t.Fatalf("the user's own flag must win and the briefing degrade to a bootstrap: %+v", p)
	}
	if !p.MCP {
		t.Fatal("MCP is unaffected")
	}
}

func TestNonInteractiveRunsAreUntouched(t *testing.T) {
	for tool, argsets := range map[string][][]string{
		"claude":  {{"-p", "hi"}, {"--print", "hi"}, {"mcp", "list"}, {"--version"}, {"doctor"}, {"--help"}},
		"codex":   {{"exec", "hi"}, {"mcp", "list"}, {"login"}, {"--help"}, {"queue", "--message", "x"}, {"review"}},
		"copilot": {{"-p", "hi"}, {"--prompt", "hi"}, {"mcp", "list"}, {"--version"}, {"login"}, {"--help"}},
	} {
		for _, args := range argsets {
			s := spec(t, "BRIEF", true, args...)
			p := prepare(t, tool, s)
			if !reflect.DeepEqual(p.Args, args) || p.MCP || p.BriefingDelivered {
				t.Errorf("%s %q must pass through untouched, got %+v", tool, args, p)
			}
			if entries, _ := os.ReadDir(s.RunDir); len(entries) != 0 {
				t.Errorf("%s %q wrote files: %v", tool, args, entries)
			}
		}
	}
}

func TestCodexUsesPerLaunchOverridesOnly(t *testing.T) {
	t.Setenv("CODEX_HOME", t.TempDir()) // an empty config
	s := spec(t, "Line1\n\"quoted\" \\ back", true, "resume", "--last")
	p := prepare(t, "codex", s)
	want := []string{
		"-c", `mcp_servers.relay.command="/opt/relay bin/relay"`,
		"-c", `mcp_servers.relay.args=["mcp", "--dir", ` + launch.TOMLString(s.RunDir) + `]`,
		"-c", `mcp_servers.relay.default_tools_approval_mode="approve"`,
		"-c", `developer_instructions="Line1\n\"quoted\" \\ back"`,
		"resume", "--last",
	}
	if !reflect.DeepEqual(p.Args, want) {
		t.Fatalf("args:\n got %q\nwant %q", p.Args, want)
	}
	if !p.MCP || !p.BriefingDelivered {
		t.Fatalf("%+v", p)
	}
	if entries, _ := os.ReadDir(s.RunDir); len(entries) != 0 {
		t.Fatalf("codex needs no files: %v", entries)
	}
	for _, a := range p.Args {
		for _, banned := range []string{"mcp add", "features", "--profile", "-p", "dangerously"} {
			if a == banned {
				t.Errorf("persistent or unsafe option %q", a)
			}
		}
	}
}

func TestCodexDoesNotReplaceUsersOwnInstructions(t *testing.T) {
	home := t.TempDir()
	t.Setenv("CODEX_HOME", home)
	os.WriteFile(filepath.Join(home, "config.toml"), []byte("model = \"x\"\ndeveloper_instructions = \"be terse\"\n[projects.\"/x\"]\ntrust_level=\"trusted\"\n"), 0o600)
	p := prepare(t, "codex", spec(t, "BRIEF", true))
	if p.BriefingDelivered || strings.Contains(strings.Join(p.Args, " "), "developer_instructions") || len(p.Notes) == 0 {
		t.Fatalf("must degrade to a bootstrap message: %+v", p)
	}
	// the same key inside a table is not top-level
	os.WriteFile(filepath.Join(home, "config.toml"), []byte("[tui]\ndeveloper_instructions = \"nested\"\n"), 0o600)
	if p := prepare(t, "codex", spec(t, "BRIEF", true)); !p.BriefingDelivered {
		t.Fatalf("a nested key is not the top-level setting: %+v", p)
	}
	// nor does an on-the-command-line override get replaced
	p = prepare(t, "codex", spec(t, "BRIEF", true, "-c", `developer_instructions="mine"`))
	if p.BriefingDelivered {
		t.Fatalf("%+v", p)
	}
}

func TestTOMLQuoting(t *testing.T) {
	for in, want := range map[string]string{
		`plain`:       `"plain"`,
		"a\"b":        `"a\"b"`,
		`back\slash`:  `"back\\slash"`,
		"tab\there":   `"tab\there"`,
		"bell\x07":    `"bell\u0007"`,
		"unicode é ✓": `"unicode é ✓"`,
	} {
		if got := launch.TOMLString(in); got != want {
			t.Errorf("%q: got %s want %s", in, got, want)
		}
	}
}

func TestScreenRulesExist(t *testing.T) {
	f := NewAdaptorFactory()
	for _, n := range f.Names() {
		a, _ := f.ByName(n)
		if len(a.ScreenRules()) == 0 {
			t.Errorf("%s has no dialog rules: relay would type into permission prompts", n)
		}
	}
}

func TestClaudeHooksArePerLaunchAndKeepUserSettings(t *testing.T) {
	s := spec(t, "BRIEF", true, "--model", "sonnet", "do it")
	s.WithHooks = true
	p := prepare(t, "claude", s)
	settings := filepath.Join(s.RunDir, "settings.json")
	want := []string{"--mcp-config", filepath.Join(s.RunDir, "mcp.json"), "--allowedTools", "mcp__relay", "--settings", settings, "--append-system-prompt", "BRIEF", "--model", "sonnet", "do it"}
	if !reflect.DeepEqual(p.Args, want) {
		t.Fatalf("args:\n got %q\nwant %q", p.Args, want)
	}
	if !p.Hooks {
		t.Fatal("hooks flag")
	}
	data, err := os.ReadFile(settings)
	if err != nil || !strings.Contains(string(data), "/opt/relay bin/relay") || !strings.Contains(string(data), "PostToolUse") {
		t.Fatalf("%v %s", err, data)
	}
	wantPrivateFile(t, settings)

	// the user's own --settings would be replaced by ours (or ours by theirs): keep theirs, drop hooks
	s = spec(t, "BRIEF", true, "--settings", "/my/settings.json")
	s.WithHooks = true
	p = prepare(t, "claude", s)
	if p.Hooks || strings.Count(strings.Join(p.Args, " "), "--settings") != 1 || len(p.Notes) == 0 {
		t.Fatalf("degrade: %+v", p)
	}
	if _, err := os.Stat(filepath.Join(s.RunDir, "settings.json")); err == nil {
		t.Fatal("no settings file when hooks are off")
	}
	// Codex has no hook registration (its rollout file is followed instead)
	s = spec(t, "", true)
	s.WithHooks = true
	t.Setenv("CODEX_HOME", t.TempDir())
	if p := prepare(t, "codex", s); p.Hooks {
		t.Fatal("codex hooks are not used")
	}
	// nor does Copilot (its events.jsonl is followed instead)
	s = spec(t, "", true)
	s.WithHooks = true
	if p := prepare(t, "copilot", s); p.Hooks {
		t.Fatal("copilot hooks are not used")
	}
}

func TestCopilotLaunchIsAdditiveAndKeepsUserArgsLast(t *testing.T) {
	s := spec(t, "BRIEF", true, "--model", "gpt-5.4", "fix the bug")
	p := prepare(t, "copilot", s)
	mcpPath := filepath.Join(s.RunDir, "mcp.json")
	want := []string{"--additional-mcp-config", "@" + mcpPath, "--allow-tool=relay", "--model", "gpt-5.4", "fix the bug"}
	if !reflect.DeepEqual(p.Args, want) {
		t.Fatalf("args:\n got %q\nwant %q", p.Args, want)
	}
	if !p.MCP || p.BriefingDelivered || len(p.Notes) == 0 {
		t.Fatalf("copilot never delivers a briefing via a flag: %+v", p)
	}
	if strings.Contains(strings.Join(p.Args, " "), "BRIEF") {
		t.Fatalf("the briefing must not appear on the command line: %q", p.Args)
	}
	data, err := os.ReadFile(mcpPath)
	if err != nil {
		t.Fatal(err)
	}
	var cfg struct {
		Servers map[string]struct {
			Type    string   `json:"type"`
			Command string   `json:"command"`
			Args    []string `json:"args"`
			Tools   []string `json:"tools"`
		} `json:"mcpServers"`
	}
	if err := json.Unmarshal(data, &cfg); err != nil {
		t.Fatal(err)
	}
	r := cfg.Servers["relay"]
	if r.Type != "local" || r.Command != "/opt/relay bin/relay" || !reflect.DeepEqual(r.Args, []string{"mcp", "--dir", s.RunDir}) ||
		!reflect.DeepEqual(r.Tools, []string{"*"}) || len(cfg.Servers) != 1 {
		t.Fatalf("mcp config: %+v", cfg)
	}
	wantPrivateFile(t, mcpPath)
}

func TestCopilotSoloWithNoMCPAddsNothing(t *testing.T) {
	s := spec(t, "BRIEF", false, "--resume")
	p := prepare(t, "copilot", s)
	if !reflect.DeepEqual(p.Args, []string{"--resume"}) || p.MCP || p.BriefingDelivered || len(p.Notes) == 0 {
		t.Fatalf("%q %+v", p.Args, p)
	}
	if entries, _ := os.ReadDir(s.RunDir); len(entries) != 0 {
		t.Fatalf("no files needed: %v", entries)
	}
	// no MCP and no briefing: the tool runs exactly as typed
	p = prepare(t, "copilot", spec(t, "", false, "a", "b"))
	if !reflect.DeepEqual(p.Args, []string{"a", "b"}) || len(p.Notes) != 0 {
		t.Fatalf("%q %+v", p.Args, p)
	}
}

func TestCopilotDegradesInsteadOfClobberingUserMCPFlag(t *testing.T) {
	s := spec(t, "BRIEF", true, "--additional-mcp-config", "@mine.json")
	p := prepare(t, "copilot", s)
	if p.MCP || len(p.Notes) == 0 {
		t.Fatalf("the user's own flag must win: %+v", p)
	}
	if entries, _ := os.ReadDir(s.RunDir); len(entries) != 0 {
		t.Fatalf("no mcp.json when degraded: %v", entries)
	}
}

// Copilot has no CLI flag for a system prompt at all, so - unlike
// Claude/Codex, where this only happens on conflict - the briefing degrades
// to a bootstrap message unconditionally, for every combination of MCP and
// user args.
func TestCopilotBriefingAlwaysDegradesToBootstrap(t *testing.T) {
	for _, mcp := range []bool{true, false} {
		for _, args := range [][]string{nil, {"--model", "gpt-5.4"}, {"fix the bug"}} {
			p := prepare(t, "copilot", spec(t, "BRIEF", mcp, args...))
			if p.BriefingDelivered || len(p.Notes) == 0 {
				t.Errorf("mcp=%v args=%q: briefing must always degrade, got %+v", mcp, args, p)
			}
			if strings.Contains(strings.Join(p.Args, " "), "BRIEF") {
				t.Errorf("mcp=%v args=%q: briefing leaked onto the command line: %q", mcp, args, p.Args)
			}
		}
	}
}
