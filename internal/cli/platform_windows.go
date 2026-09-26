//go:build windows

package cli

import (
	"os"
	"os/exec"
	"syscall"

	"github.com/thesahibnanda-max/relay/internal/agent"
)

// supported is whether relay can run agents on this platform.
const supported = true

// execReplace is Windows' substitute for syscall.Exec: Windows has no
// process-image-replacement syscall, so this spawns bin as a child with
// inherited stdio, waits for it, and exits with its exact status - the
// closest equivalent to "this process becomes that one". Like syscall.Exec,
// it never returns except on a launch failure (the one case its caller
// prints and reports as an error): a running/finished child, however it
// exited, always calls os.Exit here instead of returning to the caller.
func execReplace(bin string, argv, env []string) error {
	var cmd *exec.Cmd
	if agent.IsBatchFile(bin) {
		// bin is a .cmd/.bat shim: see agent.WrapForCmdExe for why a plain
		// argv (correct for a real .exe) is not safe here.
		cmdExe, cmdLine := agent.WrapForCmdExe(bin, argv[1:])
		cmd = exec.Command(cmdExe)
		cmd.SysProcAttr = &syscall.SysProcAttr{CmdLine: cmdLine}
	} else {
		cmd = exec.Command(bin, argv[1:]...)
	}
	cmd.Env = env
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		if cmd.ProcessState == nil {
			return err // never started: let the caller report this
		}
	}
	os.Exit(cmd.ProcessState.ExitCode())
	panic("unreachable")
}
