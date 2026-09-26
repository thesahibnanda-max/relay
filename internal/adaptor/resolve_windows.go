//go:build windows

package adaptor

import (
	"os"
	"path/filepath"
	"strings"
)

// defaultPathExt mirrors Windows' own documented default when %PATHEXT% is
// unset or empty.
const defaultPathExt = ".COM;.EXE;.BAT;.CMD;.VBS;.VBE;.JS;.JSE;.WSF;.WSH;.MSC"

// candidatesFor yields name with every %PATHEXT% extension appended (falling
// back to Windows' own documented default list), plus the bare name last in
// case PATH already holds a fully-qualified entry (e.g. "claude.exe"):
// Windows determines "is this executable" by extension, not a permission
// bit, so this - not a mode check - is what finding claude.exe/codex.exe/
// copilot.exe/agy.exe on PATH actually depends on.
func candidatesFor(dir, name string) []string {
	pathext := os.Getenv("PATHEXT")
	if pathext == "" {
		pathext = defaultPathExt
	}
	var out []string
	for _, ext := range strings.Split(pathext, ";") {
		if ext == "" {
			continue
		}
		out = append(out, filepath.Join(dir, name+ext))
	}
	return append(out, filepath.Join(dir, name))
}

// isExecutable is unconditional: Windows has no separate per-file executable
// bit to check (a regular file's Mode().Perm() is always 0444 or 0666
// regardless of whether it is runnable) - candidatesFor already did the real
// work of deciding "is this the kind of file Windows would run" by
// extension, so anything that exists at one of those candidate paths counts.
func isExecutable(os.FileInfo) bool { return true }
