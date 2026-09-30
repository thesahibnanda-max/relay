package collab

import (
	"strings"
	"testing"
)

// Screens captured live from agy 1.2.12/1.2.13.
func TestInputHolds(t *testing.T) {
	rule := strings.Repeat("─", 60)
	screen := func(box ...string) []string {
		return append(append([]string{"  42", "", rule}, box...), rule, "? for shortcuts     Gemini 3.8 Flash · high")
	}
	msg := "[relay | from lead (orchestrator) | task | normal | msg 01K6AAAAAAAAAAAAAAAAAAAAAA]\nrun the tests"
	cases := []struct {
		name   string
		screen []string
		want   bool
	}{
		{"empty prompt: submitted", screen(">"), false},
		{"still in the box", screen("> [relay | from lead (orchestrator) | task | normal | msg 01K6", "  run the tests"), true},
		{"long paste placeholder", screen("> [Pasted text #1 +25 lines]"), true},
		{"someone else's text", screen("> fix the bug"), false},
		{"no input box (a dialog)", []string{"Allow calling this tool?", "> 1. Yes"}, false},
	}
	for _, c := range cases {
		if got := inputHolds(c.screen, msg); got != c.want {
			t.Errorf("%s: %v, want %v", c.name, got, c.want)
		}
	}
	// The submitted prompt is drawn above the box too; only the box counts.
	s := append([]string{"> [relay | from lead (orchestrator) | task | normal | msg 01K6"}, screen(">")...)
	if inputHolds(s, msg) {
		t.Error("the prompt shown in the history above the box was taken for unsent input")
	}
}
