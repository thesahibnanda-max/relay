//go:build windows

package agy

import (
	"errors"
	"io"
	"os"
	"time"

	"golang.org/x/sys/windows"
)

// ErrLockTimeout means another relay process held the agy MCP config lock for
// longer than the caller was willing to wait.
var ErrLockTimeout = errors.New("timed out waiting for the agy MCP config lock")

// winFileLock is an exclusive lock on a whole file, released automatically on
// process exit/crash exactly like a Unix flock - a direct copy of
// internal/daemon/lifecycle_windows.go's winLock, duplicated for the same
// reason as lock_unix.go's agyFlock.
type winFileLock struct {
	f  *os.File
	ov windows.Overlapped
}

func (l *winFileLock) Close() error {
	_ = windows.UnlockFileEx(windows.Handle(l.f.Fd()), 0, 1, 0, &l.ov)
	return l.f.Close()
}

func agyFlock(path string, wait time.Duration) (io.Closer, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}
	l := &winFileLock{f: f}
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
				return nil, ErrLockTimeout
			}
			return nil, err
		}
		time.Sleep(20 * time.Millisecond)
	}
}
