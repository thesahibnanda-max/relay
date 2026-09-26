//go:build !unix && !windows

package doctor

import (
	"fmt"
	"os"
)

// privateEnough mirrors the Unix mode-bit check as the most conservative
// default on a genuinely unsupported platform.
func privateEnough(path string) (bool, string) {
	st, err := os.Lstat(path)
	if err != nil {
		return true, ""
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
