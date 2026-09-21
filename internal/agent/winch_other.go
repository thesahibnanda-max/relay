//go:build !unix

package agent

import "os"

func resizeSignals() []os.Signal { return nil }
