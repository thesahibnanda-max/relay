package roles

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/thesahibnanda-max/relay/internal/proto"
)

func TestBuiltinRolesAllParse(t *testing.T) {
	names := Builtin()
	for _, want := range []string{"orchestrator", "planner", "developer", "qa", "reviewer"} {
		found := false
		for _, n := range names {
			found = found || n == want
		}
		if !found {
			t.Errorf("missing builtin role %q (have %v)", want, names)
		}
	}
	for _, n := range names {
		r, err := Resolve(n)
		if err != nil {
			t.Fatalf("%s: %v", n, err)
		}
		if r.Body == "" || r.Description == "" || strings.HasPrefix(r.Body, "---") {
			t.Errorf("%s: frontmatter not split cleanly: %+v", n, r)
		}
	}
}

func TestPolicyFromFrontmatter(t *testing.T) {
	o, _ := Resolve("Orchestrator") // case-insensitive
	if !o.CanInterrupt || !o.CanBroadcast || o.DefaultPreempt != "on-p0" {
		t.Errorf("orchestrator policy wrong: %+v", o)
	}
	q, _ := Resolve("qa")
	if q.CanInterrupt || q.CanBroadcast || q.DefaultPreempt != "never" {
		t.Errorf("qa policy wrong: %+v", q)
	}
}

func TestEmptySpecIsNeutralRole(t *testing.T) {
	r, err := Resolve("")
	if err != nil || r.Name != "agent" || r.CanInterrupt {
		t.Errorf("%+v %v", r, err)
	}
}

func TestUnknownRoleListsChoices(t *testing.T) {
	_, err := Resolve("wizard")
	if err == nil || !strings.Contains(err.Error(), "orchestrator") {
		t.Errorf("err = %v", err)
	}
}

func TestRoleFromFile(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "security-auditor.md")
	os.WriteFile(p, []byte("---\ndescription: audits\ncan_interrupt: true\ndefault_preempt: on-p1\n---\nAudit everything.\r\n"), 0o600)
	r, err := Resolve(p)
	if err != nil {
		t.Fatal(err)
	}
	if r.Name != "security-auditor" || r.Source != p || !r.CanInterrupt || r.DefaultPreempt != "on-p1" || r.Body != "Audit everything." {
		t.Errorf("%+v", r)
	}
}

func TestRoleFileWithoutFrontmatter(t *testing.T) {
	p := filepath.Join(t.TempDir(), "plain.md")
	os.WriteFile(p, []byte("Just do good work."), 0o600)
	r, err := Resolve(p)
	if err != nil || r.Body != "Just do good work." || r.CanInterrupt {
		t.Errorf("%+v %v", r, err)
	}
}

func TestRoleFileErrors(t *testing.T) {
	dir := t.TempDir()
	if _, err := Resolve(filepath.Join(dir, "missing.md")); err == nil {
		t.Error("missing file accepted")
	}
	big := filepath.Join(dir, "big.md")
	os.WriteFile(big, []byte(strings.Repeat("x", MaxFileSize+10)), 0o600)
	if _, err := Resolve(big); err == nil {
		t.Error("oversize file accepted")
	}
	bin := filepath.Join(dir, "bin.md")
	os.WriteFile(bin, []byte{0xff, 0xfe, 0x00}, 0o600)
	if _, err := Resolve(bin); err == nil {
		t.Error("non-UTF-8 file accepted")
	}
	bad := filepath.Join(dir, "bad.md")
	os.WriteFile(bad, []byte("---\ndefault_preempt: sometimes\n---\nx"), 0o600)
	if _, err := Resolve(bad); err == nil {
		t.Error("bad default_preempt accepted")
	}
}

func TestLooksLikePath(t *testing.T) {
	for s, want := range map[string]bool{"qa": false, "developer": false, "./x": true, "a/b": true, "x.md": true, "~/r.md": true, "C:\\r": true} {
		if LooksLikePath(s) != want {
			t.Errorf("LooksLikePath(%q) != %v", s, want)
		}
	}
}

func TestFileRoleNamesAreSanitised(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "my fancy role]\x1b[31m.md")
	if err := os.WriteFile(path, []byte("body"), 0o600); err != nil {
		t.Skip("filesystem does not allow that name:", err)
	}
	r, err := Resolve(path)
	if err != nil {
		t.Fatal(err)
	}
	if !proto.ValidRole(r.Name) {
		t.Fatalf("role name %q would be shown to other agents and must be plain", r.Name)
	}
	for in, want := range map[string]string{"plain": "plain", "a b": "a-b", "///": "custom", "": "custom", "-x-": "x"} {
		if got := SanitizeName(in); got != want {
			t.Errorf("SanitizeName(%q) = %q, want %q", in, got, want)
		}
	}
}
