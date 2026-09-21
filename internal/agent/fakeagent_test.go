package agent

import (
	"bufio"
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/creack/pty"

	"github.com/thesahibnanda-max/relay/internal/state"
)

var (
	fakeOnce sync.Once
	fakeBin  string
	fakeErr  error
)

// buildFake compiles testdata/fakeagent once per test binary run.
func buildFake(t *testing.T) string {
	t.Helper()
	fakeOnce.Do(func() {
		dir, err := os.MkdirTemp("", "fakeagent")
		if err != nil {
			fakeErr = err
			return
		}
		fakeBin = filepath.Join(dir, "fakeagent")
		out, err := exec.Command("go", "build", "-o", fakeBin, "../../testdata/fakeagent").CombinedOutput()
		if err != nil {
			fakeErr = &buildError{string(out), err}
		}
	})
	if fakeErr != nil {
		t.Skip("cannot build fakeagent (is `go` on PATH?):", fakeErr)
	}
	return fakeBin
}

type buildError struct {
	out string
	err error
}

func (b *buildError) Error() string { return b.err.Error() + ": " + b.out }

// rig is a running fakeagent inside agent.Run, with a PTY standing in for the
// user's terminal.
type rig struct {
	t       *testing.T
	h       *Handle
	user    *os.File // write = user keystrokes
	logPath string
	mu      sync.Mutex
	screen  strings.Builder
	done    chan int
}

func startRig(t *testing.T, env ...string) *rig {
	t.Helper()
	bin := buildFake(t)
	user, tty, err := pty.Open()
	if err != nil {
		t.Skip("no pty:", err)
	}
	_ = pty.Setsize(tty, &pty.Winsize{Rows: 24, Cols: 80})

	r := &rig{t: t, user: user, logPath: filepath.Join(t.TempDir(), "fake.jsonl"), done: make(chan int, 1)}
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := user.Read(buf)
			r.mu.Lock()
			r.screen.Write(buf[:n])
			r.mu.Unlock()
			if err != nil {
				return
			}
		}
	}()

	ready := make(chan *Handle, 1)
	go func() {
		code, err := Run(Config{
			Tool: "fake", Bin: bin,
			Env: append(os.Environ(), append([]string{"FAKE_LOG=" + r.logPath}, env...)...),
			In:  tty, Out: tty,
			ScreenRules: []state.ScreenRule{{State: state.Dialog, Contains: "Allow? (y/n)", Tail: 1}},
			OnStart:     func(h *Handle) { ready <- h },
		})
		if err != nil {
			t.Errorf("Run: %v", err)
		}
		r.done <- code
	}()
	select {
	case r.h = <-ready:
	case <-time.After(5 * time.Second):
		t.Fatal("agent did not start")
	}
	t.Cleanup(func() {
		// n answers a pending dialog, Ctrl+D exits; harmless if neither applies.
		user.Write([]byte("n\x04"))
		select {
		case <-r.done:
		case <-time.After(2 * time.Second):
		}
	})
	r.waitLog(func(ev []map[string]string) bool { return hasState(ev, "idle") }, "fakeagent to show its prompt")
	return r
}

func (r *rig) events() []map[string]string {
	f, err := os.Open(r.logPath)
	if err != nil {
		return nil
	}
	defer f.Close()
	var evs []map[string]string
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		var m map[string]string
		if json.Unmarshal(sc.Bytes(), &m) == nil {
			evs = append(evs, m)
		}
	}
	if err := sc.Err(); err != nil {
		r.t.Fatalf("reading the fake tool's log: %v", err)
	}
	return evs
}

func (r *rig) waitLog(cond func([]map[string]string) bool, what string) []map[string]string {
	r.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if evs := r.events(); cond(evs) {
			return evs
		}
		time.Sleep(10 * time.Millisecond)
	}
	r.t.Fatalf("timed out waiting for %s; log: %v", what, r.events())
	return nil
}

func hasState(evs []map[string]string, s string) bool {
	for _, e := range evs {
		if e["ev"] == "state" && e["state"] == s {
			return true
		}
	}
	return false
}

func submits(evs []map[string]string) []string {
	var out []string
	for _, e := range evs {
		if e["ev"] == "submit" {
			out = append(out, e["text"])
		}
	}
	return out
}

func (r *rig) type_(s string) { r.t.Helper(); r.user.Write([]byte(s)) }

func TestFakeAgentTracksModesAndState(t *testing.T) {
	r := startRig(t)
	// Relay reads the tool's output asynchronously, so poll rather than assume
	// the first bytes were already consumed when the tool logged "idle".
	deadline := time.Now().Add(3 * time.Second)
	var snap state.Snapshot
	for time.Now().Before(deadline) {
		snap = r.h.Snapshot()
		if snap.BracketedPaste && strings.Contains(strings.Join(r.h.Screen(), "\n"), "fakeagent ready") {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if !snap.BracketedPaste {
		t.Errorf("tracker missed the tool enabling bracketed paste: %+v", snap)
	}
	if !strings.Contains(strings.Join(r.h.Screen(), "\n"), "fakeagent ready") {
		t.Errorf("screen = %q", r.h.Screen())
	}

	r.type_("hi\r")
	r.waitLog(func(ev []map[string]string) bool { return len(submits(ev)) == 1 }, "submit")
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && !strings.Contains(strings.Join(r.h.Screen(), "\n"), "reply: hi") {
		time.Sleep(20 * time.Millisecond)
	}
	if !strings.Contains(strings.Join(r.h.Screen(), "\n"), "reply: hi") {
		t.Errorf("reply not on tracked screen: %q", r.h.Screen())
	}
}

func TestInjectSubmitsAsIfTyped(t *testing.T) {
	r := startRig(t)
	if err := r.h.Inject(context.Background(), "hello from the bus", InjectOptions{Paste: true, Submit: true}); err != nil {
		t.Fatal(err)
	}
	evs := r.waitLog(func(ev []map[string]string) bool { return len(submits(ev)) == 1 }, "injected submit")
	if got := submits(evs)[0]; got != "hello from the bus" {
		t.Errorf("submitted %q", got)
	}
}

func TestMultilinePasteDoesNotSubmitEarly(t *testing.T) {
	r := startRig(t)
	if err := r.h.Inject(context.Background(), "line1\nline2\nline3", InjectOptions{Paste: true, Submit: true}); err != nil {
		t.Fatal(err)
	}
	evs := r.waitLog(func(ev []map[string]string) bool { return len(submits(ev)) >= 1 }, "submit")
	time.Sleep(100 * time.Millisecond)
	got := submits(r.events())
	if len(got) != 1 || got[0] != "line1\nline2\nline3" {
		t.Errorf("submits = %q (want exactly one multi-line submit)", got)
	}
	_ = evs
}

func TestInjectWaitsForUserToFinishKeystroke(t *testing.T) {
	r := startRig(t)
	r.type_("\x1b[") // user is mid-way through Shift+Tab
	// Bytes still in the kernel's pty buffer are invisible to Relay (and can't
	// be split by it); wait until they have been read.
	for i := 0; i < 500; i++ {
		r.h.mux.mu.Lock()
		unsafe := !r.h.mux.p.safe(0)
		r.h.mux.mu.Unlock()
		if unsafe {
			break
		}
		time.Sleep(time.Millisecond)
	}

	done := make(chan error, 1)
	go func() {
		done <- r.h.Inject(context.Background(), "queued msg", InjectOptions{Paste: true, Submit: true})
	}()

	time.Sleep(150 * time.Millisecond)
	if n := len(submits(r.events())); n != 0 {
		t.Fatalf("injection landed inside the user's escape sequence (%d submits)", n)
	}
	r.type_("Z") // completes Shift+Tab
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	evs := r.waitLog(func(ev []map[string]string) bool { return len(submits(ev)) == 1 }, "submit after keystroke")

	// Order in the tool's ground truth: the intact key first, then the submit.
	keyAt, submitAt := -1, -1
	for i, e := range evs {
		if e["ev"] == "key" && e["seq"] == "\x1b[Z" {
			keyAt = i
		}
		if e["ev"] == "submit" {
			submitAt = i
		}
	}
	if keyAt < 0 || submitAt < keyAt {
		t.Errorf("Shift+Tab was split or reordered: keyAt=%d submitAt=%d log=%v", keyAt, submitAt, evs)
	}
}

func TestDialogDetectedFromScreen(t *testing.T) {
	r := startRig(t, "FAKE_DIALOG=1")
	r.type_("do it\r")
	r.waitLog(func(ev []map[string]string) bool { return hasState(ev, "dialog") }, "dialog")

	deadline := time.Now().Add(3 * time.Second)
	var snap state.Snapshot
	for time.Now().Before(deadline) {
		if snap = r.h.Snapshot(); snap.State == state.Dialog {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if snap.State != state.Dialog {
		t.Fatalf("state = %+v, want dialog", snap)
	}
	r.type_("y")
	deadline = time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && r.h.Snapshot().State == state.Dialog {
		time.Sleep(20 * time.Millisecond)
	}
	if r.h.Snapshot().State == state.Dialog {
		t.Error("still reporting dialog after it was answered")
	}
}

// The user's typing must arrive byte-for-byte even while messages are being
// injected around it.
func TestUserTypingIntactUnderInjectionStorm(t *testing.T) {
	r := startRig(t, "FAKE_BUSY_MS=1")
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; ctx.Err() == nil && i < 15; i++ {
			_ = r.h.Inject(ctx, "bg", InjectOptions{Paste: true, Submit: true})
			time.Sleep(10 * time.Millisecond)
		}
	}()

	typed := "abcdefghijklmnopqrstuvwxyz"
	for _, c := range typed {
		r.type_(string(c))
		time.Sleep(15 * time.Millisecond)
	}
	wg.Wait()
	cancel()
	time.Sleep(200 * time.Millisecond)

	var got strings.Builder
	for _, e := range r.events() {
		if e["ev"] == "char" {
			got.WriteString(e["c"])
		}
	}
	if got.String() != typed {
		t.Errorf("tool saw typed chars %q, want %q", got.String(), typed)
	}
}
