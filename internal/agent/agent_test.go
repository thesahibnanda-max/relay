package agent

import (
	"bytes"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/creack/pty"

	"github.com/thesahibnanda-max/relay/internal/intercept"
)

// recorder is an Interceptor that keeps copies of everything it sees while
// passing bytes through unchanged.
type recorder struct{ in, out bytes.Buffer }

func (r *recorder) Input(p []byte) []byte  { r.in.Write(p); return p }
func (r *recorder) Output(p []byte) []byte { r.out.Write(p); return p }

var _ intercept.Interceptor = (*recorder)(nil)

// run drives Run with a real PTY standing in for the user's terminal, so both
// the raw-mode and the resize paths execute. It returns what the "user" saw.
func run(t *testing.T, input string, bin string, args ...string) (code int, screen string, rec *recorder) {
	t.Helper()
	user, tty, err := pty.Open()
	if err != nil {
		t.Skip("no pty available:", err)
	}
	defer user.Close()
	defer tty.Close()

	rec = &recorder{}
	var (
		mu   sync.Mutex
		seen bytes.Buffer
	)
	go func() { // the user's screen: everything relay writes to its stdout
		buf := make([]byte, 4096)
		for {
			n, err := user.Read(buf)
			mu.Lock()
			seen.Write(buf[:n])
			mu.Unlock()
			if err != nil {
				return
			}
		}
	}()

	if input != "" {
		go user.Write([]byte(input))
	}

	code, err = Run(Config{Tool: "test", Bin: bin, Args: args, Env: os.Environ(), Interceptor: rec, In: tty, Out: tty})
	if err != nil {
		t.Fatal(err)
	}
	// Relay's input goroutine stays blocked reading tty (it dies with the real
	// process), so the slave never closes and we can't wait for EOF here.
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	return code, seen.String(), rec
}

func TestOutputPassesThroughExactly(t *testing.T) {
	// Colours, cursor movement and alternate-screen toggles must arrive untouched.
	const payload = "\x1b[?1049h\x1b[31mred\x1b[0m\x1b[2;3Hxy\x1b[?1049l"
	code, screen, rec := run(t, "", "/bin/sh", "-c", `printf '\033[?1049h\033[31mred\033[0m\033[2;3Hxy\033[?1049l'`)
	if code != 0 {
		t.Fatalf("exit code %d", code)
	}
	if rec.out.String() != payload {
		t.Errorf("interceptor saw %q, want %q", rec.out.String(), payload)
	}
	if !strings.Contains(screen, payload) {
		t.Errorf("user saw %q, want it to contain %q", screen, payload)
	}
}

func TestInputReachesTool(t *testing.T) {
	// Shift+Tab (ESC [ Z) and Up arrow (ESC [ A) are forwarded byte-for-byte.
	// `cat -v` renders them visibly, proving the tool received them.
	code, screen, rec := run(t, "\x1b[Z\x1b[A\n", "/bin/sh", "-c", `read l; printf '%s' "$l" | cat -v`)
	if code != 0 {
		t.Fatalf("exit code %d", code)
	}
	if rec.in.String() != "\x1b[Z\x1b[A\n" {
		t.Errorf("interceptor saw input %q", rec.in.String())
	}
	if !strings.Contains(screen, "^[[Z^[[A") {
		t.Errorf("tool did not receive keys; screen = %q", screen)
	}
}

func TestExitCodePropagates(t *testing.T) {
	if code, _, _ := run(t, "", "/bin/sh", "-c", "exit 42"); code != 42 {
		t.Errorf("code = %d, want 42", code)
	}
}

func TestSignalExitIs128Plus(t *testing.T) {
	if code, _, _ := run(t, "", "/bin/sh", "-c", "kill -TERM $$"); code != 128+15 {
		t.Errorf("code = %d, want 143", code)
	}
}

func TestPipedStdinWorks(t *testing.T) {
	// Non-TTY stdin: no raw mode, EOF is turned into Ctrl+D so `cat` finishes.
	r, w, _ := os.Pipe()
	out, err := os.CreateTemp(t.TempDir(), "out")
	if err != nil {
		t.Fatal(err)
	}
	go func() { w.WriteString("hello\n"); w.Close() }()

	rec := &recorder{}
	code, err := Run(Config{Tool: "test", Bin: "/bin/cat", Env: os.Environ(), Interceptor: rec, In: r, Out: out})
	if err != nil || code != 0 {
		t.Fatalf("code=%d err=%v", code, err)
	}
	b, _ := os.ReadFile(out.Name())
	if !strings.Contains(string(b), "hello") {
		t.Errorf("output %q missing hello", b)
	}
}

func TestMissingBinaryErrors(t *testing.T) {
	r, _, _ := os.Pipe()
	_, err := Run(Config{Bin: "/definitely/not/here", Env: os.Environ(), In: r, Out: os.Stderr})
	if err == nil {
		t.Error("expected an error for a missing binary")
	}
}
