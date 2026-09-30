//go:build unix

package agent

import (
	"bytes"
	"os"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/creack/pty"
)

// A tool that dies with keyboard/cursor/paste modes still switched on (agy
// killed before it can restore them) must not leave the user's shell in them.
func TestModesLeftOnByADeadToolAreReset(t *testing.T) {
	user, tty, err := pty.Open()
	if err != nil {
		t.Skip("no pty:", err)
	}
	defer user.Close()
	defer tty.Close()
	var mu sync.Mutex
	var out bytes.Buffer
	go func() {
		buf := make([]byte, 4096)
		for {
			n, err := user.Read(buf)
			mu.Lock()
			out.Write(buf[:n])
			mu.Unlock()
			if err != nil {
				return
			}
		}
	}()
	code, err := Run(Config{Tool: "sh", Bin: "/bin/sh", Env: os.Environ(), In: tty, Out: tty,
		Args: []string{"-c", `printf '\033[?2004h\033[>1u\033[>4;2m\033[?25lbye'; kill -9 $$`}})
	if err != nil {
		t.Fatal(err)
	}
	if code == 0 {
		t.Fatalf("exit code %d, want the tool's own failure", code)
	}
	deadline := time.Now().Add(3 * time.Second)
	want := "\x1b[<1u\x1b[>4m\x1b[?2004l\x1b[?25h"
	for time.Now().Before(deadline) {
		mu.Lock()
		got := out.String()
		mu.Unlock()
		if i := strings.Index(got, "bye"); i >= 0 && strings.Contains(got[i:], want) {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	t.Fatalf("modes not reset after the tool died; terminal got %q", out.String())
}
