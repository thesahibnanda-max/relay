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

// ---- M-mesh-4: cross-daemon message hand-off ------------------------------

func TestCreateMessageDefaultsOriginLocalAndRevOne(t *testing.T) {
	s := open(t)
	sess, a, b := twoAgents(t, s)
	m, err := s.CreateMessage(bg, mk(sess, a, b, "hi", P2))
	if err != nil || m.Origin != OriginLocal || m.Rev != 1 {
		t.Fatalf("m: %+v, err=%v", m, err)
	}
}

func TestCreateMessageMirrorOriginStartsAtRevZero(t *testing.T) {
	s := open(t)
	sess, a, b := twoAgents(t, s)
	msg := mk(sess, a, b, "hi", P2)
	msg.Origin = OriginMirror
	msg.ToPeer = "nodekey:remote"
	m, err := s.CreateMessage(bg, msg)
	if err != nil || m.Origin != OriginMirror || m.Rev != 0 {
		t.Fatalf("m: %+v, err=%v", m, err)
	}
}

func TestAdvanceIncrementsRev(t *testing.T) {
	s := open(t)
	sess, a, b := twoAgents(t, s)
	m, err := s.CreateMessage(bg, mk(sess, a, b, "hi", P2))
	if err != nil || m.Rev != 1 {
		t.Fatalf("create: %+v, err=%v", m, err)
	}
	got, changed, err := s.Advance(bg, m.ID, MsgDispatched, "")
	if err != nil || !changed || got.Rev != 2 {
		t.Fatalf("advance: %+v changed=%v err=%v", got, changed, err)
	}
	// A no-op transition (already in that state) must not bump rev again.
	got2, changed2, err := s.Advance(bg, m.ID, MsgDispatched, "")
	if err != nil || changed2 || got2.Rev != 2 {
		t.Fatalf("idempotent advance: %+v changed=%v err=%v", got2, changed2, err)
	}
}

func mirrorHandoff(sess Session, id, toAgent, toPeer string) Message {
	return Message{ID: id, SessionID: sess.ID, FromName: "alice", ToAgent: toAgent, ToName: "bob", ToPeer: toPeer,
		Kind: KindTask, Priority: P2, Body: "hi", Origin: OriginLocal, FromPeer: "nodekey:sender"}
}

func TestApplyHandoffCreatesOnFirstCallAndIsIdempotentOnResend(t *testing.T) {
	s := open(t)
	sess := newSession(t, s)
	h := mirrorHandoff(sess, "01HANDOFF00000000000000A", "01REMOTEAGENT000000000A", "")
	created, m, err := s.ApplyHandoff(bg, h)
	if err != nil || !created || m.Rev != 1 {
		t.Fatalf("first call: created=%v m=%+v err=%v", created, m, err)
	}
	// Advance it, then resend the exact same handoff: must return the
	// existing (already-advanced) row, not overwrite it back to fresh.
	if _, _, err := s.Advance(bg, m.ID, MsgDispatched, ""); err != nil {
		t.Fatal(err)
	}
	created2, m2, err := s.ApplyHandoff(bg, h)
	if err != nil || created2 || m2.State != MsgDispatched || m2.Rev != 2 {
		t.Fatalf("resend: created=%v m=%+v err=%v", created2, m2, err)
	}
}

func TestApplyReceiptAppliesANewerRevAndIgnoresStaleOrEqual(t *testing.T) {
	s := open(t)
	sess, a, b := twoAgents(t, s)
	msg := mk(sess, a, b, "hi", P2)
	msg.Origin, msg.ToPeer = OriginMirror, "nodekey:bob-owner"
	m, err := s.CreateMessage(bg, msg)
	if err != nil || m.Rev != 0 {
		t.Fatalf("create: %+v, err=%v", m, err)
	}
	applied, got, err := s.ApplyReceipt(bg, m.ID, MsgDispatched, "", 1)
	if err != nil || !applied || got.State != MsgDispatched || got.Rev != 1 {
		t.Fatalf("first receipt: applied=%v got=%+v err=%v", applied, got, err)
	}
	// A stale/duplicate receipt (rev <= current) is ignored, not applied.
	applied2, got2, err := s.ApplyReceipt(bg, m.ID, MsgDone, "", 1)
	if err != nil || applied2 || got2.State != MsgDispatched {
		t.Fatalf("stale receipt must be ignored: applied=%v got=%+v err=%v", applied2, got2, err)
	}
	// A genuinely newer receipt is allowed to jump several states at once -
	// a mirror row does not run the local state machine, it just mirrors.
	applied3, got3, err := s.ApplyReceipt(bg, m.ID, MsgDone, "answered", 5)
	if err != nil || !applied3 || got3.State != MsgDone || got3.Rev != 5 {
		t.Fatalf("newer receipt: applied=%v got=%+v err=%v", applied3, got3, err)
	}
}

func TestApplyReceiptNotFound(t *testing.T) {
	s := open(t)
	if _, _, err := s.ApplyReceipt(bg, "01ARZ3NDEKTSV4RRFFQ69G5FAV", MsgDone, "", 1); !errors.Is(err, ErrNotFound) {
		t.Fatalf("err = %v, want ErrNotFound", err)
	}
}

func TestPendingMirrorMessagesOnlyReturnsUnconfirmedUnexpiredOnes(t *testing.T) {
	s := open(t)
	sess, a, b := twoAgents(t, s)
	now := time.Now()

	unconfirmed := mk(sess, a, b, "unconfirmed", P2)
	unconfirmed.Origin, unconfirmed.ToPeer = OriginMirror, "nodekey:peer"
	unconfirmed.ExpiresAt = now.Add(time.Hour)
	unconfirmed, err := s.CreateMessage(bg, unconfirmed)
	if err != nil {
		t.Fatal(err)
	}

	confirmed := mk(sess, a, b, "confirmed", P2)
	confirmed.Origin, confirmed.ToPeer = OriginMirror, "nodekey:peer"
	confirmed.ExpiresAt = now.Add(time.Hour)
	confirmed, err = s.CreateMessage(bg, confirmed)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.ApplyReceipt(bg, confirmed.ID, MsgQueued, "", 1); err != nil {
		t.Fatal(err)
	}

	expired := mk(sess, a, b, "expired", P2)
	expired.Origin, expired.ToPeer = OriginMirror, "nodekey:peer"
	expired.ExpiresAt = now.Add(-time.Minute)
	if _, err := s.CreateMessage(bg, expired); err != nil {
		t.Fatal(err)
	}

	otherPeer := mk(sess, a, b, "other peer", P2)
	otherPeer.Origin, otherPeer.ToPeer = OriginMirror, "nodekey:someone-else"
	otherPeer.ExpiresAt = now.Add(time.Hour)
	if _, err := s.CreateMessage(bg, otherPeer); err != nil {
		t.Fatal(err)
	}

	pending, err := s.PendingMirrorMessages(bg, sess.ID, "nodekey:peer", now)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].ID != unconfirmed.ID {
		t.Fatalf("pending: %+v", pending)
	}
}

func TestPendingReceiptsReturnsLiveAndRecentlyTerminalOnes(t *testing.T) {
	s := open(t)
	sess := newSession(t, s)

	live := mirrorHandoff(sess, "01PR000000000000000000A", "01AGENT00000000000000A", "")
	live.FromPeer = "nodekey:peer"
	if _, _, err := s.ApplyHandoff(bg, live); err != nil {
		t.Fatal(err)
	}

	longDone := mirrorHandoff(sess, "01PR000000000000000000C", "01AGENT00000000000000A", "")
	longDone.FromPeer = "nodekey:peer"
	if _, _, err := s.ApplyHandoff(bg, longDone); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Advance(bg, longDone.ID, MsgDone, ""); err != nil {
		t.Fatal(err)
	}

	// The cutoff sits strictly between longDone's update and recentlyDone's -
	// real elapsed time, not a synthetic timestamp, so it exercises the same
	// clock PendingReceipts itself reads.
	time.Sleep(30 * time.Millisecond)
	cutoff := time.Now()
	time.Sleep(30 * time.Millisecond)

	recentlyDone := mirrorHandoff(sess, "01PR000000000000000000B", "01AGENT00000000000000A", "")
	recentlyDone.FromPeer = "nodekey:peer"
	if _, _, err := s.ApplyHandoff(bg, recentlyDone); err != nil {
		t.Fatal(err)
	}
	if _, _, err := s.Advance(bg, recentlyDone.ID, MsgDone, ""); err != nil {
		t.Fatal(err)
	}

	otherPeer := mirrorHandoff(sess, "01PR000000000000000000D", "01AGENT00000000000000A", "")
	otherPeer.FromPeer = "nodekey:someone-else"
	if _, _, err := s.ApplyHandoff(bg, otherPeer); err != nil {
		t.Fatal(err)
	}

	pending, err := s.PendingReceipts(bg, sess.ID, "nodekey:peer", cutoff)
	if err != nil {
		t.Fatal(err)
	}
	ids := map[string]bool{}
	for _, m := range pending {
		ids[m.ID] = true
	}
	if len(ids) != 2 || !ids[live.ID] || !ids[recentlyDone.ID] {
		t.Fatalf("pending: %+v", pending)
	}
	if ids[longDone.ID] {
		t.Fatal("a long-terminal message should not be resent forever")
	}
	if ids[otherPeer.ID] {
		t.Fatal("a different peer's message must not be included")
	}
}
