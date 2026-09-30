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
var ErrLockTimeout = errors.New("timed out waiting for another relay's agy MCP bookkeeping")

// winFileLock is an exclusive lock on a whole file, released automatically on
// process exit/crash exactly like a Unix flock.
type winFileLock struct {
	f  *os.File
	ov windows.Overlapped
}

func (l *winFileLock) Close() error {
	_ = windows.UnlockFileEx(windows.Handle(l.f.Fd()), 0, 1, 0, &l.ov)
	return l.f.Close()
}

// agyFlock locks path, (re)creating its directory with prepare first. An
// open file cannot be deleted on Windows (Go opens without
// FILE_SHARE_DELETE), so the file a waiter holds open is always the real one.
func agyFlock(path string, prepare func() error, wait time.Duration) (io.Closer, error) {
	deadline := time.Now().Add(wait)
	for {
		if err := prepare(); err != nil {
			return nil, err
		}
		f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			if (errors.Is(err, os.ErrNotExist) || errors.Is(err, windows.ERROR_ACCESS_DENIED)) && time.Now().Before(deadline) {
				time.Sleep(20 * time.Millisecond)
				continue // directory or file being removed right now
			}
			return nil, err
		}
		l := &winFileLock{f: f}
		for {
			err = windows.LockFileEx(windows.Handle(f.Fd()),
				windows.LOCKFILE_EXCLUSIVE_LOCK|windows.LOCKFILE_FAIL_IMMEDIATELY, 0, 1, 0, &l.ov)
			if err == nil || !errors.Is(err, windows.ERROR_LOCK_VIOLATION) || time.Now().After(deadline) {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if err != nil {
			f.Close()
			if errors.Is(err, windows.ERROR_LOCK_VIOLATION) {
				return nil, ErrLockTimeout
			}
			return nil, err
		}
		return l, nil
	}
}

// removeLockedFile is a no-op on Windows: an open file cannot be deleted.
func removeLockedFile(string) {}

// removeUnlockedFile deletes the lock file after it was closed; if another
// relay already opened it, deletion fails and the file simply stays in use.
func removeUnlockedFile(path string) { _ = os.Remove(path) }
