//go:build unix

package adaptor

import (
	"os"
	"path/filepath"
)

// candidatesFor is just the bare name on Unix: there is no extension convention.
func candidatesFor(dir, name string) []string {
	return []string{filepath.Join(dir, name)}
}

// isExecutable checks the owner/group/other execute bits, exactly as before.
func isExecutable(info os.FileInfo) bool {
	return info.Mode().Perm()&0o111 != 0
}
