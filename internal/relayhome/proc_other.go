//go:build !unix && !windows

package relayhome

import "io/fs"

// Relay only runs on Unix-like systems and Windows; these keep the package
// compiling elsewhere (editors and tools analyse every platform).

// PIDAlive is conservative here: nothing is ever treated as stale.
func PIDAlive(pid int) bool { return pid > 0 }

// OwnedByCurrentUser cannot tell on this platform.
func OwnedByCurrentUser(string, fs.FileInfo) bool { return true }
