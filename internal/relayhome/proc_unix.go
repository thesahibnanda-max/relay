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

// OwnedByCurrentUser reports whether a file belongs to the user running
// relay. path is unused here (Unix gets the owner uid straight from st) but
// is part of the signature since Windows needs a fresh syscall against the
// path to get an owner SID at all.
func OwnedByCurrentUser(path string, st fs.FileInfo) bool {
	if sys, ok := st.Sys().(*syscall.Stat_t); ok {
		return int(sys.Uid) == os.Getuid()
	}
	return true
}
