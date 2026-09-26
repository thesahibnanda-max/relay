//go:build unix

package doctor

import (
	"fmt"
	"os"
)

// privateEnough checks the same 0600/0700 mode bits Relay itself sets.
func privateEnough(path string) (bool, string) {
	st, err := os.Lstat(path)
	if err != nil {
		return true, "" // cannot tell: do not block
	}
	m := st.Mode().Perm()
	if m&0o077 != 0 {
		return false, fmt.Sprintf("%s is %#o", path, m)
	}
	return true, ""
}

func fixHint(root string) string {
	return "chmod -R go-rwx " + root + "  (relay repairs directories itself on next start)"
}
