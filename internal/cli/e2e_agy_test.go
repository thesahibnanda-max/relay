//go:build !windows

package cli

import (
	"bufio"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/creack/pty"
)

// End-to-end tests of `relay agy` against testdata/fakeagy, which follows
// the real agy (1.2.12/1.2.13) where relay depends on it: its MCP config
// commands, spawning every configured MCP server per process, its log, its
// conversation database, its screens and dialogs, and how Enter answers a
// dialog. The PTY-based tests do not run on Windows (see e2e_test.go).

func (w *world) agyConfig() string {
	return filepath.Join(w.home, ".gemini", "config", "mcp_config.json")
}

func (w *world) writeAgyConfig(data string) {
	w.t.Helper()
	os.MkdirAll(filepath.Dir(w.agyConfig()), 0o755)
	if err := os.WriteFile(w.agyConfig(), []byte(data), 0o644); err != nil {
		w.t.Fatal(err)
	}
}

func (w *world) readAgyConfig() (string, bool) {
	data, err := os.ReadFile(w.agyConfig())
	return string(data), err == nil
}

// assertAgyPristine checks agy's config is byte-for-byte what the test
// wrote, and nothing else of relay's is left in agy's directories.
func (w *world) assertAgyPristine(orig string) {
	w.t.Helper()
	if data, ok := w.readAgyConfig(); !ok || data != orig {
		w.t.Fatalf("agy config = %q (exists %v), want the original %q", data, ok, orig)
	}
	for _, p := range []string{
		filepath.Join(w.home, ".gemini", "config", ".relay-agy"),
		filepath.Join(w.home, ".gemini", "antigravity-cli", "mcp", "relay"),
	} {
		if _, err := os.Stat(p); err == nil {
			w.t.Fatalf("%s left behind", p)
		}
	}
}

func (a *relayProc) events(ev string) []map[string]string {
	f, err := os.Open(a.logPath)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []map[string]string
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		var m map[string]string
		if json.Unmarshal(sc.Bytes(), &m) == nil && m["ev"] == ev {
			out = append(out, m)
		}
	}
	return out
}

func (a *relayProc) waitEvent(ev, key, contains string) map[string]string {
	a.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		for _, m := range a.events(ev) {
			if strings.Contains(m[key], contains) {
				return m
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	evs, _ := os.ReadFile(a.logPath)
	a.t.Fatalf("no %q event with %s containing %q\nevents:\n%s\nrelay output:\n%s", ev, key, contains, evs, a.output())
	return nil
}

func (a *relayProc) quitAgy() {
	a.t.Helper()
	a.pty.Write([]byte{0x04})
	time.Sleep(150 * time.Millisecond)
	a.pty.Write([]byte{0x04})
	select {
	case <-a.done:
	case <-time.After(15 * time.Second):
		a.t.Fatalf("relay agy did not exit on Ctrl+D twice; output:\n%s", a.output())
	}
	a.cmd.Wait()
}

func (w *world) startAgy(name string, extraEnv ...string) *relayProc {
	w.t.Helper()
	saved := w.env
	w.env = append(append([]string(nil), w.env...), extraEnv...)
	defer func() { w.env = saved }()
	a := w.start("agy", "developer", "--session=NEW_LOCAL", "--name="+name)
	a.waitEvent("mcp_ready", "tools", "relay_send")
	a.waitEvent("submit", "text", `You are "`+name+`"`) // the briefing, as the first turn via -i
	return a
}

func (w *world) messageState(id string) string {
	w.t.Helper()
	out, _, _ := w.runRelay("messages")
	for _, l := range strings.Split(out, "\n") {
		if f := strings.Fields(l); len(f) > 5 && f[0] == id {
			return f[5]
		}
	}
	return ""
}

func (w *world) send(to, body string) string {
	w.t.Helper()
	out, errs, code := w.runRelay("send", to, body)
	f := strings.Fields(out)
	if code != 0 || len(f) < 2 {
		w.t.Fatalf("relay send: %d %q %q", code, out, errs)
	}
	return f[1]
}

func (w *world) waitState(id, want string) {
	w.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		if w.messageState(id) == want {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	w.t.Fatalf("message %s is %q, want %q", id, w.messageState(id), want)
}

// The whole #50 path: a fresh agy install (0-byte config) gets relay's
// tools, the briefing arrives as the first turn, a message is typed,
// submitted, confirmed from agy's own log and answered - and on exit agy's
// config is exactly as it was.
func TestAgyE2EMessageRoundTripAndPristineExit(t *testing.T) {
	w := newWorld(t)
	w.writeAgyConfig("")
	bob := w.startAgy("bob")
	if !strings.Contains(bob.output(), "mcp(relay/*)") {
		t.Error("no hint about allowing relay's tools in agy")
	}
	id := w.send("bob", "What is 2 plus 3?")
	bob.waitEvent("answer", "text", "5")
	w.waitState(id, "acknowledged")
	bob.quitAgy()
	w.assertAgyPristine("")
	logs, _ := filepath.Glob(filepath.Join(w.home, ".gemini", "antigravity-cli", "log", "cli-*.log"))
	if len(logs) != 1 {
		t.Fatalf("agy's log was not handed back to agy: %v", logs)
	}
}

// Enter answers an agy dialog (confirmed live: it approved a shell command)
// and a paste into one is lost. A message arriving while one is open must
// wait, then arrive.
func TestAgyE2EHoldsMessagesWhileADialogIsOpen(t *testing.T) {
	w := newWorld(t)
	bob := w.startAgy("bob")
	w.send("bob", "RUN rm -rf build\nSLEEP 100")
	bob.waitEvent("dialog", "title", "Run this command?")
	id := w.send("bob", "What is 4 plus 4?")
	time.Sleep(2 * time.Second)
	if st := w.messageState(id); st != "dispatched" && st != "queued" {
		t.Fatalf("a message was delivered into the dialog: state %q", st)
	}
	if lost := bob.events("paste_lost"); len(lost) > 0 {
		t.Fatalf("relay typed into the dialog: %v", lost)
	}
	if ch := bob.events("choice"); len(ch) > 0 {
		t.Fatalf("relay answered the dialog: %v", ch)
	}
	bob.pty.Write([]byte("4")) // the user picks "No, cancel"
	time.Sleep(100 * time.Millisecond)
	bob.pty.Write([]byte("\r"))
	bob.waitEvent("answer", "text", "8")
	w.waitState(id, "acknowledged")
}

// Issue #50's Windows symptom: the tool leaves the message typed but not
// submitted. Relay sees agy never accepted it and presses Enter again -
// the message arrives exactly once.
func TestAgyE2EResubmitsAMessageTheToolDidNotTake(t *testing.T) {
	w := newWorld(t)
	bob := w.startAgy("bob", "FAKEAGY_SWALLOW_ENTER=1")
	id := w.send("bob", "What is 6 plus 7?")
	bob.waitEvent("enter_swallowed", "ev", "")
	bob.waitEvent("answer", "text", "13")
	w.waitState(id, "acknowledged")
	n := 0
	for _, s := range bob.submits() {
		if strings.Contains(s, "What is 6 plus 7?") {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("the message was submitted %d times, want exactly once", n)
	}
}

// Every agy process spawns every entry in agy's user-global config. Each
// relay agy agent's server must act as that agent only, and an agy relay
// did not start must get no tools at all.
func TestAgyE2EEachAgentOnlyGetsItsOwnTools(t *testing.T) {
	w := newWorld(t)
	bob := w.startAgy("bob", "FAKEAGY_ALLOW_MCP=1")
	session, _ := bob.identity()
	saved := w.env
	w.env = append(append([]string(nil), w.env...), "FAKEAGY_ALLOW_MCP=1")
	carol := w.start("agy", "reviewer", "--session="+session, "--name=carol")
	w.env = saved
	carol.waitEvent("mcp_ready", "tools", "relay_send")
	carol.waitEvent("submit", "text", `You are "carol"`)

	w.send("bob", `RELAY_CALL relay_whoami {}`)
	w.send("carol", `RELAY_CALL relay_whoami {}`)
	for _, c := range []struct {
		a    *relayProc
		name string
	}{{bob, "bob"}, {carol, "carol"}} {
		var who struct {
			Name string `json:"name"`
		}
		r := c.a.waitEvent("mcp", "tool", "relay_whoami")
		if json.Unmarshal([]byte(r["result"]), &who) != nil || who.Name != c.name {
			t.Fatalf("%s's agy acted as %q: %s", c.name, who.Name, r["result"])
		}
	}

	// A plain agy, not started by relay, while relay's entry is registered.
	logPath := filepath.Join(t.TempDir(), "plain.jsonl")
	cmd := exec.Command(filepath.Join(w.bin, binName("agy")))
	cmd.Env = append(append([]string(nil), w.env...), "FAKEAGY_LOG="+logPath)
	cmd.Dir = t.TempDir()
	p, err := pty.Start(cmd)
	if err != nil {
		t.Skip("no pty:", err)
	}
	defer func() { cmd.Process.Kill(); cmd.Wait(); p.Close() }()
	go func() {
		buf := make([]byte, 4096)
		for {
			if _, err := p.Read(buf); err != nil {
				return
			}
		}
	}()
	plain := &relayProc{t: t, logPath: logPath}
	ready := plain.waitEvent("mcp_ready", "server", "relay")
	if ready["tools"] != "" {
		t.Fatalf("an agy relay did not start got relay tools: %q", ready["tools"])
	}
}

// kill -9 of relay leaves its entry in agy's config; `relay gc` restores it.
func TestAgyE2ECrashLeftoversAreCleanedByGC(t *testing.T) {
	w := newWorld(t)
	orig := `{"mcpServers":{"mine":{"command":"echo"}}}`
	w.writeAgyConfig(orig)
	bob := w.startAgy("bob")
	if data, _ := w.readAgyConfig(); !strings.Contains(data, `"relay"`) {
		t.Fatalf("relay not registered: %s", data)
	}
	syscall.Kill(-bob.cmd.Process.Pid, syscall.SIGKILL) // relay and agy, no cleanup at all
	bob.cmd.Wait()
	out, _, _ := w.runRelay("doctor")
	if !strings.Contains(out, "relay gc") {
		t.Errorf("doctor does not point at relay gc for the leftover:\n%s", out)
	}
	if out, errs, code := w.runRelay("gc"); code != 0 || !strings.Contains(out, "agy MCP server relay") {
		t.Fatalf("relay gc: %d %q %q", code, out, errs)
	}
	w.assertAgyPristine(orig)
}

// /new starts a conversation without the relay briefing; relay notices from
// agy's log and briefs the model again.
func TestAgyE2EBriefsANewConversationAgain(t *testing.T) {
	w := newWorld(t)
	bob := w.startAgy("bob")
	bob.pty.Write([]byte("/new"))
	time.Sleep(200 * time.Millisecond)
	bob.pty.Write([]byte("\r"))
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		n := 0
		for _, s := range bob.submits() {
			if strings.Contains(s, `You are "bob"`) {
				n++
			}
		}
		if n == 2 {
			return
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("the new conversation was not briefed; submits: %q", bob.submits())
}

// An agent (via relay's MCP tools) hands work to agy; agy answers with
// relay_send; the answer reaches the asker.
func TestAgyE2EAnswersAnotherAgent(t *testing.T) {
	w := newWorld(t)
	bob := w.startAgy("bob", "FAKEAGY_ALLOW_MCP=1")
	session, _ := bob.identity()
	alice := w.start("claude", "orchestrator", "--session="+session, "--name=alice")
	alice.identity()
	aliceDir := w.agentDir(w.agents(session)["alice"].ID)
	waitForFile(t, filepath.Join(aliceDir, "ctl.sock"))
	s := w.shim(aliceDir)
	txt, isErr := s.call("relay_send", map[string]any{"to": "bob", "kind": "question",
		"body": `RELAY_CALL relay_send {"to":"alice","body":"the answer is 42"}`})
	if isErr {
		t.Fatalf("relay_send: %s", txt)
	}
	if r := bob.waitEvent("mcp", "tool", "relay_send"); r["error"] != "false" || !strings.Contains(r["result"], `"alice"`) {
		t.Fatalf("agy's relay_send failed: %v", r)
	}
	alice.waitSubmit("the answer is 42")
}
