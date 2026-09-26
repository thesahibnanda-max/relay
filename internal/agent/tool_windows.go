//go:build windows

package agent

import (
	"os"
	"syscall"

	gopty "github.com/aymanbagabas/go-pty"
	"golang.org/x/sys/windows"
	"golang.org/x/term"
)

// windowsTool wraps a real ConPTY (via aymanbagabas/go-pty) and the process
// started under it.
type windowsTool struct {
	p   gopty.Pty
	cmd *gopty.Cmd
}

func startTool(bin string, args, env []string, isTTY bool, cols, rows int) (tool, error) {
	p, err := gopty.New()
	if err != nil {
		return nil, err
	}
	if isTTY {
		if err := p.Resize(cols, rows); err != nil {
			p.Close()
			return nil, err
		}
	}
	// A new process group so Signal's GenerateConsoleCtrlEvent below can
	// target only this child, never Relay's own process.
	sys := &syscall.SysProcAttr{CreationFlags: windows.CREATE_NEW_PROCESS_GROUP}
	var cmd *gopty.Cmd
	if IsBatchFile(bin) {
		// bin is a .cmd/.bat shim (every npm-installed CLI tool on Windows):
		// CreateProcess cannot launch that directly, and a plain argv (correct
		// for a real .exe, which is all the "else" branch below ever needs)
		// is not enough once cmd.exe's own tokenizer gets involved - see
		// WrapForCmdExe. Confirmed live: without this, a briefing/message
		// containing a `|` broke codex.cmd with a literal
		// "'from' is not recognized" error from cmd.exe.
		cmdExe, cmdLine := WrapForCmdExe(bin, args)
		sys.CmdLine = cmdLine
		cmd = p.Command(cmdExe)
	} else {
		cmd = p.Command(bin, args...)
	}
	cmd.Env = env
	cmd.SysProcAttr = sys
	if err := cmd.Start(); err != nil {
		p.Close()
		return nil, err
	}
	return &windowsTool{p: p, cmd: cmd}, nil
}

func (w *windowsTool) Read(b []byte) (int, error)  { return w.p.Read(b) }
func (w *windowsTool) Write(b []byte) (int, error) { return w.p.Write(b) }
func (w *windowsTool) Close() error                { return w.p.Close() }
func (w *windowsTool) Resize(cols, rows int) error { return w.p.Resize(cols, rows) }
func (w *windowsTool) Wait() error                 { return w.cmd.Wait() }

// Signal is the one place this whole abstraction exists for:
// (*os.Process).Signal only implements os.Kill on Windows, so a real
// termination signal aimed at Relay itself (SIGINT/SIGTERM) is translated
// into a targeted console-control event instead. CREATE_NEW_PROCESS_GROUP at
// spawn time (above) is what makes this target only the child: it gives the
// child its own group ID, equal to its PID.
func (w *windowsTool) Signal(s os.Signal) error {
	switch s {
	case syscall.SIGINT, syscall.SIGTERM:
		return windows.GenerateConsoleCtrlEvent(windows.CTRL_BREAK_EVENT, uint32(w.cmd.Process.Pid))
	default:
		return nil // SIGHUP never fires on Windows: nothing to forward
	}
}

func (w *windowsTool) ExitCode(waitErr error) int {
	if w.cmd.ProcessState == nil {
		if waitErr != nil {
			return 1
		}
		return 0
	}
	return w.cmd.ProcessState.ExitCode()
}

// eofSignal is what a foreground console process reading piped stdin sees as
// end-of-input on Windows: unlike Unix's Ctrl+D, a Windows console's
// canonical-mode reader recognises Ctrl+Z (0x1A) as EOF only as a complete
// line - i.e. followed by a line ending - confirmed live: sending bare 0x04
// (the Unix byte) into a real ConPTY-attached console reader does not signal
// EOF at all and hangs the reader waiting for more input.
func eofSignal() []byte { return []byte{0x1a, '\r', '\n'} }

// winsize reads the size of f, the user's controlling terminal.
func winsize(f *os.File) (int, int, bool) {
	c, r, err := term.GetSize(int(f.Fd()))
	if err != nil {
		return 0, 0, false
	}
	return c, r, true
}
