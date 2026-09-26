//go:build windows

package agent

import (
	"os"
	"strings"
	"testing"

	"github.com/thesahibnanda-max/relay/internal/intercept"
)

// TestStartToolForwardsOutputAndExitCode drives the real, exported agent.Run
// end-to-end against a real Windows command through the real ConPTY backend
// (tool_windows.go) - modeled on a proven reference implementation's own
// ConPTY test, adapted to relay's real API. cfg.In is deliberately not a
// terminal (isTTY=false), exactly mirroring how the Unix tests avoid needing
// a real pty on the user-terminal side while still exercising the tool's own
// pty/ConPTY path underneath (see tool_unix.go/tool_windows.go: isTTY only
// controls whether an initial size is set, a real pty/ConPTY is always used).
func TestStartToolForwardsOutputAndExitCode(t *testing.T) {
	in, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := os.CreateTemp(t.TempDir(), "out")
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()

	comspec := os.Getenv("COMSPEC")
	if comspec == "" {
		comspec = "cmd.exe"
	}
	code, err := Run(Config{
		Bin:         comspec,
		Args:        []string{"/d", "/c", "echo relay-windows-conpty"},
		Interceptor: intercept.Chain(nil),
		In:          in,
		Out:         out,
	})
	if err != nil {
		t.Fatal(err)
	}
	if code != 0 {
		t.Fatalf("exit code = %d", code)
	}
	b, err := os.ReadFile(out.Name())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(b), "relay-windows-conpty") {
		t.Fatalf("missing expected output, got %q", b)
	}
}

// TestStartToolSignalDoesNotErrorWithoutAProcessGroupTarget confirms Signal's
// GenerateConsoleCtrlEvent call does not itself error against a real,
// running child - the deeper question of whether the child actually reacts
// to CTRL_BREAK from a ConPTY-hosted process group is flagged in the issue's
// plan as needing live, real-hardware verification (a forced window-close),
// since that depends on console-group semantics this test cannot observe.
func TestStartToolSignalDoesNotErrorWithoutAProcessGroupTarget(t *testing.T) {
	in, err := os.Open(os.DevNull)
	if err != nil {
		t.Fatal(err)
	}
	defer in.Close()
	out, err := os.CreateTemp(t.TempDir(), "out")
	if err != nil {
		t.Fatal(err)
	}
	defer out.Close()

	comspec := os.Getenv("COMSPEC")
	if comspec == "" {
		comspec = "cmd.exe"
	}
	tl, err := startTool(comspec, []string{"/d", "/c", "timeout /t 1 >NUL"}, os.Environ(), false, 80, 24)
	if err != nil {
		t.Fatal(err)
	}
	defer tl.Close()
	if err := tl.Signal(os.Interrupt); err != nil {
		t.Errorf("Signal: %v", err)
	}
	_ = tl.Wait()
}
