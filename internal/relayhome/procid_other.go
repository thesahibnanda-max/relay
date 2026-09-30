//go:build !linux && !darwin && !windows

package relayhome

// ProcessIdentity cannot tell on this platform: "" (unknown).
func ProcessIdentity(int) string { return "" }
