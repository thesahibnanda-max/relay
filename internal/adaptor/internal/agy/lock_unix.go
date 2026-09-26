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
var ErrLockTimeout = errors.New("timed out waiting for the agy MCP config lock")

// agyFlock takes an exclusive advisory lock on path, released when the
// returned closer is closed or the process dies - a direct copy of
// internal/daemon/lifecycle.go's flock. Duplicated rather than shared: it
// keeps this package self-contained and avoids widening the blast radius of
// daemon's already-tested lifecycle code for an unrelated caller.
func agyFlock(path string, wait time.Duration) (io.Closer, error) {
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
				return nil, ErrLockTimeout
			}
			return nil, err
		}
		time.Sleep(20 * time.Millisecond)
	}
}
