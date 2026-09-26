//go:build !unix && !windows

package adaptor

import (
	"os"
	"path/filepath"
)

// candidatesFor and isExecutable mirror the Unix behavior as the most
// conservative default on a genuinely unsupported platform.
func candidatesFor(dir, name string) []string {
	return []string{filepath.Join(dir, name)}
}

func isExecutable(info os.FileInfo) bool {
	return info.Mode().Perm()&0o111 != 0
}
