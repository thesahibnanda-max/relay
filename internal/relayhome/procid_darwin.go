package relayhome

import (
	"fmt"

	"golang.org/x/sys/unix"
)

// ProcessIdentity names process pid by its start time, or "" if it is not
// running or cannot be read. A reused pid gets a different one.
func ProcessIdentity(pid int) string {
	if pid <= 0 {
		return ""
	}
	kp, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil || int(kp.Proc.P_pid) != pid || kp.Proc.P_stat == 5 { // 5: SZOMB
		return ""
	}
	t := kp.Proc.P_starttime
	return fmt.Sprintf("%d@%d.%06d", pid, t.Sec, t.Usec)
}
