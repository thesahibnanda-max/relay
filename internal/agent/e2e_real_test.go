//go:build e2e_real

// Opt-in tests against the REAL claude and codex binaries (they spend model
// quota). Run with:
//
//	env -u CODEX_YOLO -u CLAUDECODE go test -tags e2e_real -run Real -v -timeout 300s ./internal/agent
package agent

import (
	"context"
	"os"
	"os/exec"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/creack/pty"

	"github.com/thesahibnanda-max/relay/internal/state"
)

// terminalReplies are the answers a real terminal (Warp, seen in the logs)
// gives to the probes Claude/Codex send at startup. Without them Codex waits
// on unanswered queries and can swallow early input. Kitty keyboard (ESC[?u)
// is deliberately left unanswered, as Warp does.
var terminalReplies = map[string]string{
	"\x1b[6n":         "\x1b[1;1R",
	"\x1b[c":          "\x1b[?62c",
	"\x1b[0c":         "\x1b[?62c",
	"\x1b]10;?\x1b\\": "\x1b]10;rgb:fafa/f9f9/f6f6\x1b\\",
	"\x1b]11;?\x1b\\": "\x1b]11;rgb:1212/1212/1212\x1b\\",
	"\x1b]10;?\x07":   "\x1b]10;rgb:fafa/f9f9/f6f6\x07",
	"\x1b]11;?\x07":   "\x1b]11;rgb:1212/1212/1212\x07",
}

type realRig struct {
	t    *testing.T
	h    *Handle
	user *os.File
	done chan int
	mu   sync.Mutex
	raw  strings.Builder
}

func startReal(t *testing.T, bin string, args ...string) *realRig {
	t.Helper()
	path, err := exec.LookPath(bin)
	if err != nil {
		t.Skipf("%s not installed", bin)
	}
	user, tty, err := pty.Open()
	if err != nil {
		t.Skip("no pty:", err)
	}
	_ = pty.Setsize(tty, &pty.Winsize{Rows: 45, Cols: 140})
	r := &realRig{t: t, user: user, done: make(chan int, 1)}
	go func() { // the user's terminal: record output and answer capability queries like a real one
		buf := make([]byte, 8192)
		var tail string
		for {
			n, err := user.Read(buf)
			r.mu.Lock()
			r.raw.Write(buf[:n])
			r.mu.Unlock()
			tail += string(buf[:n])
			for q, reply := range terminalReplies {
				if c := strings.Count(tail, q); c > 0 {
					for i := 0; i < c; i++ {
						user.WriteString(reply)
					}
					tail = strings.ReplaceAll(tail, q, "")
				}
			}
			if len(tail) > 256 {
				tail = tail[len(tail)-64:]
			}
			if err != nil {
				return
			}
		}
	}()
	ready := make(chan *Handle, 1)
	go func() {
		code, _ := Run(Config{Tool: bin, Bin: path, Args: args, Env: os.Environ(), In: tty, Out: tty,
			OnStart: func(h *Handle) { ready <- h }})
		r.done <- code
	}()
	select {
	case r.h = <-ready:
	case <-time.After(10 * time.Second):
		t.Fatal("did not start")
	}
	t.Cleanup(func() {
		r.user.Write([]byte{0x03})
		time.Sleep(400 * time.Millisecond)
		r.user.Write([]byte{0x03})
		select {
		case <-r.done:
		case <-time.After(5 * time.Second):
		}
	})
	return r
}

func (r *realRig) screenText() string { return strings.Join(r.h.Screen(), "\n") }

func (r *realRig) waitScreen(d time.Duration, what string, ok func(string) bool) string {
	r.t.Helper()
	end := time.Now().Add(d)
	for time.Now().Before(end) {
		if s := r.screenText(); ok(s) {
			return s
		}
		time.Sleep(200 * time.Millisecond)
	}
	r.t.Fatalf("timed out waiting for %s. Screen:\n%s", what, r.screenText())
	return ""
}

// waitIdle waits for the tool to finish drawing its first prompt.
func (r *realRig) waitIdle() {
	r.t.Helper()
	time.Sleep(3 * time.Second)
	end := time.Now().Add(20 * time.Second)
	for time.Now().Before(end) {
		if s := r.h.Snapshot(); s.State == state.Idle && s.BracketedPaste {
			return
		}
		time.Sleep(300 * time.Millisecond)
	}
	r.t.Fatalf("tool never looked idle with bracketed paste on: %+v\n%s", r.h.Snapshot(), r.screenText())
}

func hasReplyLine(screen, want string) bool {
	for _, l := range strings.Split(screen, "\n") {
		l = strings.TrimSpace(l)
		l = strings.TrimLeft(l, "●•⏺ ")
		if l == want {
			return true
		}
	}
	return false
}

func realCases() []struct {
	name string
	bin  string
	args []string
} {
	return []struct {
		name string
		bin  string
		args []string
	}{
		{"claude", "claude", []string{"--model", "haiku"}},
		{"codex", "codex", nil},
		// --allow-all is only ever used here, in this adaptor's own opt-in
		// real-binary tests: bypassing every dialog would defeat the whole
		// point of Relay's human-in-the-loop orchestration on a real launch.
		{"copilot", "copilot", []string{"--allow-all"}},
	}
}

// The core M3 mechanism: text injected while the tool sits at its prompt is
// received exactly like typed input and produces a normal reply.
func TestRealInjectWhenIdle(t *testing.T) {
	for _, c := range realCases() {
		t.Run(c.name, func(t *testing.T) {
			r := startReal(t, c.bin, c.args...)
			r.waitIdle()
			t.Logf("state before inject: %+v", r.h.Snapshot())
			if err := r.h.Inject(context.Background(), "What is 21 times 2? Reply with only the number. Use no tools.", InjectOptions{Paste: true, Submit: true}); err != nil {
				t.Fatal(err)
			}
			s := r.waitScreen(90*time.Second, "reply 42", func(s string) bool { return hasReplyLine(s, "42") })
			t.Logf("got reply; final state %+v", r.h.Snapshot())
			_ = s
		})
	}
}

// Multi-line text must arrive as ONE message, not one message per line.
func TestRealMultilineInjectIsOneMessage(t *testing.T) {
	for _, c := range realCases() {
		t.Run(c.name, func(t *testing.T) {
			r := startReal(t, c.bin, c.args...)
			r.waitIdle()
			text := "Quick arithmetic question in three lines.\nLine two: let X equal 29 plus 29.\nLine three: reply with only the value of X. Use no tools."
			if err := r.h.Inject(context.Background(), text, InjectOptions{Paste: true, Submit: true}); err != nil {
				t.Fatal(err)
			}
			r.waitScreen(90*time.Second, "reply 58", func(s string) bool { return hasReplyLine(s, "58") })
		})
	}
}

// Observational: what does each tool do with a message injected mid-turn?
func TestRealInjectWhileBusy(t *testing.T) {
	for _, c := range realCases() {
		t.Run(c.name, func(t *testing.T) {
			r := startReal(t, c.bin, c.args...)
			r.waitIdle()
			_ = r.h.Inject(context.Background(), "Count from 1 to 40, one number per line, then stop. Use no tools.", InjectOptions{Paste: true, Submit: true})
			r.waitScreen(60*time.Second, "turn to start", func(s string) bool {
				return hasReplyLine(s, "1") || hasReplyLine(s, "2")
			})
			t.Logf("busy state: %+v", r.h.Snapshot())
			t0 := time.Now()
			if err := r.h.Inject(context.Background(), "Reply with only the number 77. Use no tools.", InjectOptions{Paste: true, Submit: true}); err != nil {
				t.Fatal(err)
			}
			r.waitScreen(120*time.Second, "reply 77 (queued or interleaved)", func(s string) bool { return hasReplyLine(s, "77") })
			t.Logf("mid-turn message was answered %v after injection", time.Since(t0).Round(time.Millisecond))
			if !hasReplyLine(r.screenText(), "40") {
				t.Logf("NOTE: the count never reached 40 before 77 was answered => tool interrupted/reordered")
			}
		})
	}
}
