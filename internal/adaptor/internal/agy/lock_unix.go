//go:build unix

package agy

import (
	"errors"
	"io"
	"os"
	"syscall"
	"time"
)

// ErrLockTimeout means another relay process held the agy MCP config lock for
// longer than the caller was willing to wait.
var ErrLockTimeout = errors.New("timed out waiting for another relay's agy MCP bookkeeping")

// agyFlock takes an exclusive advisory lock on path (released when the
// returned closer is closed or the process dies). prepare (re)creates the
// directory before every open. The lock file may be unlinked by its holder
// once relay has nothing left registered, so after locking, the inode is
// checked against the path: a lock on an unlinked file is worthless and is
// retried on a fresh one.
func agyFlock(path string, prepare func() error, wait time.Duration) (io.Closer, error) {
	deadline := time.Now().Add(wait)
	for {
		if err := prepare(); err != nil {
			return nil, err
		}
		f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) && time.Now().Before(deadline) {
				continue // directory removed between prepare and open
			}
			return nil, err
		}
		for {
			err = syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB)
			if err == nil || !errors.Is(err, syscall.EWOULDBLOCK) || time.Now().After(deadline) {
				break
			}
			time.Sleep(20 * time.Millisecond)
		}
		if err != nil {
			f.Close()
			if errors.Is(err, syscall.EWOULDBLOCK) {
				return nil, ErrLockTimeout
			}
			return nil, err
		}
		if sameFile(f, path) {
			return f, nil
		}
		f.Close() // the holder unlinked it: lock the current file instead
	}
}

func sameFile(f *os.File, path string) bool {
	a, err := f.Stat()
	if err != nil {
		return false
	}
	b, err := os.Stat(path)
	if err != nil {
		return false
	}
	return os.SameFile(a, b)
}

// removeLockedFile unlinks the lock file while it is still held: every
// acquirer re-checks the inode, so nobody can be left holding a stale lock.
func removeLockedFile(path string) { _ = os.Remove(path) }

// removeUnlockedFile is a no-op on Unix: removeLockedFile already did it.
func removeUnlockedFile(string) {}
