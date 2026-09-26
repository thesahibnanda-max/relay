//go:build !windows

package relayhome

// SetPrivateACL is a Windows-only concept; on every other platform mode bits
// already do this job (see mode_unix.go/mode_other.go). This stub exists so
// callers that are guarded by a runtime.GOOS=="windows" check still compile
// everywhere.
func SetPrivateACL(string) error { return nil }
