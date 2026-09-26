//go:build !unix && !windows

package agent

import (
	"os"
	"time"
)

func resizeSignals() []os.Signal { return nil }

func pollInterval() time.Duration { return 0 }
