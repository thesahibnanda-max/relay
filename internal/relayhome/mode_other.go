//go:build !unix && !windows

package relayhome

import "io/fs"

// hardenMode cannot do anything real on this platform.
func hardenMode(string, fs.FileInfo) error { return nil }
