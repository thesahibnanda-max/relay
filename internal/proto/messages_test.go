package proto

import "testing"

func TestRoleAndToolValidation(t *testing.T) {
	for _, ok := range []string{"developer", "qa", "agent", "Code-Reviewer", "a.b_c-1"} {
		if !ValidRole(ok) {
			t.Errorf("role %q should be valid", ok)
		}
	}
	for _, bad := range []string{"", "-lead", "has space", "new\nline", "x]y", "a|b", "é", "sneaky\x1b[2J", "x" + string(make([]byte, 40))} {
		if ValidRole(bad) {
			t.Errorf("role %q must be rejected: it is printed into other agents' terminals", bad)
		}
	}
	if !ValidTool("claude") || !ValidTool("codex-2") || ValidTool("Claude") || ValidTool("a b") || ValidTool("") {
		t.Error("tool validation")
	}
}
