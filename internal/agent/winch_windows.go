//go:build windows

package agent

import (
	"os"
	"time"
)

// resizeSignals is empty: Windows delivers no resize signal.
func resizeSignals() []os.Signal { return nil }

// pollInterval enables a resize-detection poll loop in Run, since there is no
// OS signal to wait on for a Windows console. 250ms matches the interval
// independently proven correct end-to-end in a real ConPTY reference
// implementation (spawn + resize + a real, passing output test).
func pollInterval() time.Duration { return 250 * time.Millisecond }
