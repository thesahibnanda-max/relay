//go:build unix

package cli

import "syscall"

// supported is whether relay can run agents on this platform.
const supported = true

// execReplace replaces the current process with the tool (used when relay is
// already running inside a relay session, so no second layer is stacked).
func execReplace(bin string, argv, env []string) error { return syscall.Exec(bin, argv, env) }
