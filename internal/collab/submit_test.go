package collab

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/thesahibnanda-max/relay/internal/state"
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

type fakeSubmitter struct {
	screen []string
	st     state.State
	enters int
}

func (f *fakeSubmitter) Screen() []string                 { return f.screen }
func (f *fakeSubmitter) Snapshot() state.Snapshot         { return state.Snapshot{State: f.st} }
func (f *fakeSubmitter) PressEnter(context.Context) error { f.enters++; return nil }

// A message left typed but not submitted gets Enter again - unless a dialog
// is open, where Enter would answer the dialog.
func TestConfirmSubmitNeverPressesEnterIntoADialog(t *testing.T) {
	defer func(d time.Duration) { submitWait = d }(submitWait)
	submitWait = 10 * time.Millisecond
	rule := strings.Repeat("─", 40)
	box := []string{rule, "> hello there", rule, "status"}
	for _, c := range []struct {
		st   state.State
		want int
	}{{state.Idle, submitRetries}, {state.Dialog, 0}} {
		s := New()
		f := &fakeSubmitter{screen: box, st: c.st}
		s.confirmSubmit(f, "hello there", time.Now())
		if f.enters != c.want {
			t.Fatalf("%v: pressed Enter %d times, want %d", c.st, f.enters, c.want)
		}
	}
}
