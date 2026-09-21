package hooks

import (
	"encoding/json"
	"os/exec"
	"strings"
	"testing"
)

func TestQuoteSurvivesTheShell(t *testing.T) {
	for _, s := range []string{"/plain/path", "/with space/relay", `/it's/here`, `/a"b$c`, "/tmp/x;rm -rf"} {
		out, err := exec.Command("sh", "-c", "printf %s "+Quote(s)).Output()
		if err != nil || string(out) != s {
			t.Errorf("%q -> %q %v", s, out, err)
		}
	}
}

func TestSettingsShape(t *testing.T) {
	var s struct {
		Hooks map[string][]struct {
			Matcher string `json:"matcher"`
			Hooks   []struct {
				Type    string `json:"type"`
				Command string `json:"command"`
				Timeout int    `json:"timeout"`
			} `json:"hooks"`
		} `json:"hooks"`
	}
	if err := json.Unmarshal(SettingsJSON("/opt/my relay/relay", "/run/x y"), &s); err != nil {
		t.Fatal(err)
	}
	if len(s.Hooks) != len(Events) {
		t.Fatalf("%d events", len(s.Hooks))
	}
	for _, ev := range Events {
		e := s.Hooks[ev]
		if len(e) != 1 || len(e[0].Hooks) != 1 || e[0].Hooks[0].Type != "command" || e[0].Hooks[0].Timeout <= 0 {
			t.Fatalf("%s: %+v", ev, e)
		}
		if want := `'/opt/my relay/relay' hook ` + ev + ` --dir '/run/x y'`; e[0].Hooks[0].Command != want {
			t.Errorf("command %q", e[0].Hooks[0].Command)
		}
		if (ev == "PostToolUse") != (e[0].Matcher == "*") {
			t.Errorf("%s matcher %q", ev, e[0].Matcher)
		}
	}
}

func TestOutputs(t *testing.T) {
	var m map[string]any
	json.Unmarshal([]byte(AdditionalContext("PostToolUse", "hi \"there\"\n")), &m)
	h := m["hookSpecificOutput"].(map[string]any)
	if h["hookEventName"] != "PostToolUse" || h["additionalContext"] != "hi \"there\"\n" {
		t.Fatal(m)
	}
	json.Unmarshal([]byte(Block("go on")), &m)
	if m["decision"] != "block" || m["reason"] != "go on" || strings.Contains(Block("x"), "\n") {
		t.Fatal(m)
	}
}
