//go:build !unix && !windows

package cli

import "errors"

// Relay needs a pseudo-terminal and unix sockets with peer credentials and
// process groups (Linux, macOS, WSL) or their Windows equivalents. This file
// only lets the code compile (and be analysed by editors) on other, genuinely
// unsupported platforms.
const supported = false

func execReplace(string, []string, []string) error {
	return errors.New("not supported on this platform")
}
