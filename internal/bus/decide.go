// Package bus is an agent's local scheduler. The daemon durably routes
// messages to an agent; this package decides, from the tool's live state,
// WHEN each one may be typed into the terminal (or must keep waiting). It only
// ever holds messages or types them at a safe moment: it never types into a
// dialog, over the user's own typing, or into an unknown state.
package bus

import (
	"github.com/thesahibnanda-max/relay/internal/state"
)

// Priorities (lower is more urgent), matching the wire values.
const (
	P0 = 0 // interrupt: preempt current work
	P1 = 1 // high: next safe point
	P2 = 2 // normal: when idle at the prompt
	P3 = 3 // low / FYI: when idle and nothing else is waiting
)

// Verdict is what to do with one message right now.
type Verdict int

const (
	Hold      Verdict = iota // not now; try again when something changes
	Inject                   // type it now
	Interrupt                // press Esc first (once); type it when the tool is idle
)

func (v Verdict) String() string { return [...]string{"hold", "inject", "interrupt"}[v] }

// Situation is everything the decision depends on.
type Situation struct {
	State state.Snapshot

	UserTyping bool // the user pressed a key within the last moment
	DraftDirty bool // the user has unsent text in the input box
	Cooling    bool // we typed something a moment ago and the tool has not reacted yet
	// Interrupted: Esc was already sent for this very message. It is never
	// sent twice (a second Esc can open history/rewind in the tools).
	Interrupted bool
	// OthersWaiting: messages of more urgent or equal priority are waiting too
	// (P3 only goes out when nothing else is queued).
	OthersWaiting bool
}

// Decide is the delivery decision table (plan: "Delivery decision table"),
// evaluated for a message of effective priority p.
//
//	state               P0                 P1          P2          P3
//	idle at prompt      inject             inject      inject      inject if queue empty
//	busy (turn running) Esc, then inject   after turn  after turn  after turn
//	dialog / starting / unknown / desynced / no bracketed paste     hold
//	user typing or unsent draft                                     hold (wait for a pause)
//
// P1's faster path (between tool calls, via hooks) arrives with M4; until then
// it behaves like P2 but is ordered ahead of it.
func Decide(s Situation, p int) (Verdict, string) {
	st := s.State
	switch {
	case st.Desynced:
		return Hold, "screen tracker out of sync"
	case st.State == state.Dialog:
		return Hold, "tool is waiting on a dialog"
	case st.State == state.Unknown:
		return Hold, "tool state unknown"
	case st.State == state.Starting:
		return Hold, "tool is still starting"
	case !st.BracketedPaste:
		return Hold, "tool has not enabled bracketed paste"
	case s.UserTyping:
		return Hold, "user is typing"
	case s.DraftDirty:
		return Hold, "user has an unsent draft"
	}
	switch st.State {
	case state.Idle:
		if s.Cooling {
			return Hold, "waiting for the tool to react to the last message"
		}
		if p >= P3 && s.OthersWaiting {
			return Hold, "low priority waits for an empty queue"
		}
		return Inject, "idle"
	case state.Busy:
		if p <= P0 && !s.Interrupted {
			return Interrupt, "interrupt requested"
		}
		return Hold, "tool is busy; delivering after the turn"
	}
	return Hold, "unhandled state"
}
