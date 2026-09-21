//go:build unix

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
	if fi, err := os.Stat(paths.SocketPath()); err != nil || fi.Mode().Perm() != 0o600 {
		t.Errorf("socket: %v %v", fi, err)
	}
	if fi, _ := os.Stat(paths.RunDir()); fi.Mode().Perm() != 0o700 {
		t.Errorf("run dir mode %v", fi.Mode().Perm())
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

func TestOpenLogRotates(t *testing.T) {
	paths := tmpPaths(t)
	f, err := OpenLog(paths)
	if err != nil {
		t.Fatal(err)
	}
	f.Close()
	if fi, _ := os.Stat(paths.DaemonLog()); fi.Mode().Perm() != 0o600 {
		t.Errorf("log mode %v", fi.Mode().Perm())
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
