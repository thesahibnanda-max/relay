//go:build windows

package doctor

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/windows"
)

// privateEnough inspects the real DACL: os.Chmod/mode bits are meaningless
// on Windows (confirmed empirically that chmod 0600 leaves a file reading
// back as 0666, so a POSIX-style mode check would misfire on every file).
// Any ACE granting access to a trustee other than the file's own owner,
// SYSTEM or built-in Administrators (which, like root on Unix, can always
// override permissions regardless of the DACL) is a real leak.
func privateEnough(path string) (bool, string) {
	sd, err := windows.GetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.OWNER_SECURITY_INFORMATION)
	if err != nil {
		return true, "" // cannot tell: do not block
	}
	dacl, _, err := sd.DACL()
	if err != nil || dacl == nil {
		return true, ""
	}
	owner, _, _ := sd.Owner()
	systemSid, _ := windows.CreateWellKnownSid(windows.WinLocalSystemSid)
	adminsSid, _ := windows.CreateWellKnownSid(windows.WinBuiltinAdministratorsSid)

	for i := uint16(0); i < dacl.AceCount; i++ {
		var ace *windows.ACCESS_ALLOWED_ACE
		if windows.GetAce(dacl, uint32(i), &ace) != nil {
			continue
		}
		sid := (*windows.SID)(unsafe.Pointer(&ace.SidStart))
		if owner != nil && sid.Equals(owner) {
			continue
		}
		if systemSid != nil && sid.Equals(systemSid) {
			continue
		}
		if adminsSid != nil && sid.Equals(adminsSid) {
			continue
		}
		return false, fmt.Sprintf("%s grants access to %s", path, sid.String())
	}
	return true, ""
}

func fixHint(string) string {
	return "relay repairs directories itself on next start"
}
