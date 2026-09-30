//go:build e2e_real && !windows

// Live tests of `relay agy` against the REAL agy binary with the user's real
// login (they spend model quota and take minutes). Run on a machine with an
// authenticated agy:
//
//	go test -tags e2e_real -run RealAgy -v -timeout 60m -count=1 ./internal/cli
//
// RELAY_AGY_DIR=/dir/with/an/agy pins the agy version under test (put a
// copy of that version's binary there, named agy). RELAY_AGY_CWD is the
// working directory agy runs in (default ~/.relay-agy-lab/live, which agy's
// folder-trust prompt must already have been accepted for).
//
// agy's user-global files are snapshotted before every test and must be
// byte-identical afterwards (the test fails otherwise, after restoring them):
// ~/.gemini/config/mcp_config.json and ~/.gemini/antigravity-cli/settings.json.
// For the duration of a test, "mcp(relay/*)" is added to the latter so agy
// does not ask before each relay tool call.
package cli

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/charmbracelet/x/vt"
	"github.com/creack/pty"
)

var (
	realOnce  sync.Once
	realRelay string
	realErr   error
)

func buildRealRelay(t *testing.T) string {
	t.Helper()
	realOnce.Do(func() {
		dir, err := os.MkdirTemp("", "relay-real")
		if err != nil {
			realErr = err
			return
		}
		realRelay = filepath.Join(dir, "relay")
		if out, err := exec.Command("go", "build", "-o", realRelay, "../..").CombinedOutput(); err != nil {
			realErr = fmt.Errorf("%s: %w", out, err)
		}
	})
	if realErr != nil {
		t.Fatal("building relay:", realErr)
	}
	return realRelay
}

type liveFile struct {
	path   string
	data   []byte
	exists bool
}

func snapshot(path string) liveFile {
	data, err := os.ReadFile(path)
	return liveFile{path: path, data: data, exists: err == nil}
}

func (f liveFile) restore() {
	if f.exists {
		os.WriteFile(f.path, f.data, 0o644)
	} else {
		os.Remove(f.path)
	}
}

func (f liveFile) unchanged() bool {
	data, err := os.ReadFile(f.path)
	if !f.exists {
		return errors.Is(err, os.ErrNotExist)
	}
	return err == nil && bytes.Equal(data, f.data)
}

type live struct {
	t      *testing.T
	relay  string
	home   string // RELAY_HOME
	cwd    string
	env    []string
	gemini string
}

// newLive isolates relay (RELAY_HOME), guards agy's global files, and
// allows relay's tools in agy for the test.
func newLive(t *testing.T) *live {
	t.Helper()
	agy := "agy"
	if d := os.Getenv("RELAY_AGY_DIR"); d != "" {
		agy = filepath.Join(d, "agy")
	}
	if _, err := exec.LookPath(agy); err != nil {
		t.Skip("agy not installed")
	}
	userHome, _ := os.UserHomeDir()
	l := &live{t: t, relay: buildRealRelay(t), gemini: filepath.Join(userHome, ".gemini")}
	var err error
	if l.home, err = os.MkdirTemp("/tmp", "rlr"); err != nil { // short: unix socket paths are limited
		t.Fatal(err)
	}
	l.cwd = os.Getenv("RELAY_AGY_CWD")
	if l.cwd == "" {
		l.cwd = filepath.Join(userHome, ".relay-agy-lab", "live")
	}
	os.MkdirAll(l.cwd, 0o755)
	path := os.Getenv("PATH")
	if d := os.Getenv("RELAY_AGY_DIR"); d != "" {
		path = d + string(os.PathListSeparator) + path
	}
	l.env = append(os.Environ(), "RELAY_HOME="+l.home, "RELAY_SKIP_SESSION_PROMPT=1", "PATH="+path, "RELAY_ACTIVE=")

	cfg := snapshot(filepath.Join(l.gemini, "config", "mcp_config.json"))
	settings := snapshot(filepath.Join(l.gemini, "antigravity-cli", "settings.json"))
	allowRelayTools(settings)
	t.Cleanup(func() {
		stop := exec.Command(l.relay, "daemon", "stop")
		stop.Env = l.env // this test's RELAY_HOME: never the user's own daemon
		_ = stop.Run()
		cfgOK := cfg.unchanged()
		cfg.restore()
		settings.restore()
		os.RemoveAll(l.home)
		if !cfgOK {
			t.Errorf("agy's %s was not left byte-identical (restored now)", cfg.path)
		}
		if _, err := os.Stat(filepath.Join(l.gemini, "config", ".relay-agy")); err == nil {
			t.Errorf("relay's agy state directory was left behind")
			os.RemoveAll(filepath.Join(l.gemini, "config", ".relay-agy"))
		}
	})
	t.Logf("agy: %s", agyVersion(agy, l.env))
	return l
}

func agyVersion(agy string, env []string) string {
	cmd := exec.Command(agy, "--version")
	cmd.Env = env
	out, _ := cmd.CombinedOutput()
	return strings.TrimSpace(string(out))
}

// allowRelayTools adds "mcp(relay/*)" to agy's permissions (the one-time
// step the launch note asks users to take).
func allowRelayTools(f liveFile) {
	s := string(f.data)
	if strings.Contains(s, `"mcp(relay/*)"`) {
		return
	}
	switch {
	case !f.exists || strings.TrimSpace(s) == "":
		s = `{"permissions":{"allow":["mcp(relay/*)"]}}`
	case strings.Contains(s, `"allow": [`):
		s = strings.Replace(s, `"allow": [`, `"allow": ["mcp(relay/*)", `, 1)
	case strings.Contains(s, `"permissions"`):
		s = strings.Replace(s, `"permissions": {`, `"permissions": {"allow": ["mcp(relay/*)"], `, 1)
	default:
		s = strings.Replace(s, "{", `{"permissions": {"allow": ["mcp(relay/*)"]}, `, 1)
	}
	os.MkdirAll(filepath.Dir(f.path), 0o755)
	os.WriteFile(f.path, []byte(s), 0o644)
}

func (l *live) run(args ...string) string {
	l.t.Helper()
	cmd := exec.Command(l.relay, args...)
	cmd.Env = l.env
	out, _ := cmd.CombinedOutput()
	return string(out)
}

// liveProc is one `relay <tool>` in a PTY that answers terminal queries.
type liveProc struct {
	t    *testing.T
	cmd  *exec.Cmd
	pty  *os.File
	mu   sync.Mutex
	raw  bytes.Buffer
	em   *vt.Emulator
	done chan struct{}
}

// output is everything the process has printed so far.
func (p *liveProc) output() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.raw.String()
}

var liveReplies = map[string]string{
	"\x1b[6n": "\x1b[1;1R", "\x1b[c": "\x1b[?62c", "\x1b[0c": "\x1b[?62c",
	"\x1b]10;?\x1b\\": "\x1b]10;rgb:fafa/f9f9/f6f6\x1b\\", "\x1b]11;?\x1b\\": "\x1b]11;rgb:1212/1212/1212\x1b\\",
	"\x1b]10;?\x07": "\x1b]10;rgb:fafa/f9f9/f6f6\x07", "\x1b]11;?\x07": "\x1b]11;rgb:1212/1212/1212\x07",
}

func (l *live) start(tool string, args ...string) *liveProc {
	l.t.Helper()
	p := &liveProc{t: l.t, done: make(chan struct{}), em: vt.NewEmulator(140, 45)}
	go func() {
		buf := make([]byte, 4096)
		for {
			if _, err := p.em.Read(buf); err != nil {
				return
			}
		}
	}()
	p.cmd = exec.Command(l.relay, append([]string{tool}, args...)...)
	p.cmd.Env = l.env
	p.cmd.Dir = l.cwd
	var err error
	if p.pty, err = pty.StartWithSize(p.cmd, &pty.Winsize{Rows: 45, Cols: 140}); err != nil {
		l.t.Fatal(err)
	}
	go func() {
		defer close(p.done)
		buf := make([]byte, 32768)
		tail := ""
		for {
			n, err := p.pty.Read(buf)
			if n > 0 {
				p.mu.Lock()
				p.raw.Write(buf[:n])
				p.em.Write(buf[:n])
				p.mu.Unlock()
				tail += string(buf[:n])
				for q, r := range liveReplies {
					for c := strings.Count(tail, q); c > 0; c-- {
						p.pty.WriteString(r)
					}
					tail = strings.ReplaceAll(tail, q, "")
				}
				if len(tail) > 512 {
					tail = tail[len(tail)-128:]
				}
			}
			if err != nil {
				return
			}
		}
	}()
	l.t.Cleanup(func() {
		p.quit()
		p.pty.Close()
	})
	return p
}

func (p *liveProc) screen() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.em.String()
}

func (p *liveProc) quit() {
	select {
	case <-p.done:
		return
	default:
	}
	// Cancel whatever runs (a dialog, then a turn), then Ctrl+D twice.
	p.pty.Write([]byte{0x1b})
	time.Sleep(time.Second)
	p.pty.Write([]byte{0x1b})
	time.Sleep(time.Second)
	p.pty.Write([]byte{0x04})
	time.Sleep(500 * time.Millisecond)
	p.pty.Write([]byte{0x04})
	select {
	case <-p.done:
	case <-time.After(30 * time.Second):
		// relay forwards SIGTERM to agy, then cleans up (agy mcp remove):
		// give that all the time it needs before a kill that would skip it.
		p.cmd.Process.Signal(syscall.SIGTERM)
		select {
		case <-p.done:
		case <-time.After(60 * time.Second):
			p.cmd.Process.Kill()
		}
	}
	p.cmd.Wait()
}

// inputBox is what sits in agy's input box: the rows between the last two
// full-width rules above its status line.
func inputBox(screen string) string {
	lines := strings.Split(screen, "\n")
	var rules []int
	for i, l := range lines {
		if t := strings.TrimSpace(l); len(t) >= 20 && strings.Trim(t, "─") == "" {
			rules = append(rules, i)
		}
	}
	if len(rules) < 2 {
		return ""
	}
	box := strings.Join(lines[rules[len(rules)-2]+1:rules[len(rules)-1]], "\n")
	return strings.TrimSpace(strings.TrimPrefix(strings.TrimSpace(box), ">"))
}

func (p *liveProc) waitScreen(d time.Duration, what string, re *regexp.Regexp) {
	p.t.Helper()
	deadline := time.Now().Add(d)
	for time.Now().Before(deadline) {
		if re.MatchString(p.screen()) {
			return
		}
		time.Sleep(250 * time.Millisecond)
	}
	p.t.Fatalf("timed out waiting for %s; screen:\n%s", what, p.screen())
}

var sentID = regexp.MustCompile(`sent ([0-9A-Z]{26})`)

func (l *live) send(to, body string) string {
	l.t.Helper()
	out := l.run("send", to, body)
	m := sentID.FindStringSubmatch(out)
	if m == nil {
		l.t.Fatalf("relay send: %s", out)
	}
	return m[1]
}

func (l *live) state(id string) string {
	for _, line := range strings.Split(l.run("messages"), "\n") {
		if f := strings.Fields(line); len(f) > 5 && f[0] == id {
			return f[5]
		}
	}
	return ""
}

func (l *live) waitState(id string, d time.Duration, want ...string) string {
	l.t.Helper()
	deadline := time.Now().Add(d)
	st := ""
	for time.Now().Before(deadline) {
		st = l.state(id)
		for _, w := range want {
			if st == w {
				return st
			}
		}
		time.Sleep(time.Second)
	}
	l.t.Fatalf("message %s is %q after %v, want %v\nmessages:\n%s", id, st, d, want, l.run("messages"))
	return st
}

func (l *live) agy(name string, extra ...string) *liveProc {
	l.t.Helper()
	p := l.start("agy", append([]string{"developer", "--session=NEW_LOCAL", "--name=" + name}, extra...)...)
	p.waitScreen(3*time.Minute, "agy ready after the briefing turn", regexp.MustCompile(`for shortcuts`))
	return p
}

// The #50 path end to end on the real agy: tools present, briefing as the
// first turn, a message typed, submitted, acknowledged (from agy's own log)
// and answered.
func TestRealAgyMessageIsDeliveredAndAnswered(t *testing.T) {
	l := newLive(t)
	bob := l.agy("bob")
	id := l.send("bob", "What is 17 plus 25? Reply in this terminal with just the number. Do not use any tools.")
	l.waitState(id, 3*time.Minute, "acknowledged", "done")
	bob.waitScreen(3*time.Minute, "the answer", regexp.MustCompile(`(?m)^\s*42\s*$`))
}

// The #50 regression: many consecutive messages, none left sitting typed
// but unsubmitted - every one reaches acknowledged.
func TestRealAgyManyMessagesAllSubmitted(t *testing.T) {
	l := newLive(t)
	bob := l.agy("bob")
	n := 20
	if v := os.Getenv("RELAY_REAL_N"); v != "" {
		fmt.Sscan(v, &n)
	}
	var ids []string
	for i := 1; i <= n; i++ {
		ids = append(ids, l.send("bob", fmt.Sprintf("Message %d of %d: reply with only the word ok-%d. No tools.", i, n, i)))
		time.Sleep(time.Duration(i%3) * time.Second) // some arrive while agy is busy with the previous one
	}
	for _, id := range ids {
		l.waitState(id, 5*time.Minute, "acknowledged", "done")
	}
	bob.waitScreen(3*time.Minute, "the last reply", regexp.MustCompile(fmt.Sprintf(`ok-%d`, n)))
	if box := inputBox(bob.screen()); box != "" {
		t.Fatalf("something is still sitting in agy's input box: %q\n%s", box, bob.screen())
	}
}

// Enter answers agy's dialogs (confirmed live: it approved a shell command).
// A message arriving while one is open waits, and nothing is typed into it.
func TestRealAgyHoldsMessagesDuringAPermissionDialog(t *testing.T) {
	l := newLive(t)
	bob := l.agy("bob")
	l.send("bob", "Use your shell tool to run exactly this command, even though it is harmless: echo relay-dialog-probe . Then reply with the word finished.")
	bob.waitScreen(3*time.Minute, "the permission dialog", regexp.MustCompile(`Run this command\?`))
	id := l.send("bob", "What is 3 plus 4? Reply with just the number. No tools.")
	time.Sleep(10 * time.Second)
	if st := l.state(id); st != "dispatched" && st != "queued" {
		t.Fatalf("a message was delivered while agy's dialog was open (state %s):\n%s", st, bob.screen())
	}
	if !regexp.MustCompile(`Run this command\?`).MatchString(bob.screen()) {
		t.Fatalf("the dialog was answered by relay:\n%s", bob.screen())
	}
	bob.pty.Write([]byte("4")) // No, cancel
	time.Sleep(300 * time.Millisecond)
	bob.pty.Write([]byte("\r"))
	l.waitState(id, 3*time.Minute, "acknowledged", "done")
	bob.waitScreen(3*time.Minute, "the answer", regexp.MustCompile(`(?m)^\s*7\s*$`))
}

// Two agy agents talk through relay's MCP tools, each acting only as itself.
func TestRealAgyToAgyThroughRelayTools(t *testing.T) {
	l := newLive(t)
	bob := l.agy("bob")
	sid := regexp.MustCompile(`session ([0-9A-Z]{26})`).FindStringSubmatch(bob.output())
	if sid == nil {
		t.Fatalf("no session id in:\n%s", bob.output())
	}
	carol := l.start("agy", "reviewer", "--session="+sid[1], "--name=carol")
	carol.waitScreen(3*time.Minute, "carol ready", regexp.MustCompile(`for shortcuts`))
	l.send("bob", "Use the relay_send tool to send carol a message whose body is exactly: ping-from-bob-7")
	deadline := time.Now().Add(4 * time.Minute)
	for time.Now().Before(deadline) {
		msgs := l.run("messages")
		if regexp.MustCompile(`bob\s+carol\s+.*ping-from-bob-7`).MatchString(msgs) {
			return
		}
		time.Sleep(2 * time.Second)
	}
	t.Fatalf("bob's agy never messaged carol:\n%s\nbob:\n%s", l.run("messages"), bob.screen())
}

// /new starts a conversation without the briefing; relay briefs it again.
func TestRealAgyNewConversationIsBriefed(t *testing.T) {
	l := newLive(t)
	bob := l.agy("bob")
	bob.pty.Write([]byte("/new"))
	time.Sleep(time.Second)
	bob.pty.Write([]byte("\r"))
	bob.waitScreen(3*time.Minute, "the briefing typed into the new conversation", regexp.MustCompile(`You are "bob", working in a Relay session`))
	id := l.send("bob", "Reply with only the word renewed. No tools.")
	l.waitState(id, 3*time.Minute, "acknowledged", "done")
}

// kill -9 of relay and agy: `relay gc` puts agy's config back exactly.
func TestRealAgyCrashIsCleanedUpByGC(t *testing.T) {
	l := newLive(t)
	bob := l.agy("bob")
	syscall.Kill(-bob.cmd.Process.Pid, syscall.SIGKILL)
	bob.cmd.Wait()
	if out := l.run("gc"); !strings.Contains(out, "agy MCP server relay") {
		t.Fatalf("relay gc did not clean up agy: %s", out)
	}
	// newLive's cleanup verifies agy's config is byte-identical again.
}

// A global session on a real relay server (server/docker-compose.yml plus
// the server binary; RELAY_E2E_GLOBAL=host:port names it): two real agy
// agents, joined by the printed token, talk through relay_send.
func TestRealAgyGlobalSession(t *testing.T) {
	server := os.Getenv("RELAY_E2E_GLOBAL")
	if server == "" {
		t.Skip("set RELAY_E2E_GLOBAL=host:port of a running relay server")
	}
	l := newLive(t)
	bob := l.start("agy", "developer", "--session=NEW", "--server="+server, "--name=bob")
	bob.waitScreen(3*time.Minute, "bob ready", regexp.MustCompile(`for shortcuts`))
	token := regexp.MustCompile(`--session=(\S+@\S+)`).FindStringSubmatch(bob.output())
	if token == nil {
		t.Fatalf("no join token printed:\n%s", bob.output())
	}
	// The printed token names the port the session was created on.
	if !strings.Contains(token[1], "@") || !strings.HasSuffix(token[1], ":"+strings.Split(server, ":")[1]) {
		t.Fatalf("token %q does not carry the server's port", token[1])
	}
	carol := l.start("agy", "reviewer", "--session="+token[1], "--name=carol")
	carol.waitScreen(3*time.Minute, "carol ready", regexp.MustCompile(`for shortcuts`))
	bob.pty.Write([]byte("\x1b[200~Use the relay_send tool to send carol a message whose body is exactly: ping-global-9\x1b[201~"))
	time.Sleep(500 * time.Millisecond)
	bob.pty.Write([]byte("\r"))
	carol.waitScreen(4*time.Minute, "bob's message typed into carol's agy", regexp.MustCompile(`\[relay \| from bob \(developer\)[\s\S]*ping-global-9`))
}
