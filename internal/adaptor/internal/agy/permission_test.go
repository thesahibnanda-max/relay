package agy

import (
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func settingsIn(t *testing.T, content string) (home, path string) {
	t.Helper()
	home = t.TempDir()
	path = filepath.Join(home, ".gemini", "antigravity-cli", "settings.json")
	if content != "" {
		os.MkdirAll(filepath.Dir(path), 0o755)
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	return home, path
}

func TestAllowRelayToolsKeepsEverythingElse(t *testing.T) {
	cases := []struct{ in, want string }{
		{"", "{\n  \"permissions\": {\n    \"allow\": [\n      \"mcp(relay/*)\"\n    ]\n  }\n}\n"},
		{"  \n", "{\n  \"permissions\": {\n    \"allow\": [\n      \"mcp(relay/*)\"\n    ]\n  }\n}\n"},
		{`{"trustedWorkspaces":["/a"],"permissions":{"deny":["x"],"allow":["shell(ls)"]},"n":1.50}`,
			"{\n  \"trustedWorkspaces\": [\n    \"/a\"\n  ],\n  \"permissions\": {\n    \"deny\": [\n      \"x\"\n    ],\n    \"allow\": [\n      \"shell(ls)\",\n      \"mcp(relay/*)\"\n    ]\n  },\n  \"n\": 1.50\n}\n"},
		{`{"permissions":{"deny":[]}}`, "{\n  \"permissions\": {\n    \"deny\": [],\n    \"allow\": [\n      \"mcp(relay/*)\"\n    ]\n  }\n}\n"},
	}
	for _, c := range cases {
		home, path := settingsIn(t, c.in)
		if ok, err := RelayToolsAllowed(home); ok || err != nil {
			t.Fatalf("%q: allowed=%v err=%v before", c.in, ok, err)
		}
		if err := AllowRelayTools(home); err != nil {
			t.Fatalf("%q: %v", c.in, err)
		}
		got, _ := os.ReadFile(path)
		if string(got) != c.want {
			t.Fatalf("%q: wrote\n%s\nwant\n%s", c.in, got, c.want)
		}
		if ok, err := RelayToolsAllowed(home); !ok || err != nil {
			t.Fatalf("%q: allowed=%v err=%v after", c.in, ok, err)
		}
		if c.in != "" && runtime.GOOS != "windows" {
			if st, _ := os.Stat(path); st.Mode().Perm() != 0o600 {
				t.Fatalf("%q: mode %v, want the file's own 0600", c.in, st.Mode().Perm())
			}
		}
	}
}

func TestAllowRelayToolsRefusesWhatItCannotRead(t *testing.T) {
	for _, in := range []string{`{"permissions":`, `[1]`, `{"permissions":{"allow":"x"}}`, `{"permissions":[]}`} {
		home, path := settingsIn(t, in)
		if _, err := RelayToolsAllowed(home); err == nil {
			t.Fatalf("%q: no error", in)
		}
		if err := AllowRelayTools(home); err == nil {
			t.Fatalf("%q: rewrote a file it could not read", in)
		}
		if got, _ := os.ReadFile(path); string(got) != in {
			t.Fatalf("%q: file changed to %q", in, got)
		}
	}
}

// A settings file kept elsewhere (dotfiles) behind a symlink is edited in
// place: the link stays a link.
func TestAllowRelayToolsWritesThroughASymlink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlinks need privileges on Windows")
	}
	home, path := settingsIn(t, "")
	real := filepath.Join(t.TempDir(), "settings.json")
	os.WriteFile(real, []byte(`{"permissions":{"allow":[]}}`), 0o644)
	os.MkdirAll(filepath.Dir(path), 0o755)
	if err := os.Symlink(real, path); err != nil {
		t.Fatal(err)
	}
	if err := AllowRelayTools(home); err != nil {
		t.Fatal(err)
	}
	if st, err := os.Lstat(path); err != nil || st.Mode()&os.ModeSymlink == 0 {
		t.Fatal("the symlink was replaced")
	}
	if got, _ := os.ReadFile(real); !strings.Contains(string(got), relayToolsRule) {
		t.Fatalf("target not updated: %s", got)
	}
}
