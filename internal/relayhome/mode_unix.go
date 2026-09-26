//go:build unix

package relayhome

import (
	"io/fs"
	"os"
)

// hardenMode tightens d's mode to 0700 if it is not already - unchanged
// behavior, extracted from VerifyPrivateDir so Windows can substitute a real
// ACL-based equivalent instead.
func hardenMode(d string, st fs.FileInfo) error {
	if st.Mode().Perm() != 0o700 {
		return os.Chmod(d, 0o700)
	}
	return nil
}
