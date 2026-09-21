//go:build !unix

package relayhome

import "io/fs"

// Relay only runs on Unix-like systems; these keep the package compiling
// elsewhere (editors and tools analyse every platform).

// PIDAlive is conservative here: nothing is ever treated as stale.
func PIDAlive(pid int) bool { return pid > 0 }

// OwnedByCurrentUser cannot tell on this platform.
func OwnedByCurrentUser(fs.FileInfo) bool { return true }
