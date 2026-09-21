//go:build !unix

package cli

import "errors"

// Relay needs Unix pseudo-terminals, unix sockets with peer credentials and
// process groups: Linux, macOS and WSL. This file only lets the code compile
// (and be analysed by editors) on other platforms.
const supported = false

func execReplace(string, []string, []string) error {
	return errors.New("not supported on this platform")
}
