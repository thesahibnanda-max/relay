// Package state models what a wrapped tool is doing right now, merging
// authoritative signals (hooks, transcripts) with heuristics (output activity,
// screen contents). Heuristics can never override a fresh authoritative signal.
package state

import (
	"strings"
	"sync"
	"time"
)

type State uint8

const (
	Unknown  State = iota // we cannot tell; callers must be conservative
	Starting              // tool launched, prompt not seen yet
	Idle                  // waiting at the prompt for the user
	Busy                  // a turn is running
	Dialog                // blocked on a permission / plan-approval / question dialog
)

func (s State) String() string {
	return [...]string{"unknown", "starting", "idle", "busy", "dialog"}[s]
}

type Confidence uint8

const (
	Low  Confidence = iota // heuristic
	High                   // authoritative (hook, transcript event)
)

// Signal is one source's opinion. It expires after TTL (0 = never).
type Signal struct {
	Source string
	State  State
	Conf   Confidence
	TTL    time.Duration
	At     time.Time
	// QuietStale, for Busy signals: if the terminal has produced no output for
	// this long the signal is disregarded. Hooks and transcripts announce when
	// a turn starts but not always when it is cut short (Esc fires no Stop
	// hook), while a working TUI keeps redrawing its spinner.
	QuietStale time.Duration

	seq uint64 // arrival order; the clock can tie, this cannot
}

// Snapshot is the merged view.
type Snapshot struct {
	State  State
	Conf   Confidence
	Source string
	Since  time.Time

	PlanMode       bool
	BracketedPaste bool
	AltScreen      bool
	Desynced       bool // screen tracker fell behind; heuristics untrustworthy
}

// Quiet is how long without output before the activity heuristic says Idle.
const Quiet = 400 * time.Millisecond

type Machine struct {
	mu         sync.Mutex
	now        func() time.Time
	sigs       map[string]Signal
	seq        uint64
	lastOutput time.Time
	flags      Snapshot // only the flag fields are used
	last       Snapshot
}

func NewMachine() *Machine {
	m := &Machine{now: time.Now, sigs: map[string]Signal{}}
	t := m.now()
	m.seq++
	m.sigs["init"] = Signal{Source: "init", State: Starting, Conf: Low, At: t, seq: m.seq}
	m.last = Snapshot{State: Starting, Conf: Low, Source: "init", Since: t}
	return m
}

// Apply records a source's latest opinion, replacing its previous one.
func (m *Machine) Apply(s Signal) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s.At.IsZero() {
		s.At = m.now()
	}
	m.seq++
	s.seq = m.seq
	m.sigs[s.Source] = s
}

// Clear forgets a source's opinion (e.g. the dialog is no longer on screen).
func (m *Machine) Clear(source string) {
	m.mu.Lock()
	delete(m.sigs, source)
	m.mu.Unlock()
}

// Output notes tool output (activity heuristic).
func (m *Machine) Output() {
	m.mu.Lock()
	m.lastOutput = m.now()
	m.mu.Unlock()
}

// SetFlags updates the non-state facts.
func (m *Machine) SetFlags(planMode, bracketedPaste, altScreen, desynced bool) {
	m.mu.Lock()
	m.flags = Snapshot{PlanMode: planMode, BracketedPaste: bracketedPaste, AltScreen: altScreen, Desynced: desynced}
	m.mu.Unlock()
}

// Snapshot merges everything into one answer.
func (m *Machine) Snapshot() Snapshot {
	m.mu.Lock()
	defer m.mu.Unlock()
	now := m.now()

	var best *Signal
	for k, s := range m.sigs {
		if s.TTL > 0 && now.Sub(s.At) > s.TTL {
			delete(m.sigs, k)
			continue
		}
		if s.State == Busy && s.QuietStale > 0 && !m.lastOutput.IsZero() && now.Sub(m.lastOutput) > s.QuietStale {
			continue
		}
		s := s
		// Highest confidence wins; among equals the newest wins.
		if best == nil || s.Conf > best.Conf || (s.Conf == best.Conf && s.seq > best.seq) {
			best = &s
		}
	}

	// A dialog outranks everything, whatever its confidence: a hook or
	// transcript saying "busy" cannot see a permission prompt on the screen,
	// and typing (or pressing Esc) into one answers it.
	for _, sg := range m.sigs {
		if sg.State == Dialog && (best == nil || best.State != Dialog || sg.seq > best.seq) {
			sg := sg
			best = &sg
		}
	}

	out := Snapshot{State: Unknown, Source: "none"}
	if best != nil {
		out.State, out.Conf, out.Source, out.Since = best.State, best.Conf, best.Source, best.At
	}

	// Activity heuristic: only informs us while nothing authoritative does and
	// once the tool has left Starting (an idle TUI still repaints its cursor).
	if (best == nil || best.Conf == Low) && !m.lastOutput.IsZero() && out.State != Dialog {
		if now.Sub(m.lastOutput) < Quiet {
			out.State, out.Conf, out.Source = Busy, Low, "activity"
		} else if out.State == Busy || out.State == Starting && now.Sub(m.lastOutput) >= Quiet*3 {
			out.State, out.Conf, out.Source = Idle, Low, "activity"
		}
	}

	out.PlanMode, out.BracketedPaste, out.AltScreen, out.Desynced =
		m.flags.PlanMode, m.flags.BracketedPaste, m.flags.AltScreen, m.flags.Desynced

	// A stale screen makes every heuristic worthless: be conservative.
	if out.Desynced && out.Conf == Low {
		out.State, out.Source = Unknown, "desynced"
	}
	m.last = out
	return out
}

// DefaultTail is how many non-empty bottom rows a ScreenRule inspects unless
// it says otherwise. Matching the whole screen would keep reporting a dialog
// long after it was answered, because its text lingers higher up.
const DefaultTail = 3

// ScreenRule maps on-screen text to a state (supplied per tool by adaptors).
type ScreenRule struct {
	State    State
	Contains string
	Tail     int  // look only at this many bottom non-empty rows (0 = DefaultTail)
	PlanMode bool // additionally marks plan mode when matched
}

// FromScreen evaluates rules against the bottom of the screen; first match wins.
func FromScreen(lines []string, rules []ScreenRule) (sig Signal, planMode, ok bool) {
	var rows []string
	for _, l := range lines {
		if strings.TrimSpace(l) != "" {
			rows = append(rows, l)
		}
	}
	for _, r := range rules {
		n := r.Tail
		if n <= 0 {
			n = DefaultTail
		}
		if n > len(rows) {
			n = len(rows)
		}
		if strings.Contains(strings.Join(rows[len(rows)-n:], "\n"), r.Contains) {
			return Signal{Source: "screen", State: r.State, Conf: Low, TTL: 2 * time.Second}, r.PlanMode, true
		}
	}
	return Signal{}, false, false
}
