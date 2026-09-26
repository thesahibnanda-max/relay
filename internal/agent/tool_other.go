//go:build !unix && !windows

package agent

import (
	"errors"
	"os"
)

// Relay only runs on Unix-like systems and Windows; this keeps the package
// compiling elsewhere (editors and tools analyse every platform).
var errUnsupported = errors.New("agent: not supported on this platform")

func startTool(string, []string, []string, bool, int, int) (tool, error) {
	return nil, errUnsupported
}

func eofSignal() []byte { return []byte{0x04} }

func winsize(*os.File) (int, int, bool) { return 0, 0, false }
