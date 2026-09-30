package relayhome

import (
	"os"
	"os/exec"
	"runtime"
	"testing"
)

// A process is known by its pid and start time: a pid reused after a crash
// never passes for the process that held it.
func TestProcessIdentity(t *testing.T) {
	switch runtime.GOOS {
	case "linux", "darwin", "windows":
	default:
		t.Skip("no process start time on", runtime.GOOS)
	}
	me := ProcessIdentity(os.Getpid())
	if me == "" || ProcessIdentity(os.Getpid()) != me {
		t.Fatalf("identity %q is empty or unstable", me)
	}
	exe, _ := os.Executable()
	cmd := exec.Command(exe, "-test.run=^$")
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	child := ProcessIdentity(cmd.Process.Pid)
	cmd.Wait()
	if child == "" || child == me {
		t.Fatalf("child identity %q (mine %q)", child, me)
	}
	if got := ProcessIdentity(cmd.Process.Pid); got == child {
		t.Fatalf("a finished process still has identity %q", got)
	}
	if ProcessIdentity(-1) != "" {
		t.Fatal("identity for an invalid pid")
	}
}
