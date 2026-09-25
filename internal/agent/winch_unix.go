//go:build unix

package agent

import (
	"os"
	"syscall"
	"time"
)

// resizeSignals are the signals that mean "the terminal was resized".
func resizeSignals() []os.Signal { return []os.Signal{syscall.SIGWINCH} }

// pollInterval is 0 (disabled): SIGWINCH already tells us exactly when to re-check.
func pollInterval() time.Duration { return 0 }
