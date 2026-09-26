//go:build windows

package daemon

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"os"
	"testing"
	"time"

	"github.com/thesahibnanda-max/relay/internal/proto"
	"github.com/thesahibnanda-max/relay/internal/relayhome"
)

func tmpPaths(t *testing.T) relayhome.Paths {
	t.Helper()
	root, err := os.MkdirTemp("", "rl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	return relayhome.Paths{Root: root}
}

func quietLog() *slog.Logger { return slog.New(slog.NewTextHandler(io.Discard, nil)) }

// runInBackground starts Run and returns a stop func that cancels ctx and
// waits for Run to return - used only to prove ctx-cancellation still works
// (the SIGTERM-equivalent path); TestStopViaRPCActuallyStopsRunLoop below is
// what proves the RPC path this platform's real relay daemon stop uses.
func runInBackground(t *testing.T, paths relayhome.Paths) (stop func() error) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, paths, "vtest", quietLog()) }()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := Query(paths); err == nil {
			return func() error { cancel(); return <-done }
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	t.Fatalf("daemon did not come up: %v", <-done)
	return nil
}

func TestRunServesLocksAndCleansUp(t *testing.T) {
	paths := tmpPaths(t)
	stop := runInBackground(t, paths)

	st, err := Query(paths)
	if err != nil || st.Version != "vtest" || st.Proto != proto.Version || st.PID != os.Getpid() {
		t.Fatalf("status: %+v %v", st, err)
	}
	// Real ACL hardening (not a mode-bit check - meaningless on Windows) is
	// asserted directly in internal/relayhome's own tests and confirmed live
	// with icacls per the issue's live-verification phase; here we only
	// confirm the files this depends on actually exist.
	if _, err := os.Stat(paths.SocketPath()); err != nil {
		t.Errorf("socket: %v", err)
	}
	if _, err := os.Stat(paths.RunDir()); err != nil {
		t.Errorf("run dir: %v", err)
	}
	if _, err := os.Stat(paths.PidPath()); err != nil {
		t.Errorf("pid file: %v", err)
	}

	// a second daemon on the same home must refuse to start
	if err := Run(context.Background(), paths, "other", quietLog()); !errors.Is(err, ErrAlreadyRunning) {
		t.Errorf("second daemon: %v", err)
	}
	// ...and must not have disturbed the first
	if st, err := Query(paths); err != nil || st.Version != "vtest" {
		t.Fatalf("first daemon disturbed: %+v %v", st, err)
	}

	if err := stop(); err != nil {
		t.Fatal(err)
	}
	if _, err := Query(paths); err == nil {
		t.Error("daemon still answering after stop")
	}
	if _, err := os.Stat(paths.SocketPath()); !os.IsNotExist(err) {
		t.Errorf("socket not removed: %v", err)
	}
	if _, err := os.Stat(paths.PidPath()); !os.IsNotExist(err) {
		t.Errorf("pid file not removed: %v", err)
	}
	// the lock is released: a new daemon can start
	stop2 := runInBackground(t, paths)
	stop2()
}

func TestRunReplacesStaleSocketAfterCrash(t *testing.T) {
	paths := tmpPaths(t)
	paths.Ensure()
	// leftover socket file from a daemon that was killed
	os.WriteFile(paths.SocketPath(), nil, 0o600)
	stop := runInBackground(t, paths)
	defer stop()
	if _, err := Query(paths); err != nil {
		t.Fatal(err)
	}
}

func TestStopWithoutDaemon(t *testing.T) {
	if err := Stop(tmpPaths(t)); err == nil {
		t.Error("expected an error when nothing is running")
	}
}

// TestStopViaRPCActuallyStopsRunLoop is the Windows-specific test the RPC-
// based shutdown redesign exists for: Windows has no SIGTERM to send a
// detached daemon (confirmed live: syscall.Kill has no Windows
// implementation, and even taskkill without /F refuses to signal a
// console-less process), so Stop asks over the daemon's own admin API
// instead. This proves the RPC genuinely reaches and cancels the running
// Run() call - not just that the socket happens to stop answering.
func TestStopViaRPCActuallyStopsRunLoop(t *testing.T) {
	paths := tmpPaths(t)
	ctx := context.Background()
	done := make(chan error, 1)
	go func() { done <- Run(ctx, paths, "vtest", quietLog()) }()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := Query(paths); err == nil {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if err := Stop(paths); err != nil {
		t.Fatalf("Stop: %v", err)
	}
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("Run returned an error: %v", err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Run did not return after Stop: the RPC did not reach runCtx's cancel")
	}
	if _, err := Query(paths); err == nil {
		t.Error("daemon still answering after Stop")
	}
}

func TestOpenLogRotates(t *testing.T) {
	paths := tmpPaths(t)
	f, err := OpenLog(paths)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	if _, err := os.Stat(paths.DaemonLog()); err != nil {
		t.Errorf("log: %v", err)
	}
	big := make([]byte, maxLogSize+1)
	os.WriteFile(paths.DaemonLog(), big, 0o600)
	f, _ = OpenLog(paths)
	f.Close()
	if _, err := os.Stat(paths.DaemonLog() + ".1"); err != nil {
		t.Errorf("oversized log not rotated: %v", err)
	}
	if fi, _ := os.Stat(paths.DaemonLog()); fi.Size() != 0 {
		t.Errorf("new log should be empty, size %d", fi.Size())
	}
}
