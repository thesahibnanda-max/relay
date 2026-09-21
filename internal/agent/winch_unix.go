//go:build unix

package agent

import (
	"os"
	"syscall"
)

// resizeSignals are the signals that mean "the terminal was resized".
func resizeSignals() []os.Signal { return []os.Signal{syscall.SIGWINCH} }
