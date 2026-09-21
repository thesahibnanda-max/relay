//go:build unix

package relayhome

import (
	"errors"
	"io/fs"
	"os"
	"syscall"
)

// PIDAlive reports whether a process exists.
func PIDAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	err := syscall.Kill(pid, 0)
	return err == nil || errors.Is(err, syscall.EPERM)
}

// OwnedByCurrentUser reports whether a file belongs to the user running relay.
func OwnedByCurrentUser(st fs.FileInfo) bool {
	if sys, ok := st.Sys().(*syscall.Stat_t); ok {
		return int(sys.Uid) == os.Getuid()
	}
	return true
}
