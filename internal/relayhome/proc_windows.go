//go:build windows

package relayhome

import (
	"io/fs"

	"golang.org/x/sys/windows"
)

// stillActive is Win32's STILL_ACTIVE pseudo exit code.
const stillActive = 259

// PIDAlive reports whether a process exists: opening it succeeds and reports
// a still-running exit code, or fails with access-denied (a process is
// there, just not one we can query - still "alive" for our purposes).
func PIDAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	h, err := windows.OpenProcess(windows.PROCESS_QUERY_LIMITED_INFORMATION, false, uint32(pid))
	if err != nil {
		return err == windows.ERROR_ACCESS_DENIED
	}
	defer windows.CloseHandle(h)
	var code uint32
	return windows.GetExitCodeProcess(h, &code) == nil && code == stillActive
}

// OwnedByCurrentUser compares path's owner SID (fetched fresh - Windows has
// no owner-uid field on a plain Stat/Lstat result the way Unix does) against
// the current process token's user SID. Conservative on any error: relay's
// callers already treat "cannot tell" as "do not block", matching
// proc_other.go's stance for every platform this can't be determined on.
func OwnedByCurrentUser(path string, _ fs.FileInfo) bool {
	want, err := currentUserSID()
	if err != nil {
		return true
	}
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT, windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return true
	}
	owner, _, err := sd.Owner()
	if err != nil || owner == nil {
		return true
	}
	return owner.Equals(want)
}

// currentUserSID is the current process token's user SID - the Windows
// equivalent of os.Getuid(), which always returns -1 on this platform and
// must never be relied on for a real identity/ownership check here.
func currentUserSID() (*windows.SID, error) {
	tok, err := windows.OpenCurrentProcessToken()
	if err != nil {
		return nil, err
	}
	defer tok.Close()
	u, err := tok.GetTokenUser()
	if err != nil {
		return nil, err
	}
	return u.User.Sid, nil
}
