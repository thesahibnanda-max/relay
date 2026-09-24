package repository

import (
	"reflect"
	"sort"
	"testing"

	"github.com/thesahibnanda-max/relay/server/package/database/mongodb"
)

func TestAllowedPredecessors(t *testing.T) {
	cases := []struct {
		state string
		want  []string // nil means "not a recognized state"
	}{
		{mongodb.MessageStateHeld, []string{mongodb.MessageStateHeld}},
		{mongodb.MessageStateQueued, []string{mongodb.MessageStateHeld, mongodb.MessageStateQueued}},
		{mongodb.MessageStateDispatched, []string{mongodb.MessageStateQueued, mongodb.MessageStateDispatched}},
		{mongodb.MessageStateInjected, []string{mongodb.MessageStateQueued, mongodb.MessageStateDispatched, mongodb.MessageStateInjected}},
		{mongodb.MessageStateAcknowledged, []string{mongodb.MessageStateQueued, mongodb.MessageStateDispatched, mongodb.MessageStateInjected, mongodb.MessageStateAcknowledged}},
		{mongodb.MessageStateDone, []string{mongodb.MessageStateQueued, mongodb.MessageStateDispatched, mongodb.MessageStateInjected, mongodb.MessageStateAcknowledged, mongodb.MessageStateDone}},
		{mongodb.MessageStateRejected, []string{mongodb.MessageStateHeld, mongodb.MessageStateQueued, mongodb.MessageStateDispatched, mongodb.MessageStateRejected}},
		{mongodb.MessageStateExpired, []string{mongodb.MessageStateHeld, mongodb.MessageStateQueued, mongodb.MessageStateDispatched, mongodb.MessageStateExpired}},
		{mongodb.MessageStateUndeliverable, []string{mongodb.MessageStateHeld, mongodb.MessageStateQueued, mongodb.MessageStateDispatched, mongodb.MessageStateUndeliverable}},
		{"bogus", nil},
	}
	for _, c := range cases {
		got, ok := allowedPredecessors[c.state]
		if c.want == nil {
			if ok {
				t.Errorf("allowedPredecessors[%q] = %v, want absent", c.state, got)
			}
			continue
		}
		if !ok {
			t.Errorf("allowedPredecessors[%q] missing, want %v", c.state, c.want)
			continue
		}
		gotSorted, wantSorted := append([]string(nil), got...), append([]string(nil), c.want...)
		sort.Strings(gotSorted)
		sort.Strings(wantSorted)
		if !reflect.DeepEqual(gotSorted, wantSorted) {
			t.Errorf("allowedPredecessors[%q] = %v, want %v", c.state, gotSorted, wantSorted)
		}
	}
}

// TestTerminalStatesHaveNoOutgoingTransitions guards the state machine's own
// invariant directly: nextStates must never list an entry for a terminal
// state (done/rejected/expired/undeliverable) - if it did, SetState would
// let a "finished" message move again.
func TestTerminalStatesHaveNoOutgoingTransitions(t *testing.T) {
	for _, s := range terminalStates {
		if _, ok := nextStates[s]; ok {
			t.Errorf("terminal state %q has outgoing transitions in nextStates: %v", s, nextStates[s])
		}
	}
}
