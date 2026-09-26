//go:build windows

package relayhome

import "io/fs"

// hardenMode sets a real, owner-only DACL: os.Chmod on Windows cannot
// express "owner-only" at all (see SetPrivateACL), so mode bits are not
// checked here - a Windows directory is always (re-)hardened.
func hardenMode(d string, _ fs.FileInfo) error {
	return SetPrivateACL(d)
}
