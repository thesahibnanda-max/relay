//go:build windows

package doctor

import (
	"strings"
	"testing"

	"golang.org/x/sys/windows"
)

// widenACL grants Everyone full control of path, alongside whatever ACE is
// already there - the real equivalent of the os.Chmod(0o755) attack
// TestOpenPermissionsAreAFailure uses on Unix, which does nothing on
// Windows (confirmed elsewhere: chmod there only ever touches the read-only
// attribute, never the real ACL).
func widenACL(t *testing.T, path string) {
	t.Helper()
	everyone, err := windows.CreateWellKnownSid(windows.WinWorldSid)
	if err != nil {
		t.Fatal(err)
	}
	ea := []windows.EXPLICIT_ACCESS{{
		AccessPermissions: windows.GENERIC_ALL,
		AccessMode:        windows.GRANT_ACCESS,
		Trustee: windows.TRUSTEE{
			TrusteeForm:  windows.TRUSTEE_IS_SID,
			TrusteeValue: windows.TrusteeValueFromSID(everyone),
		},
	}}
	dacl, err := windows.ACLFromEntries(ea, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := windows.SetNamedSecurityInfo(path, windows.SE_FILE_OBJECT,
		windows.DACL_SECURITY_INFORMATION|windows.PROTECTED_DACL_SECURITY_INFORMATION, nil, nil, dacl, nil); err != nil {
		t.Fatal(err)
	}
}

// TestOpenACLIsAFailure is the Windows equivalent of TestOpenPermissionsAreAFailure:
// a real broadened DACL, not a chmod call that Windows would ignore.
func TestOpenACLIsAFailure(t *testing.T) {
	env, _ := testEnv(t)
	widenACL(t, env.Paths.DataDir())
	c := find(Run(env), "relay home")
	if c.Status != Fail || !strings.Contains(c.Detail, "data") || c.Fix == "" {
		t.Fatalf("%+v", c)
	}
}
