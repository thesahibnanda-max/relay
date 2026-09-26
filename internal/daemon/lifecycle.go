//go:build unix

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
	"strings"
	"syscall"
	"time"

	"github.com/thesahibnanda-max/relay/internal/proto"
	"github.com/thesahibnanda-max/relay/internal/relayhome"
)

var (
	ErrAlreadyRunning  = errors.New("relay daemon is already running")
	ErrVersionMismatch = errors.New("relay daemon speaks a different protocol version")
)

const maxLogSize = 10 << 20

// flock takes an exclusive advisory lock, returning the held file. The lock is
// released when the file is closed or the process dies, so a crashed daemon
// never leaves a stale lock behind.
func flock(path string, wait time.Duration) (*os.File, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	deadline := time.Now().Add(wait)
	for {
		err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
		if err == nil {
			return f, nil
		}
		if !errors.Is(err, syscall.EWOULDBLOCK) || time.Now().After(deadline) {
			f.Close()
			if errors.Is(err, syscall.EWOULDBLOCK) {
				return nil, ErrAlreadyRunning
			}
			return nil, err
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// OpenLog opens the daemon log (0600), rotating it once if it grew too large.
func OpenLog(paths relayhome.Paths) (*os.File, error) {
	if err := paths.Ensure(); err != nil {
		return nil, err
	}
	p := paths.DaemonLog()
	if st, err := os.Stat(p); err == nil && st.Size() > maxLogSize {
		_ = os.Rename(p, p+".1")
	}
	return os.OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
}

// Run is the body of `relay daemon`: it takes the single-instance lock,
// listens on the private socket and serves until ctx is cancelled.
func Run(ctx context.Context, paths relayhome.Paths, version string, log *slog.Logger) error {
	if err := paths.Ensure(); err != nil {
		return err
	}
	lock, err := flock(paths.LockPath(), 0)
	if err != nil {
		return err
	}
	defer lock.Close()

	if removed, err := paths.GC(nil); err == nil && len(removed) > 0 {
		log.Info("removed stale agent run directories", "count", len(removed))
	}

	sock := paths.SocketPath()
	_ = os.Remove(sock) // stale socket from a crashed daemon; we hold the lock so it is safe
	ln, err := net.Listen("unix", sock)
	if err != nil {
		return fmt.Errorf("listen %s: %w", sock, err)
	}
	if err := os.Chmod(sock, 0o600); err != nil {
		ln.Close()
		return err
	}
	if err := os.WriteFile(paths.PidPath(), []byte(strconv.Itoa(os.Getpid())+"\n"), 0o600); err != nil {
		ln.Close()
		return err
	}
	defer os.Remove(paths.PidPath())

	// runCtx additionally lets an RPC (Windows' Stop, which has no signal to
	// send a detached process) trigger the same shutdown path a SIGTERM does
	// here via ctx: on Unix this is a no-op wrapper around ctx, since Stop
	// still uses SIGTERM unchanged.
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
	sctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
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
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true} // survive this terminal closing
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

// Stop asks the running daemon to exit and waits for it.
func Stop(paths relayhome.Paths) error {
	data, err := os.ReadFile(paths.PidPath())
	if err != nil {
		if _, qerr := Query(paths); qerr != nil {
			return errors.New("relay daemon is not running")
		}
		return fmt.Errorf("daemon is running but %s is unreadable: %w", paths.PidPath(), err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 1 {
		return fmt.Errorf("bad pid file %s", paths.PidPath())
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil {
		if errors.Is(err, syscall.ESRCH) {
			os.Remove(paths.PidPath())
			return errors.New("relay daemon is not running")
		}
		return err
	}
	deadline := time.Now().Add(8 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := Query(paths); err != nil {
			return nil
		}
		time.Sleep(50 * time.Millisecond)
	}
	return fmt.Errorf("daemon (pid %d) did not stop in time", pid)
}
