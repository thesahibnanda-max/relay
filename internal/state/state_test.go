package state

import (
	"testing"
	"time"
)

type clock struct{ t time.Time }

func (c *clock) now() time.Time          { return c.t }
func (c *clock) advance(d time.Duration) { c.t = c.t.Add(d) }

func newTest() (*Machine, *clock) {
	c := &clock{t: time.Unix(1000, 0)}
	m := NewMachine()
	m.now = c.now
	m.sigs["init"] = Signal{Source: "init", State: Starting, Conf: Low, At: c.t, seq: 1}
	m.seq = 1
	return m, c
}

func TestStartsInStarting(t *testing.T) {
	m, _ := newTest()
	if s := m.Snapshot(); s.State != Starting {
		t.Fatalf("state = %v", s.State)
	}
}

func TestAuthoritativeBeatsHeuristic(t *testing.T) {
	m, c := newTest()
	m.Apply(Signal{Source: "hook", State: Idle, Conf: High})
	m.Output() // heavy output would normally look Busy
	c.advance(10 * time.Millisecond)
	if s := m.Snapshot(); s.State != Idle || s.Conf != High || s.Source != "hook" {
		t.Fatalf("snapshot = %+v", s)
	}
}

func TestActivityHeuristicWhenNothingAuthoritative(t *testing.T) {
	m, c := newTest()
	m.Output()
	c.advance(50 * time.Millisecond)
	if s := m.Snapshot(); s.State != Busy || s.Source != "activity" {
		t.Fatalf("recent output: %+v", s)
	}
	c.advance(2 * time.Second)
	if s := m.Snapshot(); s.State != Idle {
		t.Fatalf("after quiet: %+v", s)
	}
}

func TestExpiredSignalFallsBack(t *testing.T) {
	m, c := newTest()
	m.Apply(Signal{Source: "hook", State: Busy, Conf: High, TTL: time.Second})
	if m.Snapshot().State != Busy {
		t.Fatal("fresh hook should win")
	}
	c.advance(2 * time.Second)
	if s := m.Snapshot(); s.Source == "hook" {
		t.Fatalf("expired signal still used: %+v", s)
	}
}

func TestDialogIsNotOverriddenByActivity(t *testing.T) {
	m, c := newTest()
	m.Apply(Signal{Source: "screen", State: Dialog, Conf: Low})
	m.Output()
	c.advance(10 * time.Millisecond)
	if s := m.Snapshot(); s.State != Dialog {
		t.Fatalf("dialog lost to activity: %+v", s)
	}
}

func TestDesyncedHeuristicsGoUnknown(t *testing.T) {
	m, c := newTest()
	m.Output()
	c.advance(10 * time.Millisecond)
	m.SetFlags(false, true, false, true)
	if s := m.Snapshot(); s.State != Unknown || !s.Desynced {
		t.Fatalf("desynced heuristic should be unknown: %+v", s)
	}
	// ...but an authoritative signal is still trusted.
	m.Apply(Signal{Source: "hook", State: Idle, Conf: High})
	if s := m.Snapshot(); s.State != Idle {
		t.Fatalf("authoritative ignored while desynced: %+v", s)
	}
}

func TestFlagsPassThrough(t *testing.T) {
	m, _ := newTest()
	m.SetFlags(true, true, true, false)
	s := m.Snapshot()
	if !s.PlanMode || !s.BracketedPaste || !s.AltScreen {
		t.Fatalf("flags: %+v", s)
	}
}

func TestFromScreen(t *testing.T) {
	rules := []ScreenRule{
		{State: Dialog, Contains: "Allow? (y/n)"},
		{State: Idle, Contains: "> ", PlanMode: true},
	}
	sig, plan, ok := FromScreen([]string{"working...", "Allow? (y/n)"}, rules)
	if !ok || sig.State != Dialog || plan {
		t.Fatalf("got %+v plan=%v ok=%v", sig, plan, ok)
	}
	if _, _, ok := FromScreen([]string{"nothing"}, rules); ok {
		t.Fatal("unexpected match")
	}
}

func TestFromScreenIgnoresStaleTextHigherUp(t *testing.T) {
	rules := []ScreenRule{{State: Dialog, Contains: "Allow? (y/n)"}}
	answered := []string{"Allow? (y/n) y", "thinking...", "reply: ok", "> ", "", ""}
	if _, _, ok := FromScreen(answered, rules); ok {
		t.Fatal("answered dialog still matched")
	}
	live := []string{"old output", "Allow? (y/n)", "", ""}
	if _, _, ok := FromScreen(live, rules); !ok {
		t.Fatal("live dialog not matched")
	}
	wide := []ScreenRule{{State: Dialog, Contains: "Approve?", Tail: 6}}
	if _, _, ok := FromScreen([]string{"Approve?", "1. yes", "2. no", "3. edit", "hint"}, wide); !ok {
		t.Fatal("per-rule Tail not honoured")
	}
}

func TestDialogOutranksAuthoritativeBusy(t *testing.T) {
	m := NewMachine()
	m.Apply(Signal{Source: "rollout", State: Busy, Conf: High})
	m.Apply(Signal{Source: "screen", State: Dialog, Conf: Low, TTL: 2 * time.Second})
	if s := m.Snapshot(); s.State != Dialog {
		t.Fatalf("a permission dialog must win over 'busy' from a hook or transcript, got %+v", s)
	}
	m.Clear("screen")
	if s := m.Snapshot(); s.State != Busy || s.Conf != High {
		t.Fatalf("back to the authoritative view once the dialog is gone: %+v", s)
	}
	// an authoritative dialog stays until its source says otherwise
	m.Apply(Signal{Source: "hook", State: Dialog, Conf: High})
	m.Apply(Signal{Source: "rollout", State: Idle, Conf: High})
	if s := m.Snapshot(); s.State != Dialog {
		t.Fatalf("%+v", s)
	}
	m.Apply(Signal{Source: "hook", State: Busy, Conf: High})
	if s := m.Snapshot(); s.State == Dialog {
		t.Fatalf("the hook moved on: %+v", s)
	}
}

func TestAuthoritativeBusyGoesStaleWhenTheTerminalIsQuiet(t *testing.T) {
	m := NewMachine()
	now := time.Now()
	m.now = func() time.Time { return now }
	m.Apply(Signal{Source: "hook", State: Busy, Conf: High, TTL: 5 * time.Minute, QuietStale: 4 * time.Second})
	m.Output()
	now = now.Add(2 * time.Second)
	if s := m.Snapshot(); s.State != Busy || s.Conf != High {
		t.Fatalf("recent output keeps it busy: %+v", s)
	}
	now = now.Add(3 * time.Second) // 5 s of silence: the turn was cut short (Esc fires no Stop hook)
	if s := m.Snapshot(); s.State == Busy && s.Conf == High {
		t.Fatalf("stale busy must not block delivery for minutes: %+v", s)
	}
	if s := m.Snapshot(); s.State != Idle {
		t.Fatalf("falls back to the activity heuristic: %+v", s)
	}
	// Idle signals have no such expiry
	m.Apply(Signal{Source: "hook", State: Idle, Conf: High, QuietStale: 4 * time.Second})
	if s := m.Snapshot(); s.State != Idle || s.Conf != High {
		t.Fatalf("%+v", s)
	}
}
