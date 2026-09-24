package cli

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/creack/pty"

	"github.com/thesahibnanda-max/relay/internal/proto"
	"github.com/thesahibnanda-max/relay/internal/relayhome"
)

// These tests run the real relay binary with a scripted fake tool standing in
// for claude and codex, wired up exactly as in production: relay resolves
// `claude`/`codex` on PATH, adds its per-launch flags, and starts the tool in
// a PTY.

var (
	buildOnce sync.Once
	binDir    string
	buildErr  error
)

func buildBinaries(t *testing.T) string {
	t.Helper()
	if testing.Short() {
		t.Skip("skipping binary e2e in -short mode")
	}
	buildOnce.Do(func() {
		binDir, buildErr = os.MkdirTemp("", "relaye2e")
		if buildErr != nil {
			return
		}
		for out, pkg := range map[string]string{"relay": "../..", "fakeagent": "../../testdata/fakeagent"} {
			if b, err := exec.Command("go", "build", "-o", filepath.Join(binDir, out), pkg).CombinedOutput(); err != nil {
				buildErr = &buildFailure{string(b), err}
				return
			}
		}
		// the fake tool is installed under the names of the real ones
		for _, name := range []string{"claude", "codex"} {
			if err := os.Symlink(filepath.Join(binDir, "fakeagent"), filepath.Join(binDir, name)); err != nil {
				buildErr = err
				return
			}
		}
	})
	if buildErr != nil {
		t.Skip("cannot build binaries:", buildErr)
	}
	return binDir
}

type buildFailure struct {
	out string
	err error
}

func (b *buildFailure) Error() string { return b.err.Error() + ": " + b.out }

// world is an isolated HOME + RELAY_HOME.
type world struct {
	t     *testing.T
	home  string // fake $HOME holding fake ~/.claude and ~/.codex
	relay string // RELAY_HOME
	bin   string
	env   []string
}

func newWorld(t *testing.T) *world {
	t.Helper()
	bin := buildBinaries(t)
	root, err := os.MkdirTemp("", "rw")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	w := &world{t: t, home: filepath.Join(root, "home"), relay: filepath.Join(root, "r"), bin: bin}
	for path, content := range map[string]string{
		".claude/settings.json": `{"theme":"dark"}`,
		".claude/CLAUDE.md":     "my notes",
		".codex/config.toml":    "model = \"x\"\n",
		".mcp.json":             `{"mcpServers":{}}`,
	} {
		full := filepath.Join(w.home, path)
		os.MkdirAll(filepath.Dir(full), 0o755)
		os.WriteFile(full, []byte(content), 0o644)
	}
	w.env = append(os.Environ(),
		"HOME="+w.home, "CODEX_HOME="+filepath.Join(w.home, ".codex"),
		"RELAY_HOME="+w.relay, "PATH="+bin+string(os.PathListSeparator)+os.Getenv("PATH"),
		"RELAY_ACTIVE=", // never inherit the nesting marker from an outer relay
	)
	t.Cleanup(func() {
		if t.Failed() {
			if b, err := os.ReadFile(relayhome.Paths{Root: w.relay}.DaemonLog()); err == nil {
				t.Logf("daemon log:\n%s", b)
			}
		}
	})
	t.Cleanup(func() { // stop the daemon this world started
		cmd := exec.Command(filepath.Join(bin, "relay"), "daemon", "stop")
		cmd.Env = w.env
		cmd.Run()
	})
	return w
}

// footprint hashes every file under the fake tool config dirs and project files.
func (w *world) footprint() map[string]string {
	out := map[string]string{}
	filepath.WalkDir(w.home, func(p string, d fs.DirEntry, err error) error {
		if err == nil && !d.IsDir() {
			b, _ := os.ReadFile(p)
			sum := sha256.Sum256(b)
			out[strings.TrimPrefix(p, w.home)] = hex.EncodeToString(sum[:])
		}
		return nil
	})
	return out
}

// agent is one relay process running under a PTY.
type relayProc struct {
	t       *testing.T
	cmd     *exec.Cmd
	pty     *os.File
	logPath string
	mu      sync.Mutex
	out     bytes.Buffer
	done    chan struct{}
}

func (w *world) start(tool string, args ...string) *relayProc {
	w.t.Helper()
	a := &relayProc{t: w.t, logPath: filepath.Join(w.t.TempDir(), tool+".jsonl"), done: make(chan struct{})}
	a.cmd = exec.Command(filepath.Join(w.bin, "relay"), append([]string{tool}, args...)...)
	a.cmd.Env = append(append([]string(nil), w.env...), "FAKE_LOG="+a.logPath, "FAKE_BUSY_MS=300")
	a.cmd.Dir = w.t.TempDir()
	var err error
	a.pty, err = pty.StartWithSize(a.cmd, &pty.Winsize{Rows: 30, Cols: 100})
	if err != nil {
		w.t.Skip("no pty:", err)
	}
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := a.pty.Read(buf)
			a.mu.Lock()
			a.out.Write(buf[:n])
			a.mu.Unlock()
			if err != nil {
				close(a.done)
				return
			}
		}
	}()
	w.t.Cleanup(func() {
		a.pty.Write([]byte("\x03\x03"))
		select {
		case <-a.done:
		case <-time.After(3 * time.Second):
			a.cmd.Process.Kill()
		}
		a.cmd.Wait()
		a.pty.Close()
	})
	return a
}

func (a *relayProc) output() string {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.out.String()
}

func (a *relayProc) waitOutput(sub string) string {
	a.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if o := a.output(); strings.Contains(o, sub) {
			return o
		}
		time.Sleep(20 * time.Millisecond)
	}
	a.t.Fatalf("timed out waiting for %q in relay output:\n%s", sub, a.output())
	return ""
}

// submits returns the texts the fake tool received as submitted prompts.
func (a *relayProc) submits() []string {
	f, err := os.Open(a.logPath)
	if err != nil {
		return nil
	}
	defer f.Close()
	var out []string
	sc := bufio.NewScanner(f)
	sc.Buffer(nil, 1<<20)
	for sc.Scan() {
		var m map[string]string
		if json.Unmarshal(sc.Bytes(), &m) == nil && m["ev"] == "submit" {
			out = append(out, m["text"])
		}
	}
	if err := sc.Err(); err != nil {
		a.t.Fatalf("reading %s: %v", a.logPath, err)
	}
	return out
}

func (a *relayProc) waitSubmit(contains string) string {
	a.t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for time.Now().Before(deadline) {
		for _, s := range a.submits() {
			if strings.Contains(s, contains) {
				return s
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	a.t.Fatalf("the tool never received a prompt containing %q; got %q\nrelay output:\n%s", contains, a.submits(), a.output())
	return ""
}

var sessionLine = regexp.MustCompile(`relay: session ([0-9A-Z]{26}) · (?:you are|welcome back,) (\S+) \((\S+)\)`)

func (a *relayProc) identity() (session, name string) {
	a.t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if m := sessionLine.FindStringSubmatch(a.output()); m != nil {
			return m[1], m[2]
		}
		time.Sleep(20 * time.Millisecond)
	}
	a.t.Fatalf("no identity line in %q", a.output())
	return "", ""
}

// shim drives `relay mcp` the way a model's MCP client does.
type shim struct {
	t   *testing.T
	in  io.WriteCloser
	out *bufio.Reader
	n   int
}

func (w *world) shim(dir string) *shim {
	w.t.Helper()
	cmd := exec.Command(filepath.Join(w.bin, "relay"), "mcp", "--dir", dir)
	cmd.Env = w.env
	in, _ := cmd.StdinPipe()
	out, _ := cmd.StdoutPipe()
	if err := cmd.Start(); err != nil {
		w.t.Fatal(err)
	}
	w.t.Cleanup(func() { in.Close(); cmd.Wait() })
	s := &shim{t: w.t, in: in, out: bufio.NewReader(out)}
	s.rpc("initialize", map[string]any{"protocolVersion": "2025-06-18", "capabilities": map[string]any{}, "clientInfo": map[string]any{"name": "test"}})
	return s
}

func (s *shim) rpc(method string, params any) map[string]any {
	s.t.Helper()
	s.n++
	b, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": s.n, "method": method, "params": params})
	s.in.Write(append(b, '\n'))
	line, err := s.out.ReadBytes('\n')
	if err != nil {
		s.t.Fatalf("shim closed: %v", err)
	}
	var m map[string]any
	json.Unmarshal(line, &m)
	return m
}

// call runs a tool and returns its text and error flag.
func (s *shim) call(tool string, args map[string]any) (string, bool) {
	s.t.Helper()
	m := s.rpc("tools/call", map[string]any{"name": tool, "arguments": args})
	res, ok := m["result"].(map[string]any)
	if !ok {
		s.t.Fatalf("protocol error: %v", m)
	}
	txt := res["content"].([]any)[0].(map[string]any)["text"].(string)
	isErr, _ := res["isError"].(bool)
	return txt, isErr
}

func (w *world) agentDir(agentID string) string {
	return relayhome.Paths{Root: w.relay}.AgentDir(agentID)
}

func (w *world) agents(session string) map[string]proto.AgentInfo {
	w.t.Helper()
	c := proto.HTTPClient(relayhome.Paths{Root: w.relay}.SocketPath())
	var s proto.SessionInfo
	if _, err := getJSON(c, "/v1/admin/sessions/"+session+"?all=1", &s); err != nil {
		w.t.Fatal(err)
	}
	m := map[string]proto.AgentInfo{}
	for _, a := range s.Agents {
		m[a.Name] = a
	}
	return m
}

func (w *world) runRelay(args ...string) (string, string, int) {
	w.t.Helper()
	cmd := exec.Command(filepath.Join(w.bin, "relay"), args...)
	cmd.Env = w.env
	var o, e bytes.Buffer
	cmd.Stdout, cmd.Stderr = &o, &e
	err := cmd.Run()
	code := 0
	if ee, ok := err.(*exec.ExitError); ok {
		code = ee.ExitCode()
	}
	return o.String(), e.String(), code
}

// waitForFileErr polls up to 10 s for path to exist and returns the last stat error.
func waitForFileErr(path string) error {
	deadline := time.Now().Add(10 * time.Second)
	for {
		_, err := os.Stat(path)
		if err == nil || time.Now().After(deadline) {
			return err
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func waitForFile(t *testing.T, path string) {
	t.Helper()
	if err := waitForFileErr(path); err != nil {
		t.Fatalf("run dir file %s: %v", filepath.Base(path), err)
	}
}

func TestTwoAgentsCollaborateAndLeaveNoFootprint(t *testing.T) {
	w := newWorld(t)
	before := w.footprint()

	alice := w.start("claude", "orchestrator", "--session=NEW_LOCAL", "--name=alice")
	session, name := alice.identity()
	if name != "alice" {
		t.Fatalf("name %q", name)
	}
	bob := w.start("codex", "developer", "--session="+session, "--name=bob")
	bob.identity()

	agents := w.agents(session)
	aliceDir, bobDir := w.agentDir(agents["alice"].ID), w.agentDir(agents["bob"].ID)
	// The identity line is printed before the run dir is populated, so wait for the files.
	for _, d := range []string{aliceDir, bobDir} {
		for _, f := range []string{"ctl.sock", "ctl.token", "agent.json"} {
			waitForFile(t, filepath.Join(d, f))
		}
	}
	if err := waitForFileErr(filepath.Join(aliceDir, "mcp.json")); err != nil {
		t.Fatalf("claude gets an MCP config file: %v", err)
	}

	// Alice's model asks who it is and who else is here.
	as, bs := w.shim(aliceDir), w.shim(bobDir)
	txt, isErr := as.call("relay_whoami", nil)
	if isErr || !strings.Contains(txt, session) || !strings.Contains(txt, `"name": "alice"`) || !strings.Contains(txt, `"name": "bob"`) {
		t.Fatalf("whoami: %q", txt)
	}

	// Alice hands work to Bob; it appears in Bob's terminal as a prompt.
	txt, isErr = as.call("relay_send", map[string]any{"to": "bob", "body": "please run the tests in ./pkg", "priority": "high"})
	if isErr {
		t.Fatalf("send: %s", txt)
	}
	var sent struct {
		ID string `json:"msg_id"`
	}
	json.Unmarshal([]byte(txt), &sent)
	got := bob.waitSubmit("please run the tests in ./pkg")
	for _, want := range []string{"[relay | from alice (orchestrator) | task | high | msg " + sent.ID + "]", `reply_to="` + sent.ID + `"`} {
		if !strings.Contains(got, want) {
			t.Errorf("Bob's prompt lacks %q:\n%s", want, got)
		}
	}

	// Bob's model answers through its own shim; the reply reaches Alice's terminal.
	txt, isErr = bs.call("relay_send", map[string]any{"reply_to": sent.ID, "body": "all 12 tests pass"})
	if isErr || !strings.Contains(txt, `"to": [`) || !strings.Contains(txt, "alice") {
		t.Fatalf("reply: %q", txt)
	}
	reply := alice.waitSubmit("all 12 tests pass")
	if !strings.Contains(reply, "from bob (developer) | answer") {
		t.Errorf("Alice's prompt:\n%s", reply)
	}
	// the parent is now done, and the model can wait for it
	txt, _ = as.call("relay_wait", map[string]any{"msg_id": sent.ID, "timeout_s": 2})
	if !strings.Contains(txt, "all 12 tests pass") {
		t.Errorf("wait should return the reply: %q", txt)
	}

	// A mistyped name produces an actionable error listing the agents.
	txt, isErr = as.call("relay_send", map[string]any{"to": "carol", "body": "x"})
	if !isErr || !strings.Contains(txt, "unknown_agent") || !strings.Contains(txt, "bob") {
		t.Errorf("unknown agent: %q", txt)
	}

	// The human can inspect and join in.
	out, _, code := w.runRelay("messages", "--session="+session)
	if code != 0 || !strings.Contains(out, "please run the tests") || !strings.Contains(out, "all 12 tests pass") {
		t.Errorf("relay messages (%d):\n%s", code, out)
	}
	if _, errs, code := w.runRelay("send", "bob", "--session="+session, "thanks", "from", "the", "user"); code != 0 {
		t.Fatalf("relay send: %s", errs)
	}
	if h := bob.waitSubmit("thanks from the user"); !strings.Contains(h, "from user") {
		t.Errorf("user message header: %q", h)
	}

	// Both tools quit; the daemon and run directories end up clean.
	alice.pty.Write([]byte("/quit\r"))
	bob.pty.Write([]byte("/quit\r"))
	alice.waitOutput("bye")
	bob.waitOutput("bye")
	<-alice.done
	<-bob.done
	for _, d := range []string{aliceDir, bobDir} {
		deadline := time.Now().Add(3 * time.Second)
		for {
			if _, err := os.Stat(d); os.IsNotExist(err) {
				break
			}
			if time.Now().After(deadline) {
				t.Fatalf("run dir %s was not removed on exit", d)
			}
			time.Sleep(20 * time.Millisecond)
		}
	}

	// ZERO FOOTPRINT: nothing under the tools' config dirs or the project changed.
	after := w.footprint()
	if len(before) != len(after) {
		t.Fatalf("files appeared or vanished in the fake HOME:\nbefore %v\nafter  %v", keys(before), keys(after))
	}
	for k, v := range before {
		if after[k] != v {
			t.Errorf("%s was modified by relay", k)
		}
	}
	// ...and the tool was only ever given per-launch flags.
	if _, err := os.Stat(filepath.Join(w.home, ".claude", "mcp.json")); err == nil {
		t.Error("relay must not create MCP config in ~/.claude")
	}
}

func keys(m map[string]string) []string {
	var k []string
	for x := range m {
		k = append(k, x)
	}
	sort.Strings(k)
	return k
}

func (r *relayProc) notSubmitted(contains string, within time.Duration) {
	r.t.Helper()
	deadline := time.Now().Add(within)
	for time.Now().Before(deadline) {
		for _, s := range r.submits() {
			if strings.Contains(s, contains) {
				r.t.Fatalf("%q must not have reached the tool yet, but it got %q", contains, s)
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func TestApproveInboundHoldsUntilApprovedByCLIOrChord(t *testing.T) {
	w := newWorld(t)
	alice := w.start("claude", "orchestrator", "--session=NEW_LOCAL", "--name=alice")
	session, _ := alice.identity()
	bob := w.start("codex", "developer", "--session="+session, "--name=bob", "--approve-inbound")
	bob.identity()
	agents := w.agents(session)
	as := w.shim(w.agentDir(agents["alice"].ID))

	// held: nothing is typed into Bob's terminal, and his terminal rings the bell
	for _, body := range []string{"delete the build dir", "run the linter"} {
		if txt, isErr := as.call("relay_send", map[string]any{"to": "bob", "body": body}); isErr || !strings.Contains(txt, "held for bob") {
			t.Fatalf("send: %q", txt)
		}
	}
	bob.notSubmitted("delete the build dir", 1500*time.Millisecond)
	waitFor(t, "bell in Bob's terminal", func() bool { return strings.Contains(bob.output(), "\x07") })

	// the human lists and approves one from another terminal
	out, _, code := w.runRelay("approve", "ls", "--session="+session)
	if code != 0 || !strings.Contains(out, "delete the build dir") || !strings.Contains(out, "run the linter") || !strings.Contains(out, "awaiting human approval") {
		t.Fatalf("approve ls (%d):\n%s", code, out)
	}
	var heldID string
	for _, l := range strings.Split(out, "\n") {
		if strings.Contains(l, "run the linter") {
			heldID = strings.Fields(l)[0]
		}
	}
	if _, errs, code := w.runRelay("approve", "accept", heldID); code != 0 {
		t.Fatalf("approve accept: %s", errs)
	}
	bob.waitSubmit("run the linter")
	bob.notSubmitted("delete the build dir", 300*time.Millisecond)

	// ...and rejects the other with the in-terminal chord: Ctrl+\ then r
	bob.pty.Write([]byte{0x1c, 'r'})
	waitFor(t, "the held message to be rejected", func() bool {
		out, _, _ := w.runRelay("messages", "--session="+session, "--state=rejected")
		return strings.Contains(out, "delete the build dir")
	})
	// Alice hears about the rejection, and the chord keys never reached the tool.
	alice.waitSubmit("was rejected by the user")
	for _, s := range bob.submits() {
		if strings.ContainsRune(s, 0x1c) {
			t.Errorf("chord prefix leaked to the tool: %q", s)
		}
	}
	if out, _, _ := w.runRelay("approve"); !strings.Contains(out, "Nothing is waiting") {
		t.Errorf("approve with nothing held: %q", out)
	}
}

func TestMessagesWaitForAnUnsentDraft(t *testing.T) {
	w := newWorld(t)
	alice := w.start("claude", "--session=NEW_LOCAL", "--name=alice")
	session, _ := alice.identity()
	bob := w.start("codex", "--session="+session, "--name=bob")
	bob.identity()
	agents := w.agents(session)
	as := w.shim(w.agentDir(agents["alice"].ID))

	time.Sleep(1500 * time.Millisecond)  // let Bob's tool settle at its prompt
	bob.pty.Write([]byte("half a sent")) // the user is typing something and pauses
	time.Sleep(2 * time.Second)          // longer than the "user is typing" grace period
	if txt, isErr := as.call("relay_send", map[string]any{"to": "bob", "body": "interrupting your draft"}); isErr {
		t.Fatal(txt)
	}
	bob.notSubmitted("interrupting your draft", 2500*time.Millisecond)

	bob.pty.Write([]byte("ence\r")) // the user finishes and sends
	if s := bob.waitSubmit("half a sentence"); strings.Contains(s, "relay") {
		t.Fatalf("the draft must be submitted alone: %q", s)
	}
	got := bob.waitSubmit("interrupting your draft") // now it is Bob's turn to get it, as its own prompt
	if strings.Contains(got, "half a sentence") {
		t.Fatalf("message merged with the draft: %q", got)
	}
}

func TestKilledAgentLeavesOnlyCollectableLeftovers(t *testing.T) {
	w := newWorld(t)
	alice := w.start("claude", "--session=NEW_LOCAL", "--name=alice")
	session, _ := alice.identity()
	dir := w.agentDir(w.agents(session)["alice"].ID)
	if _, err := os.Stat(dir); err != nil {
		t.Fatal(err)
	}
	alice.cmd.Process.Kill() // kill -9: no cleanup runs
	<-alice.done
	alice.cmd.Wait()
	// the fake tool exits with its PTY; the run dir is left behind
	out, _, code := w.runRelay("gc")
	if code != 0 || !strings.Contains(out, dir) {
		t.Fatalf("gc (%d): %q", code, out)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatal("gc must remove the stale run dir")
	}
	if out, _, _ := w.runRelay("gc"); !strings.Contains(out, "Nothing to clean up") {
		t.Errorf("second gc: %q", out)
	}
}

// TestKilledAgentResumesAutomaticallyOnRelaunch is the real end-to-end proof
// of the resume feature: relaunching the exact same `relay <tool>
// --session=<id> --name=<x>` after a crash picks the same agent identity
// back up, using a resume token this CLI process never saw directly - it
// was saved to ~/.relay/identities by the killed process and read back
// automatically, with no new flag needed for the common case.
func TestKilledAgentResumesAutomaticallyOnRelaunch(t *testing.T) {
	w := newWorld(t)
	alice := w.start("claude", "--session=NEW_LOCAL", "--name=alice")
	session, _ := alice.identity()
	aliceID := w.agents(session)["alice"].ID

	alice.cmd.Process.Kill() // kill -9: no bye, no cleanup, no chance to hand off a token by hand
	<-alice.done
	alice.cmd.Wait()
	waitFor(t, "alice marked disconnected", func() bool {
		a, ok := w.agents(session)["alice"]
		return ok && !a.Connected
	})

	alice2 := w.start("claude", "--session="+session, "--name=alice")
	out := alice2.waitOutput("welcome back")
	if !strings.Contains(out, "welcome back, alice") {
		t.Fatalf("expected a resumed welcome, got:\n%s", out)
	}
	if got := w.agents(session)["alice"].ID; got != aliceID {
		t.Fatalf("resumed as a different agent: got %s, want %s", got, aliceID)
	}
}

// TestFreshIgnoresASavedIdentityAndResumeFailsLoudlyWithoutOne exercises
// both new flags: --fresh always registers a new agent even though a saved
// identity for that exact name exists (colliding on the name, since the old
// one is still "occupied" from the daemon's point of view), and --resume
// refuses to silently fall back to fresh registration when nothing is saved.
func TestFreshIgnoresASavedIdentityAndResumeFailsLoudlyWithoutOne(t *testing.T) {
	w := newWorld(t)
	alice := w.start("claude", "--session=NEW_LOCAL", "--name=alice")
	session, _ := alice.identity()

	alice.cmd.Process.Kill()
	<-alice.done
	alice.cmd.Wait()
	waitFor(t, "alice marked disconnected", func() bool {
		a, ok := w.agents(session)["alice"]
		return ok && !a.Connected
	})

	// --fresh: a saved identity exists, but must be ignored - the name is
	// still occupied by the not-yet-reaped old registration, so this fails
	// exactly like it would have if resume never existed.
	_, errOut, code := w.runRelay("claude", "--session="+session, "--name=alice", "--fresh")
	if code == 0 || !strings.Contains(errOut, "already used") {
		t.Fatalf("--fresh should collide on the occupied name: code=%d stderr=%q", code, errOut)
	}

	// --resume against a name with no saved identity at all: fails loudly
	// rather than silently registering "bob" fresh.
	_, errOut2, code2 := w.runRelay("claude", "--session="+session, "--name=bob", "--resume")
	if code2 == 0 || !strings.Contains(errOut2, "no saved identity") {
		t.Fatalf("--resume without a saved identity should fail loudly: code=%d stderr=%q", code2, errOut2)
	}
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(15 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

// hook runs `relay hook` the way Claude Code does: payload on stdin, answer on stdout.
func (w *world) hook(dir, event string, payload map[string]any) string {
	w.t.Helper()
	payload["hook_event_name"] = event
	b, _ := json.Marshal(payload)
	cmd := exec.Command(filepath.Join(w.bin, "relay"), "hook", event, "--dir", dir)
	cmd.Env = w.env
	cmd.Stdin = bytes.NewReader(b)
	out, err := cmd.Output()
	if err != nil {
		w.t.Fatalf("relay hook %s: %v", event, err)
	}
	return strings.TrimSpace(string(out))
}

func (w *world) peerState(as *shim, name string) string {
	w.t.Helper()
	txt, _ := as.call("relay_list_agents", nil)
	var l struct {
		Agents []struct{ Name, State string } `json:"agents"`
	}
	json.Unmarshal([]byte(strings.SplitN(txt, "\n\nNote", 2)[0]), &l)
	for _, a := range l.Agents {
		if a.Name == name {
			return a.State
		}
	}
	return ""
}

func TestHooksDeliverMidTurnAndAtStopWithoutTypingTwice(t *testing.T) {
	w := newWorld(t)
	alice := w.start("claude", "--session=NEW_LOCAL", "--name=alice")
	session, _ := alice.identity()
	bob := w.start("claude", "--session="+session, "--name=bob")
	bob.identity()
	agents := w.agents(session)
	as := w.shim(w.agentDir(agents["alice"].ID))
	bobDir := w.agentDir(agents["bob"].ID)

	// the per-launch settings file registers exactly our hook events
	data, err := os.ReadFile(filepath.Join(bobDir, "settings.json"))
	if err != nil || !strings.Contains(string(data), "hook PostToolUse --dir") {
		t.Fatalf("hook settings: %v %s", err, data)
	}

	time.Sleep(1800 * time.Millisecond) // Bob's tool settles at its prompt
	// Bob's user starts a long turn (the fake tool "thinks" for 4 s)
	bob.pty.Write([]byte("work\r"))
	w.hook(bobDir, "UserPromptSubmit", map[string]any{"prompt": "work"})
	waitFor(t, "Bob busy", func() bool { return w.peerState(as, "bob") == "busy" })

	send := func(body, prio string) {
		if txt, isErr := as.call("relay_send", map[string]any{"to": "bob", "body": body, "priority": prio}); isErr {
			t.Fatal(txt)
		}
	}
	send("high-priority note", "high")
	send("normal note", "normal")
	bob.notSubmitted("note", 800*time.Millisecond) // both wait: the tool is busy

	// a tool finishes: only the high-priority message rides along, as hook context
	out := w.hook(bobDir, "PostToolUse", map[string]any{"tool_name": "Bash"})
	var pt struct {
		H struct {
			Event string `json:"hookEventName"`
			AC    string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	if err := json.Unmarshal([]byte(out), &pt); err != nil || pt.H.Event != "PostToolUse" ||
		!strings.Contains(pt.H.AC, "high-priority note") || strings.Contains(pt.H.AC, "normal note") ||
		!strings.Contains(pt.H.AC, "from alice") || !strings.Contains(pt.H.AC, `reply_to=`) {
		t.Fatalf("PostToolUse output %q (%v)", out, err)
	}
	if again := w.hook(bobDir, "PostToolUse", map[string]any{"tool_name": "Bash"}); again != "" {
		t.Fatalf("a delivered message is not delivered twice: %q", again)
	}

	// as the turn ends, the rest goes out as a Stop-continue instead of a typed prompt
	out = w.hook(bobDir, "Stop", map[string]any{"stop_hook_active": false})
	var st struct{ Decision, Reason string }
	if err := json.Unmarshal([]byte(out), &st); err != nil || st.Decision != "block" || !strings.Contains(st.Reason, "normal note") {
		t.Fatalf("Stop output %q", out)
	}
	if out := w.hook(bobDir, "Stop", map[string]any{"stop_hook_active": true}); out != "" {
		t.Fatalf("nothing left to say at the next stop: %q", out)
	}

	// neither message was ever typed into the terminal, and the daemon knows they were injected
	time.Sleep(4500 * time.Millisecond) // let the fake turn finish and the bus tick a few times
	bob.notSubmitted("note", 500*time.Millisecond)
	out2, _, _ := w.runRelay("messages", "--session="+session, "--state=injected")
	if !strings.Contains(out2, "high-priority note") || !strings.Contains(out2, "normal note") {
		t.Fatalf("messages:\n%s", out2)
	}
}

func TestStopContinueIsBoundedAndHookFailuresAreSilent(t *testing.T) {
	w := newWorld(t)
	alice := w.start("claude", "--session=NEW_LOCAL", "--name=alice")
	session, _ := alice.identity()
	bob := w.start("claude", "--session="+session, "--name=bob")
	bob.identity()
	agents := w.agents(session)
	as := w.shim(w.agentDir(agents["alice"].ID))
	bobDir := w.agentDir(agents["bob"].ID)
	time.Sleep(1500 * time.Millisecond)

	// two agents ping-ponging via Stop-continue can't trap the tool in an endless loop
	continued := 0
	for i := 0; i < maxStopBlocksForTest+3; i++ {
		as.call("relay_send", map[string]any{"to": "bob", "body": fmt.Sprintf("more work %d", i)})
		if out := w.hook(bobDir, "Stop", map[string]any{"stop_hook_active": true}); strings.Contains(out, "block") {
			continued++
		}
	}
	if continued != maxStopBlocksForTest {
		t.Fatalf("Stop-continue must stop after %d in a row, got %d", maxStopBlocksForTest, continued)
	}
	w.hook(bobDir, "UserPromptSubmit", map[string]any{"prompt": "user typed"}) // a real prompt resets the count
	as.call("relay_send", map[string]any{"to": "bob", "body": "after reset"})
	if out := w.hook(bobDir, "Stop", map[string]any{}); !strings.Contains(out, "block") {
		t.Fatalf("reset: %q", out)
	}

	// hooks against a dead agent, a missing dir or garbage input say nothing and succeed
	cmd := exec.Command(filepath.Join(w.bin, "relay"), "hook", "Stop", "--dir", "/nonexistent")
	cmd.Env = w.env
	cmd.Stdin = strings.NewReader(`{"hook_event_name":"Stop"}`)
	if out, err := cmd.Output(); err != nil || len(out) != 0 {
		t.Fatalf("a hook must never break the tool: %q %v", out, err)
	}
	cmd = exec.Command(filepath.Join(w.bin, "relay"), "hook", "Stop", "--dir", bobDir)
	cmd.Env = w.env
	cmd.Stdin = strings.NewReader(`not json`)
	if out, err := cmd.Output(); err != nil || len(out) != 0 {
		t.Fatalf("garbage input: %q %v", out, err)
	}
}

const maxStopBlocksForTest = 5 // collab.maxStopBlocks

func TestTranscriptFeedsContextAndConfirmsDelivery(t *testing.T) {
	w := newWorld(t)
	alice := w.start("claude", "--session=NEW_LOCAL", "--name=alice")
	session, _ := alice.identity()
	bob := w.start("claude", "--session="+session, "--name=bob")
	bob.identity()
	agents := w.agents(session)
	as := w.shim(w.agentDir(agents["alice"].ID))
	bobDir := w.agentDir(agents["bob"].ID)

	// Claude tells its agent where the transcript is (any hook payload carries it)
	tr := filepath.Join(t.TempDir(), "session.jsonl")
	os.WriteFile(tr, nil, 0o600)
	w.hook(bobDir, "SessionStart", map[string]any{"transcript_path": tr, "session_id": "s1"})

	f, _ := os.OpenFile(tr, os.O_APPEND|os.O_WRONLY, 0o600)
	defer f.Close()
	now := time.Now().UTC().Format(time.RFC3339Nano)
	line := func(v any) { b, _ := json.Marshal(v); f.Write(append(b, '\n')) }
	line(map[string]any{"type": "user", "uuid": "u1", "timestamp": now, "message": map[string]any{"role": "user", "content": "refactor the parser"}})
	line(map[string]any{"type": "assistant", "uuid": "a1", "timestamp": now, "message": map[string]any{"role": "assistant", "content": []any{
		map[string]any{"type": "text", "text": "Done. I used key sk-ant-api03-abcdefghijklmnopqrstuvwxyz for the test run and moved parse() to parser.go."}}}})

	var txt string
	waitFor(t, "Bob's conversation to reach the daemon", func() bool {
		var isErr bool
		txt, isErr = as.call("relay_get_context", map[string]any{"agent": "bob", "mode": "last_answer"})
		return !isErr && strings.Contains(txt, "parser.go")
	})
	if strings.Contains(txt, "sk-ant-") || !strings.Contains(txt, "[REDACTED:api-key]") {
		t.Fatalf("secrets must be masked across agents: %s", txt)
	}
	txt, _ = as.call("relay_get_context", map[string]any{"agent": "bob", "mode": "search", "query": "refactor"})
	if !strings.Contains(txt, `"role": "user"`) || !strings.Contains(txt, "refactor the parser") {
		t.Fatalf("search: %s", txt)
	}
	if txt, isErr := as.call("relay_get_context", map[string]any{"agent": "nobody"}); !isErr || !strings.Contains(txt, "bob") {
		t.Fatalf("unknown agent: %s", txt)
	}

	// A message reaches Bob through a hook; the transcript then shows it (as the hook_additional_context
	// attachment Claude records) and the daemon moves it to "acknowledged" on that evidence.
	waitFor(t, "Bob idle", func() bool { return true })
	time.Sleep(1500 * time.Millisecond)
	bob.pty.Write([]byte("x\r")) // keep Bob busy so the hook path is the one used
	w.hook(bobDir, "UserPromptSubmit", map[string]any{})
	txt, _ = as.call("relay_send", map[string]any{"to": "bob", "body": "please double-check parser.go", "priority": "high"})
	var sent struct {
		ID string `json:"msg_id"`
	}
	json.Unmarshal([]byte(txt), &sent)
	out := w.hook(bobDir, "PostToolUse", map[string]any{})
	if !strings.Contains(out, sent.ID) {
		t.Fatalf("hook output %q", out)
	}
	if got, _, _ := w.runRelay("messages", "--session="+session, "--state=acknowledged"); strings.Contains(got, sent.ID) {
		t.Fatal("not acknowledged before the transcript shows it")
	}
	line(map[string]any{"type": "attachment", "uuid": "h1", "timestamp": now, "attachment": map[string]any{
		"type": "hook_additional_context", "hookEvent": "PostToolUse", "content": []string{"[relay | from alice | task | high | msg " + sent.ID + "]\nplease double-check parser.go"}}})
	waitFor(t, "delivery confirmed from the transcript", func() bool {
		got, _, _ := w.runRelay("messages", "--session="+session, "--state=acknowledged")
		return strings.Contains(got, sent.ID)
	})
}

func TestCodexRolloutDrivesStateAndPlanHold(t *testing.T) {
	w := newWorld(t)
	alice := w.start("claude", "--session=NEW_LOCAL", "--name=alice")
	session, _ := alice.identity()
	coder := w.start("codex", "--session="+session, "--name=coder")
	coder.identity()
	agents := w.agents(session)
	as := w.shim(w.agentDir(agents["alice"].ID))
	time.Sleep(1500 * time.Millisecond)

	// Codex creates its rollout at the first message; its briefing names our agent
	now := time.Now()
	dir := filepath.Join(w.home, ".codex", "sessions", now.Format("2006"), now.Format("01"), now.Format("02"))
	os.MkdirAll(dir, 0o755)
	path := filepath.Join(dir, "rollout-test.jsonl")
	f, _ := os.Create(path)
	defer f.Close()
	line := func(typ string, payload map[string]any) {
		b, _ := json.Marshal(map[string]any{"timestamp": time.Now().UTC().Format(time.RFC3339Nano), "type": typ, "payload": payload})
		f.Write(append(b, '\n'))
	}
	line("session_meta", map[string]any{"cwd": coder.cmd.Dir, "id": "x"})
	line("response_item", map[string]any{"type": "message", "role": "developer", "content": []any{
		map[string]any{"type": "input_text", "text": `You are "coder", working in a Relay session (id ` + session + `) alongside...`}}})
	line("event_msg", map[string]any{"type": "task_started", "turn_id": "t1"})
	line("event_msg", map[string]any{"type": "item_completed", "item": map[string]any{"type": "AgentMessage", "id": "m1", "content": []any{map[string]any{"type": "Text", "text": "working on the plan"}}}})

	waitFor(t, "state busy from the rollout", func() bool { return w.peerState(as, "coder") == "busy" })
	if txt, _ := as.call("relay_get_context", map[string]any{"agent": "coder"}); !strings.Contains(txt, "working on the plan") {
		waitFor(t, "rollout turns uploaded", func() bool {
			txt, _ = as.call("relay_get_context", map[string]any{"agent": "coder"})
			return strings.Contains(txt, "working on the plan")
		})
	}

	// the plan turn ends with a proposed plan: the user must decide, nothing may be typed meanwhile
	line("event_msg", map[string]any{"type": "task_complete", "turn_id": "t1", "last_agent_message": "Plan:\n<proposed_plan>\n1. do it\n</proposed_plan>"})
	waitFor(t, "dialog state while the plan awaits a decision", func() bool { return w.peerState(as, "coder") == "dialog" })
	as.call("relay_send", map[string]any{"to": "coder", "body": "ping during plan decision"})
	coder.notSubmitted("ping during plan decision", 2*time.Second)

	// the user answers: the next turn starts, and then completes
	line("event_msg", map[string]any{"type": "task_started", "turn_id": "t2"})
	line("event_msg", map[string]any{"type": "task_complete", "turn_id": "t2", "last_agent_message": "done"})
	coder.waitSubmit("ping during plan decision")
}

func TestGCRetentionCommandsReportAndNeverTouchLiveSessions(t *testing.T) {
	w := newWorld(t)
	alice := w.start("claude", "--session=NEW_LOCAL", "--name=alice")
	session, _ := alice.identity()
	out, errs, code := w.runRelay("gc", "--older-than=1d", "--compress", "--dry-run")
	if code != 0 || !strings.Contains(out, "would remove 0 idle session(s)") || !strings.Contains(out, "would compress 0 log segment(s)") {
		t.Fatalf("dry run (%d): %q %q", code, out, errs)
	}
	out, _, code = w.runRelay("gc", "--older-than=1m") // the session has a live agent: nothing to prune
	if code != 0 || !strings.Contains(out, "removed 0 idle session(s)") {
		t.Fatalf("(%d) %q", code, out)
	}
	if _, ok := w.agents(session)["alice"]; !ok {
		t.Fatal("a live session must survive gc")
	}
	if _, errs, code := w.runRelay("gc", "--older-than=abc"); code != 2 || !strings.Contains(errs, "--older-than") {
		t.Fatalf("bad duration: %d %q", code, errs)
	}
}
