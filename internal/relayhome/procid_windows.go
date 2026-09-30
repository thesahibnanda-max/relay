package relayhome

import (
	"fmt"

	"golang.org/x/sys/windows"
)

// ProcessIdentity names process pid by its creation time, or "" if it is
// not running or cannot be read. A reused pid gets a different one.
func ProcessIdentity(pid int) string {
	if pid <= 0 || !PIDAlive(pid) {
		return ""
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return ""
	}
	defer windows.CloseHandle(h)
	var created, exited, kernel, user windows.Filetime
	if windows.GetProcessTimes(h, &created, &exited, &kernel, &user) != nil {
		return ""
	}
	return fmt.Sprintf("%d@%d", pid, created.Nanoseconds())
}
