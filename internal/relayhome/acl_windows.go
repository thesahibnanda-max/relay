//go:build windows

package relayhome

import (
	"fmt"

	"golang.org/x/sys/windows"
)

// SetPrivateACL replaces path's DACL with a single ACE granting the current
// user full control, and marks it protected so no broader ACE is inherited
// back from the parent directory. This is the real Windows equivalent of
// chmod 0600/0700: os.Chmod on Windows only toggles the read-only attribute
// bit (confirmed empirically: it succeeds but leaves a file reading back as
// 0666) and provides no actual access restriction at all.
func SetPrivateACL(path string) error {
	sid, err := currentUserSID()
	if err != nil {
		return fmt.Errorf("current user sid: %w", err)
	}
	ea := []windows.EXPLICIT_ACCESS{{
		AccessPermissions: windows.GENERIC_ALL,
		AccessMode:        windows.GRANT_ACCESS,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeValue: windows.TrusteeValueFromSID(sid),
		},
	}}
	dacl, err := windows.ACLFromEntries(ea, nil)
	if err != nil {
		return fmt.Errorf("build dacl: %w", err)
	}
	return windows.SetNamedSecurityInfo(
		path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION,
		nil, nil, dacl, nil,
	)
}
