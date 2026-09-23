package store

import (
	"errors"
	"testing"
	"time"
)

func twoAgents(t *testing.T, s *Store) (Session, Agent, Agent) {
	t.Helper()
	sess := newSession(t, s)
	a, _, err := s.RegisterAgent(bg, RegisterParams{SessionID: sess.ID, Tool: "claude", Role: "dev", Name: "alice"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	b, _, err := s.RegisterAgent(bg, RegisterParams{SessionID: sess.ID, Tool: "codex", Role: "qa", Name: "bob"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	return sess, a, b
}

func mk(sess Session, from, to Agent, body string, prio int) Message {
	return Message{SessionID: sess.ID, FromAgent: from.ID, FromName: from.Name, ToAgent: to.ID, ToName: to.Name, Kind: KindTask, Priority: prio, Body: body}
}

func TestMessageTransitionsOnlyMoveForward(t *testing.T) {
	s := open(t)
	sess, a, b := twoAgents(t, s)
	m, err := s.CreateMessage(bg, mk(sess, a, b, "hi", P2))
	if err != nil || m.State != MsgQueued || m.Thread != m.ID {
		t.Fatalf("create: %+v %v", m, err)
	}
	steps := []struct {
		to      string
		changed bool
		err     error
	}{
		{MsgDispatched, true, nil},
		{MsgDispatched, false, nil}, // idempotent
		{MsgInjected, true, nil},
		{MsgDispatched, false, ErrBadTransition}, // no going back
		{MsgAcknowledged, true, nil},
		{MsgDone, true, nil},
		{MsgQueued, false, ErrBadTransition}, // terminal
	}
	for _, st := range steps {
		got, changed, err := s.Advance(bg, m.ID, st.to, "")
		if changed != st.changed || !errors.Is(err, st.err) {
			t.Fatalf("-> %s: changed=%v err=%v (state %s)", st.to, changed, err, got.State)
		}
	}
	if got, _ := s.GetMessage(bg, m.ID); got.State != MsgDone || got.Attempts != 1 {
		t.Fatalf("final %+v", got)
	}
	if _, _, err := s.Advance(bg, "01ARZ3NDEKTSV4RRFFQ69G5FAV", MsgDone, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown id: %v", err)
	}
	// held may only be released, refused or die
	h := mk(sess, a, b, "held", P2)
	h.State = MsgHeld
	h, _ = s.CreateMessage(bg, h)
	if _, _, err := s.Advance(bg, h.ID, MsgInjected, ""); !errors.Is(err, ErrBadTransition) {
		t.Fatalf("a held message must not be injected: %v", err)
	}
}

func TestDeliverableOrdersByPriorityThenAge(t *testing.T) {
	s := open(t)
	sess, a, b := twoAgents(t, s)
	low, _ := s.CreateMessage(bg, mk(sess, a, b, "low", P3))
	n1, _ := s.CreateMessage(bg, mk(sess, a, b, "n1", P2))
	urgent, _ := s.CreateMessage(bg, mk(sess, a, b, "urgent", P0))
	n2, _ := s.CreateMessage(bg, mk(sess, a, b, "n2", P2))
	held := mk(sess, a, b, "held", P0)
	held.State = MsgHeld
	s.CreateMessage(bg, held)
	s.Advance(bg, n1.ID, MsgDispatched, "") // dispatched but unconfirmed still counts
	s.Advance(bg, n2.ID, MsgInjected, "")   // typed: no longer deliverable

	got, err := s.Deliverable(bg, b.ID)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{urgent.ID, n1.ID, low.ID}
	if len(got) != len(want) {
		t.Fatalf("got %d messages: %+v", len(got), got)
	}
	for i := range want {
		if got[i].ID != want[i] {
			t.Errorf("position %d: got %s want %s", i, got[i].Body, want[i])
		}
	}
	if none, _ := s.Deliverable(bg, a.ID); len(none) != 0 {
		t.Fatal("alice has no mail")
	}
}

func TestExpireFailPendingAndDuplicates(t *testing.T) {
	s := open(t)
	sess, a, b := twoAgents(t, s)
	old := mk(sess, a, b, "old", P2)
	old.ExpiresAt = time.Now().Add(-time.Second)
	old, _ = s.CreateMessage(bg, old)
	fresh, _ := s.CreateMessage(bg, mk(sess, a, b, "fresh", P2))
	typed, _ := s.CreateMessage(bg, mk(sess, a, b, "typed", P2))
	s.Advance(bg, typed.ID, MsgInjected, "")

	exp, err := s.ExpireDue(bg, time.Now())
	if err != nil || len(exp) != 1 || exp[0].ID != old.ID || exp[0].State != MsgExpired {
		t.Fatalf("expire: %+v %v", exp, err)
	}
	if dup, ok, _ := s.FindDuplicate(bg, a.ID, b.ID, KindTask, "fresh", time.Now().Add(-time.Minute)); !ok || dup.ID != fresh.ID {
		t.Fatalf("duplicate not found: %+v %v", dup, ok)
	}
	if _, ok, _ := s.FindDuplicate(bg, a.ID, b.ID, KindTask, "old", time.Now().Add(-time.Minute)); ok {
		t.Fatal("expired messages are not duplicates")
	}
	failed, err := s.FailPending(bg, b.ID, MsgUndeliverable, "gone")
	if err != nil || len(failed) != 1 || failed[0].ID != fresh.ID {
		t.Fatalf("fail pending: %+v %v", failed, err)
	}
	if got, _ := s.GetMessage(bg, typed.ID); got.State != MsgInjected {
		t.Fatalf("already typed messages are untouched, got %s", got.State)
	}
	// filters
	if l, _ := s.ListMessages(bg, MessageFilter{Session: sess.ID, States: []string{MsgUndeliverable, MsgExpired}}); len(l) != 2 {
		t.Fatalf("filtered list: %d", len(l))
	}
	if l, _ := s.ListMessages(bg, MessageFilter{Agent: a.ID}); len(l) != 3 {
		t.Fatalf("by agent: %d", len(l))
	}
}
