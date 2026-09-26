//go:build windows

package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"os"
	"os/exec"
	"strconv"
	"syscall"
	"time"

	"golang.org/x/sys/windows"

	"github.com/thesahibnanda-max/relay/internal/proto"
	"github.com/thesahibnanda-max/relay/internal/relayhome"
)

var (
	ErrAlreadyRunning  = errors.New("relay daemon is already running")
	ErrVersionMismatch = errors.New("relay daemon speaks a different protocol version")
)

const maxLogSize = 10 << 20

// winLock is an exclusive lock on a whole file, released automatically on
// process exit/crash exactly like a Unix flock - the Windows equivalent used
// for both the daemon's single-instance lock and the spawn-serialising lock.
type winLock struct {
	f  *os.File
	ov windows.Overlapped
}

func flock(path string, wait time.Duration) (*winLock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	l := &winLock{f: f}
	deadline := time.Now().Add(wait)
	for {
		err := windows.LockFileEx(windows.Handle(f.Fd()),
			windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &l.ov)
		if err == nil {
			return l, nil
		}
		if !errors.Is(err, windows.ERROR_LOCK_VIOLATION) || time.Now().After(deadline) {
			f.Close()
			if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
				return nil, ErrAlreadyRunning
			}
			return nil, err
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (l *winLock) Close() error {
	_ = windows.UnlockFileEx(windows.Handle(l.f.Fd()), 0, 1, 0, &l.ov)
	return l.f.Close()
}

// OpenLog opens the daemon log (hardened with a real ACL, since 0600 does
// nothing on Windows), rotating it once if it grew too large.
func OpenLog(paths relayhome.Paths) (*os.File, error) {
	if err := paths.Ensure(); err != nil {
		return nil, err
	}
	p := paths.DaemonLog()
	if st, err := os.Stat(p); err == nil && st.Size() > maxLogSize {
		_ = os.Rename(p, p+".1")
	}
	f, err := os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return nil, err
	}
	_ = relayhome.SetPrivateACL(p)
	return f, nil
}

// Run is the body of `relay daemon`: it takes the single-instance lock,
// listens on the private socket and serves until ctx is cancelled or a
// POST /v1/admin/shutdown RPC arrives (Windows has no SIGTERM to send a
// detached process, so lifecycle.Stop uses that RPC instead - see Stop below
// and server.go's Options.OnShutdownRequested).
func Run(ctx context.Context, paths relayhome.Paths, version string, log *slog.Logger) error {
	if err := paths.Ensure(); err != nil {
		return err
	}
	lock, err := flock(paths.LockPath(), 0)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := relayhome.SetPrivateACL(paths.LockPath()); err != nil {
		log.Warn("could not harden lock file ACL", "err", err)
	}

	if removed, err := paths.GC(nil); err == nil && len(removed) > 0 {
		log.Info("removed stale agent run directories", "count", len(removed))
	}

	sock := paths.SocketPath()
	_ = os.Remove(sock)
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return fmt.Errorf("listen %s: %w", sock, err)
	}
	if err := relayhome.SetPrivateACL(sock); err != nil {
		ln.Close()
		return fmt.Errorf("harden socket ACL: %w", err)
	}
	if err := os.WriteFile(paths.PidPath(), []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600); err != nil {
		ln.Close()
		return err
	}
	if err := relayhome.SetPrivateACL(paths.PidPath()); err != nil {
		log.Warn("could not harden pid file ACL", "err", err)
	}
	defer os.Remove(paths.PidPath())

	runCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	srv, err := New(Options{Paths: paths, Version: version, Log: log, OnShutdownRequested: cancel})
	if err != nil {
		ln.Close()
		os.Remove(sock)
		return err
	}
	log.Info("relayd started", "version", version, "socket", sock, "pid", os.Getpid())

	errc := make(chan error, 1)
	go func() { errc <- srv.Serve(ln) }()
	select {
	case <-runCtx.Done():
	case err = <-errc:
		log.Error("server stopped", "err", err)
	}
	sctx, scancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer scancel()
	serr := srv.Shutdown(sctx)
	os.Remove(sock)
	log.Info("relayd stopped")
	if err != nil {
		return err
	}
	return serr
}

// Query asks a running daemon for its status; err is non-nil if none answers.
func Query(paths relayhome.Paths) (*proto.Status, error) {
	c := proto.HTTPClient(paths.SocketPath())
	c.Timeout = 2 * time.Second
	resp, err := c.Get("http://relay/v1/admin/status")
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	var st proto.Status
	if err := json.NewDecoder(resp.Body).Decode(&st); err != nil {
		return nil, err
	}
	return &st, nil
}

func compatible(st *proto.Status) error {
	if st.Proto != proto.Version {
		return fmt.Errorf("%w (daemon v%d, this relay v%d): run `relay daemon stop`, then retry", ErrVersionMismatch, st.Proto, proto.Version)
	}
	return nil
}

// Ensure returns a running, compatible daemon, starting one (detached) if
// none answers. exe is the relay binary to launch as `exe daemon`.
func Ensure(ctx context.Context, paths relayhome.Paths, exe string) (*proto.Status, error) {
	if st, err := Query(paths); err == nil {
		return st, compatible(st)
	}
	if err := paths.Ensure(); err != nil {
		return nil, err
	}
	// Serialise concurrent starters (e.g. two terminals opened together).
	spawn, err := flock(paths.SpawnLockPath(), 10*time.Second)
	if err != nil {
		return nil, fmt.Errorf("waiting to start the daemon: %w", err)
	}
	defer spawn.Close()
	if st, err := Query(paths); err == nil { // someone else won the race
		return st, compatible(st)
	}

	devnull, err := os.OpenFile(os.DevNull, os.O_RDWR, 0)
	if err != nil {
		return nil, err
	}
	defer devnull.Close()
	cmd := exec.Command(exe, "daemon")
	cmd.Env = os.Environ()
	cmd.Stdin, cmd.Stdout, cmd.Stderr = devnull, devnull, devnull
	// The Windows substitute for Setsid: detach from this console (survive
	// it closing) and give the daemon its own process group.
	cmd.SysProcAttr = &syscall.SysProcAttr{CreationFlags: windows.DETACHED_PROCESS | windows.CREATE_NEW_PROCESS_GROUP}
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start daemon: %w", err)
	}
	_ = cmd.Process.Release()

	deadline := time.Now().Add(6 * time.Second)
	for time.Now().Before(deadline) {
		if st, err := Query(paths); err == nil {
			return st, compatible(st)
		}
		select {
		case <-ctx.Done():
			return nil, ctx.Err()
		case <-time.After(30 * time.Millisecond):
		}
	}
	return nil, fmt.Errorf("daemon did not start; see %s", paths.DaemonLog())
}

// Stop asks the running daemon to exit over the same admin API every other
// admin command uses, and waits for it: Windows has no polite process-
// external signal deliverable to a detached process (confirmed: syscall.Kill
// has no Windows implementation, and even taskkill without /F refuses to
// signal a console-less process) - see server.go's handleShutdown, which
// triggers the identical internal shutdown path Unix's SIGTERM does. No
// escalation to a hard kill if the daemon does not stop in time, for exact
// parity with Unix's own Stop, which does not escalate either.
func Stop(paths relayhome.Paths) error {
	if _, err := Query(paths); err != nil {
		return errors.New("relay daemon is not running")
	}
	c := proto.HTTPClient(paths.SocketPath())
	c.Timeout = 5 * time.Second
	resp, err := c.Post("http://relay/v1/admin/shutdown", "application/json", nil)
	if err != nil {
		return fmt.Errorf("asking the daemon to stop: %w", err)
	}
	resp.Body.Close()
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := Query(paths); err != nil {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("daemon did not stop in time")
}
