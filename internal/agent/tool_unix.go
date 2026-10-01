//go:build unix

package agent

import (
	"os"
	"os/exec"
	"syscall"

	"github.com/creack/pty"
	"golang.org/x/sys/unix"
)

// unixTool wraps a real Unix pty (creack/pty) and the *exec.Cmd started
// under it - a pure, behavior-preserving extraction of what used to be
// inline in Run: every call here matches exactly what this file's code did
// before the tool interface existed.
type unixTool struct {
	cmd  *exec.Cmd
	ptmx *os.File
}

func startTool(bin string, args, env []string, isTTY bool, cols, rows int) (tool, error) {
	cmd := exec.Command(bin, args...)
	cmd.Env = env
	var ptmx *os.File
	var err error
	if isTTY {
		ptmx, err = pty.StartWithSize(cmd, &pty.Winsize{Rows: uint16(rows), Cols: uint16(cols)})
	} else {
		ptmx, err = pty.Start(cmd)
	}
	if err != nil {
		return nil, err
	}
	return &unixTool{cmd: cmd, ptmx: ptmx}, nil
}

func (u *unixTool) Read(p []byte) (int, error)  { return u.ptmx.Read(p) }
func (u *unixTool) Write(p []byte) (int, error) { return u.ptmx.Write(p) }
func (u *unixTool) Close() error                { return u.ptmx.Close() }

func (u *unixTool) Resize(cols, rows int) error {
	return pty.Setsize(u.ptmx, &pty.Winsize{Rows: uint16(rows), Cols: uint16(cols)})
}

// canonical reports whether the tool's terminal is still in canonical (line)
// mode, read through the pty master (which reports the tool's side); ok is
// false if it cannot be read. Through SyscallConn, not Fd, which would switch
// the master to blocking mode and stop Close from ending a pending Read.
func (u *unixTool) canonical() (canon, ok bool) {
	rc, err := u.ptmx.SyscallConn()
	if err != nil {
		return false, false
	}
	var tio *unix.Termios
	var terr error
	if err := rc.Control(func(fd uintptr) { tio, terr = unix.IoctlGetTermios(int(fd), ioctlReadTermios) }); err != nil || terr != nil {
		return false, false
	}
	return tio.Lflag&unix.ICANON != 0, true
}

func (u *unixTool) Wait() error              { return u.cmd.Wait() }
func (u *unixTool) Signal(s os.Signal) error { return u.cmd.Process.Signal(s) }

func (u *unixTool) ExitCode(waitErr error) int {
	if u.cmd.ProcessState == nil {
		if waitErr != nil {
			return 1
		}
		return 0
	}
	if ws, ok := u.cmd.ProcessState.Sys().(syscall.WaitStatus); ok && ws.Signaled() {
		return 128 + int(ws.Signal())
	}
	return u.cmd.ProcessState.ExitCode()
}

// eofSignal is what a foreground process reading from a console/pty sees as
// end-of-input when piped stdin runs out: on Unix the pty line discipline
// turns byte 0x04 (Ctrl+D) at the start of a line into EOF for a cooked
// reader.
func eofSignal() []byte { return []byte{0x04} }

// winsize reads the size of f via the same ioctl creack/pty already used
// inline before - unchanged behavior.
func winsize(f *os.File) (int, int, bool) {
	ws, err := pty.GetsizeFull(f)
	if err != nil {
		return 0, 0, false
	}
	return int(ws.Cols), int(ws.Rows), true
}
