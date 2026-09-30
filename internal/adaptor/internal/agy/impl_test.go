package agy

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/thesahibnanda-max/relay/internal/adaptor/launch"
	"github.com/thesahibnanda-max/relay/internal/state"
)

func TestNameAndBinary(t *testing.T) {
	a := &AgyAdaptor{}
	if a.Name() != "agy" || a.Binary() != "agy" {
		t.Fatalf("got %q/%q", a.Name(), a.Binary())
	}
	if a.Env() != nil {
		t.Fatal("agy must not add --dangerously-skip-permissions or anything else to a real launch")
	}
}

// Which flags take a value was confirmed live (`agy <flag>=x changelog`).
func TestParseArgvFollowsAgysOwnFlagParsing(t *testing.T) {
	cases := []struct {
		args           []string
		nonInteractive bool
		resume         bool
	}{
		{nil, false, false},
		{[]string{"--model", "gemini-3"}, false, false},
		{[]string{"--mode", "agent"}, false, false}, // a flag value, not the "agent" subcommand
		{[]string{"-i", "help"}, false, false},      // an -i prompt, not the "help" subcommand
		{[]string{"-i=update the readme"}, false, false},
		{[]string{"--add-dir", "/x", "--effort", "low"}, false, false},
		{[]string{"-c"}, false, true},
		{[]string{"--continue"}, false, true},
		{[]string{"-c=false"}, false, false},
		{[]string{"--conversation", "abc"}, false, true},
		{[]string{"-p", "update"}, true, false}, // -p takes the prompt as its value
		{[]string{"--print=hi"}, true, false},
		{[]string{"--prompt", "hi"}, true, false},
		{[]string{"--version"}, true, false},
		{[]string{"-h"}, true, false},
		{[]string{"mcp", "list"}, true, false},
		{[]string{"--effort", "low", "changelog"}, true, false}, // a subcommand after flags
		{[]string{"install"}, true, false},
		{[]string{"--", "update"}, false, false},
		{[]string{"fix the bug"}, false, false}, // a positional that is not a subcommand
	}
	for _, c := range cases {
		a := parseArgv(c.args)
		if a.nonInteractive() != c.nonInteractive || a.resume != c.resume {
			t.Errorf("%q: nonInteractive=%v resume=%v, want %v/%v", c.args, a.nonInteractive(), a.resume, c.nonInteractive, c.resume)
		}
	}
}

func TestWithBriefing(t *testing.T) {
	b := "BRIEF\n\n" + launch.BriefingTurnTail // no task yet: it says so
	cases := []struct{ in, want []string }{
		{nil, []string{"-i", b}},
		{[]string{"--model", "m"}, []string{"-i", b, "--model", "m"}},
		{[]string{"-i", "do x"}, []string{"-i", "BRIEF\n\n---\n\ndo x"}},
		{[]string{"--prompt-interactive=do x"}, []string{"--prompt-interactive=BRIEF\n\n---\n\ndo x"}},
		{[]string{"--model", "m", "-i", ""}, []string{"--model", "m", "-i", b}},
		// A resumed conversation may be mid-task: never "there is no task".
		{[]string{"-c"}, []string{"-i", "BRIEF", "-c"}},
		{[]string{"--conversation", "abc"}, []string{"-i", "BRIEF", "--conversation", "abc"}},
	}
	for _, c := range cases {
		if got := withBriefing(c.in, "BRIEF"); !reflect.DeepEqual(got, c.want) {
			t.Errorf("withBriefing(%q) = %q, want %q", c.in, got, c.want)
		}
	}
}

func spec(t *testing.T, r *rig, args ...string) launch.Spec {
	t.Helper()
	return launch.Spec{
		AgentID: agentA, AgentName: "bob", Session: "S", RunDir: t.TempDir(), RelayExe: r.opt.RelayExe,
		RelayHome: t.TempDir(), ToolBin: r.opt.AgyBin, WithMCP: true, Briefing: "You are \"bob\".", UserArgs: args,
	}
}

func TestPrepareNonInteractiveIsPassthrough(t *testing.T) {
	r := newRig(t)
	a := &AgyAdaptor{}
	for _, args := range [][]string{{"-p", "hi"}, {"mcp", "list"}, {"--version"}, {"update"}} {
		plan, err := a.Prepare(spec(t, r, args...))
		if err != nil || !plan.Passthrough || !reflect.DeepEqual(plan.Args, args) {
			t.Fatalf("%q: plan %+v err %v", args, plan, err)
		}
	}
	if _, ok := r.read(); ok {
		t.Fatal("a passthrough launch touched agy's config")
	}
}

func TestPrepareRegistersBriefsAndCleanupRestores(t *testing.T) {
	r := newRig(t)
	r.write("")
	a := &AgyAdaptor{}
	sp := spec(t, r, "--model", "m")
	plan, err := a.Prepare(sp)
	if err != nil || plan.Passthrough || !plan.MCP || !plan.BriefingDelivered || !plan.VerifySubmit {
		t.Fatalf("plan %+v err %v", plan, err)
	}
	wantLog := filepath.Join(sp.RunDir, logFile)
	want := []string{"-i", sp.Briefing + "\n\n" + launch.BriefingTurnTail, "--log-file", wantLog, "--model", "m"}
	if !reflect.DeepEqual(plan.Args, want) || plan.ToolLog != wantLog {
		t.Fatalf("args %q log %q, want %q %q", plan.Args, plan.ToolLog, want, wantLog)
	}
	if !reflect.DeepEqual(plan.Env, []string{"RELAY_RUN_DIR=" + sp.RunDir}) {
		t.Fatalf("env %q", plan.Env)
	}
	r.assertOurEntry()
	os.WriteFile(wantLog, []byte("agy log line\n"), 0o644)
	if err := a.Cleanup(sp); err != nil {
		t.Fatal(err)
	}
	if data, ok := r.read(); !ok || data != "" {
		t.Fatalf("config %q exists=%v, want the original empty file", data, ok)
	}
	r.assertNoTrace()
	logs, _ := filepath.Glob(filepath.Join(r.home, ".gemini", "antigravity-cli", "log", "cli-*.log"))
	if len(logs) != 1 {
		t.Fatalf("agy's log was not handed back to agy's log directory: %v", logs)
	}
}

func TestPrepareKeepsTheUsersOwnLogFile(t *testing.T) {
	r := newRig(t)
	a := &AgyAdaptor{}
	sp := spec(t, r, "--log-file", "/tmp/mine.log")
	plan, _ := a.Prepare(sp)
	defer a.Cleanup(sp)
	if plan.ToolLog != "/tmp/mine.log" || strings.Count(strings.Join(plan.Args, " "), "--log-file") != 1 {
		t.Fatalf("plan %+v", plan)
	}
}

func TestPrepareWithoutSessionNeverTouchesAgy(t *testing.T) {
	r := newRig(t)
	a := &AgyAdaptor{}
	sp := spec(t, r)
	sp.WithMCP, sp.RunDir = false, ""
	plan, err := a.Prepare(sp)
	if err != nil || plan.MCP || plan.ToolLog != "" {
		t.Fatalf("plan %+v err %v", plan, err)
	}
	if _, ok := r.read(); ok {
		t.Fatal("agy's config was touched without a shared session")
	}
	if err := a.Cleanup(sp); err != nil {
		t.Fatal(err)
	}
}

func TestPrepareDegradesWhenRegistrationFails(t *testing.T) {
	r := newRig(t)
	r.write("{broken")
	a := &AgyAdaptor{}
	sp := spec(t, r)
	plan, err := a.Prepare(sp)
	if err != nil || plan.MCP || len(plan.Notes) == 0 || !strings.Contains(strings.Join(plan.Notes, " "), "could not register") {
		t.Fatalf("plan %+v err %v", plan, err)
	}
	if _, err := os.Stat(filepath.Join(sp.RunDir, markerFile)); err == nil {
		t.Fatal("marker written for a failed registration")
	}
	if err := a.Cleanup(sp); err != nil {
		t.Fatal(err)
	}
	if data, _ := r.read(); data != "{broken" {
		t.Fatalf("config changed: %q", data)
	}
}

func TestCleanupIsNoOpWithoutMarker(t *testing.T) {
	r := newRig(t)
	if err := (&AgyAdaptor{}).Cleanup(spec(t, r)); err != nil {
		t.Fatal(err)
	}
}

func TestPermissionNote(t *testing.T) {
	r := newRig(t)
	a := &AgyAdaptor{}
	sp := spec(t, r)
	plan, _ := a.Prepare(sp)
	a.Cleanup(sp)
	if !strings.Contains(strings.Join(plan.Notes, " "), "mcp(relay/*)") {
		t.Fatalf("no permission hint: %q", plan.Notes)
	}
	settings := filepath.Join(r.home, ".gemini", "antigravity-cli", "settings.json")
	os.MkdirAll(filepath.Dir(settings), 0o755)
	os.WriteFile(settings, []byte(`{"permissions":{"allow":["mcp(relay/*)"]}}`), 0o644)
	sp = spec(t, r)
	plan, _ = a.Prepare(sp)
	a.Cleanup(sp)
	if strings.Contains(strings.Join(plan.Notes, " "), "mcp(relay/*)") {
		t.Fatalf("hint shown although already allowed: %q", plan.Notes)
	}
}

// Screens captured live from agy 1.2.12/1.2.13.
func TestScreenRulesRecogniseAgysDialogs(t *testing.T) {
	rules := (&AgyAdaptor{}).ScreenRules()
	dialog := func(screen string) bool {
		sig, _, ok := state.FromScreen(strings.Split(screen, "\n"), rules)
		return ok && sig.State == state.Dialog
	}
	menu := "\nAllow calling this tool?\n> 1. Yes, allow tool call\n  4. No, deny tool call\n\n  ↑/↓ Navigate · tab Amend\nesc to cancel      Gemini 3.8 Flash · high"
	fileDialog := "/Users/x/hello.txt  +1\n   1 +  hi\n\nAllow creation of this file?\n> 1. Yes, allow creation\n  2. No, deny creation\n\n  ↑/↓ Navigate · tab Amend · f full diff\nesc to cancel"
	trust := "Accessing workspace:\n\n/Users/x\n\nDo you trust the contents of this project?\n\n> Yes, I trust this folder\n  No, exit\n\n  ↑/↓ Navigate · enter Confirm\n                     Gemini 3.8 Flash · high"
	exitPrompt := ">\n────────────────────\npress ctrl+d again to exit                     Gemini 3.8 Flash · high"
	for name, s := range map[string]string{"tool": menu, "file": fileDialog, "trust": trust, "exit": exitPrompt} {
		if !dialog(s) {
			t.Errorf("%s dialog not recognised", name)
		}
	}
	idle := "  42\n\n────────────────────\n>\n────────────────────\n? for shortcuts                     Gemini 3.8 Flash · high"
	if dialog(idle) {
		t.Error("the idle prompt was taken for a dialog")
	}
	busy := "⣾  Generating...\n────────────────────\n>\n────────────────────\nesc to cancel                     Gemini 3.8 Flash · high"
	if sig, _, ok := state.FromScreen(strings.Split(busy, "\n"), rules); !ok || sig.State != state.Busy {
		t.Errorf("the busy screen was not recognised: %+v %v", sig, ok)
	}
}
