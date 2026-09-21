package store

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thesahibnanda-max/relay/internal/naming"
)

func open(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "relay.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { s.Close() })
	return s
}

var bg = context.Background()

func newSession(t *testing.T, s *Store) Session {
	t.Helper()
	sess, err := s.CreateSession(bg, "shared", "demo")
	if err != nil {
		t.Fatal(err)
	}
	return sess
}

func TestFilePermissionsAndReopen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "relay.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	sess := newSession(t, s)
	s.Close()
	for _, p := range []string{path, path + "-wal", path + "-shm"} {
		if st, err := os.Stat(p); err == nil && st.Mode().Perm()&0o077 != 0 {
			t.Errorf("%s is accessible to others: %v", p, st.Mode().Perm())
		}
	}
	s2, err := Open(path) // migrations must be idempotent and data must persist
	if err != nil {
		t.Fatal(err)
	}
	defer s2.Close()
	if got, err := s2.GetSession(bg, sess.ID); err != nil || got.ID != sess.ID {
		t.Fatalf("session lost on reopen: %v %v", got, err)
	}
}

func TestSessionLifecycle(t *testing.T) {
	s := open(t)
	sess := newSession(t, s)
	solo, _ := s.CreateSession(bg, "solo", "")
	if _, err := s.CreateSession(bg, "weird", ""); err == nil {
		t.Error("bad kind accepted")
	}
	if _, err := s.GetSession(bg, "01ARZ3NDEKTSV4RRFFQ69G5FAV"); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v", err)
	}
	// lookups are case-insensitive
	if got, err := s.GetSession(bg, strings.ToLower(sess.ID)); err != nil || got.ID != sess.ID {
		t.Errorf("case-insensitive lookup: %v %v", got, err)
	}
	list, _ := s.ListSessions(bg, false)
	if len(list) != 1 || list[0].ID != sess.ID {
		t.Errorf("default list should hide solo: %+v", list)
	}
	if all, _ := s.ListSessions(bg, true); len(all) != 2 {
		t.Errorf("all = %+v", all)
	}
	if err := s.EndSession(bg, sess.ID); err != nil {
		t.Fatal(err)
	}
	if err := s.EndSession(bg, sess.ID); err != nil {
		t.Errorf("EndSession must be idempotent: %v", err)
	}
	if err := s.EndSession(bg, "01ARZ3NDEKTSV4RRFFQ69G5FAV"); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v", err)
	}
	if list, _ := s.ListSessions(bg, false); len(list) != 0 {
		t.Errorf("ended session still listed: %+v", list)
	}
	_ = solo
}

func TestRegisterGeneratesUniqueNamesUnderConcurrency(t *testing.T) {
	s := open(t)
	sess := newSession(t, s)
	const n = 60
	names := make(chan string, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			a, tok, err := s.RegisterAgent(bg, RegisterParams{SessionID: sess.ID, Tool: "claude", Role: "developer"}, nil)
			if err != nil || tok == "" {
				t.Errorf("register: %v", err)
				return
			}
			names <- a.Name
		}()
	}
	wg.Wait()
	close(names)
	seen := map[string]bool{}
	for nm := range names {
		if seen[nm] {
			t.Fatalf("duplicate name %q", nm)
		}
		seen[nm] = true
	}
	if len(seen) != n {
		t.Fatalf("registered %d of %d", len(seen), n)
	}
}

// Force every random attempt to collide: the fallback suffix must still find a name.
type constGen struct{}

func (constGen) ID() string       { return "const" }
func (constGen) Generate() string { return "same-name" }

func TestRegisterSurvivesGeneratorCollisions(t *testing.T) {
	s := open(t)
	sess := newSession(t, s)
	namer := naming.With(constGen{})
	seen := map[string]bool{}
	for i := 0; i < 25; i++ {
		a, _, err := s.RegisterAgent(bg, RegisterParams{SessionID: sess.ID, Tool: "codex", Role: "qa"}, namer)
		if err != nil {
			t.Fatalf("agent %d: %v", i, err)
		}
		if seen[a.Name] {
			t.Fatalf("duplicate %q", a.Name)
		}
		seen[a.Name] = true
	}
	if !seen["same-name"] {
		t.Error("first agent should get the generated name as is")
	}
}

func TestExplicitNames(t *testing.T) {
	s := open(t)
	sess := newSession(t, s)
	other := newSession(t, s)
	a, _, err := s.RegisterAgent(bg, RegisterParams{SessionID: sess.ID, Tool: "claude", Role: "qa", Name: "alice"}, nil)
	if err != nil || a.Name != "alice" {
		t.Fatalf("%v %v", a, err)
	}
	if _, _, err := s.RegisterAgent(bg, RegisterParams{SessionID: sess.ID, Tool: "codex", Role: "qa", Name: "alice"}, nil); !errors.Is(err, ErrNameTaken) {
		t.Errorf("duplicate name: %v", err)
	}
	// names are per session
	if _, _, err := s.RegisterAgent(bg, RegisterParams{SessionID: other.ID, Tool: "codex", Role: "qa", Name: "alice"}, nil); err != nil {
		t.Errorf("same name in another session: %v", err)
	}
	for _, bad := range []string{"Bad Name", "user", "-x"} {
		if _, _, err := s.RegisterAgent(bg, RegisterParams{SessionID: sess.ID, Tool: "x", Role: "x", Name: bad}, nil); !errors.Is(err, ErrBadName) {
			t.Errorf("%q: %v", bad, err)
		}
	}
	if _, _, err := s.RegisterAgent(bg, RegisterParams{SessionID: "01ARZ3NDEKTSV4RRFFQ69G5FAV", Tool: "x", Role: "x"}, nil); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown session: %v", err)
	}
	s.EndSession(bg, other.ID)
	if _, _, err := s.RegisterAgent(bg, RegisterParams{SessionID: other.ID, Tool: "x", Role: "x", Name: "late"}, nil); !errors.Is(err, ErrSessionEnded) {
		t.Errorf("ended session: %v", err)
	}
}

func TestTokenIsStoredHashedAndResumeWorks(t *testing.T) {
	s := open(t)
	sess := newSession(t, s)
	a, tok, err := s.RegisterAgent(bg, RegisterParams{SessionID: sess.ID, Tool: "claude", Role: "dev", Name: "bob", ApproveInbound: true}, nil)
	if err != nil {
		t.Fatal(err)
	}
	var stored string
	s.r.QueryRow(`SELECT token_hash FROM agents WHERE id=?`, a.ID).Scan(&stored)
	if stored == tok || strings.Contains(stored, tok) {
		t.Fatal("token stored in the clear")
	}
	// still connected => cannot resume (would hijack a live agent)
	if _, err := s.ResumeAgent(bg, sess.ID, "bob", tok); !errors.Is(err, ErrAgentLive) {
		t.Errorf("resume while connected: %v", err)
	}
	s.SetAgentStatus(bg, a.ID, "disconnected", nil)
	if _, err := s.ResumeAgent(bg, sess.ID, "bob", "wrong"); !errors.Is(err, ErrBadToken) {
		t.Errorf("wrong token: %v", err)
	}
	if _, err := s.ResumeAgent(bg, sess.ID, "nobody", tok); !errors.Is(err, ErrNotFound) {
		t.Errorf("unknown name: %v", err)
	}
	got, err := s.ResumeAgent(bg, sess.ID, "BOB", tok) // case-insensitive
	if err != nil || got.ID != a.ID || got.Status != "connected" || !got.ApproveInbound {
		t.Fatalf("resume: %+v %v", got, err)
	}
	// exited agents can be resumed too, and lose their exit code
	code := 7
	s.SetAgentStatus(bg, a.ID, "exited", &code)
	if x, _ := s.GetAgent(bg, a.ID); x.ExitCode == nil || *x.ExitCode != 7 {
		t.Errorf("exit code not stored: %+v", x)
	}
	if got, err := s.ResumeAgent(bg, sess.ID, "bob", tok); err != nil || got.ExitCode != nil {
		t.Errorf("resume after exit: %+v %v", got, err)
	}
}

func TestListAgentsAndMarkAllDisconnected(t *testing.T) {
	s := open(t)
	sess := newSession(t, s)
	var first Agent
	for i, nm := range []string{"a1", "a2", "a3"} {
		a, _, err := s.RegisterAgent(bg, RegisterParams{SessionID: sess.ID, Tool: "claude", Role: "r", Name: nm}, nil)
		if err != nil {
			t.Fatal(err)
		}
		if i == 0 {
			first = a
		}
	}
	s.SetAgentStatus(bg, first.ID, "exited", nil)
	if live, _ := s.ListAgents(bg, sess.ID, false); len(live) != 2 || live[0].Name != "a2" {
		t.Errorf("live agents (join order): %+v", live)
	}
	if all, _ := s.ListAgents(bg, sess.ID, true); len(all) != 3 || all[0].Name != "a1" {
		t.Errorf("all agents: %+v", all)
	}
	list, _ := s.ListSessions(bg, false)
	if list[0].Agents != 2 {
		t.Errorf("agent count = %d", list[0].Agents)
	}
	s.MarkAllDisconnected(bg)
	a2, _ := s.FindAgent(bg, sess.ID, "A2")
	if a2.Status != "disconnected" {
		t.Errorf("status = %s", a2.Status)
	}
	if x, _ := s.GetAgent(bg, first.ID); x.Status != "exited" {
		t.Error("exited agents must stay exited")
	}
}

func TestAppendEventsIsIdempotentAndOrdered(t *testing.T) {
	s := open(t)
	sess := newSession(t, s)
	a, _, _ := s.RegisterAgent(bg, RegisterParams{SessionID: sess.ID, Tool: "claude", Role: "r"}, nil)
	now := time.Now()
	batch := []EventRow{{1, now, "start", `{"a":1}`}, {2, now, "resize", `{}`}, {3, now, "exit", `{"code":0}`}}
	if max, err := s.AppendEvents(bg, a.ID, batch, 0); err != nil || max != 3 {
		t.Fatalf("max=%d err=%v", max, err)
	}
	// resend overlapping + new
	if max, err := s.AppendEvents(bg, a.ID, append(batch[1:], EventRow{4, now, "x", `{}`}), 0); err != nil || max != 4 {
		t.Fatalf("resend: max=%d err=%v", max, err)
	}
	evs, _ := s.Events(bg, a.ID)
	if len(evs) != 4 || evs[0].Type != "start" || evs[3].Seq != 4 {
		t.Errorf("events: %+v", evs)
	}
	// raw events are not stored here but must still advance the ack, and it never goes backwards
	if max, _ := s.AppendEvents(bg, a.ID, nil, 90); max != 90 {
		t.Errorf("ack from raw-only batch = %d, want 90", max)
	}
	if max, _ := s.AppendEvents(bg, a.ID, nil, 10); max != 90 {
		t.Errorf("ack went backwards: %d", max)
	}
	if x, _ := s.GetAgent(bg, a.ID); x.AckedSeq != 90 {
		t.Errorf("Agent.AckedSeq = %d", x.AckedSeq)
	}
	if _, err := s.AppendEvents(bg, "no-such-agent", batch, 0); err == nil {
		t.Error("events for an unknown agent must be rejected (foreign key)")
	}
}

func TestConcurrentWritersAndReaders(t *testing.T) {
	s := open(t)
	sess := newSession(t, s)
	a, _, _ := s.RegisterAgent(bg, RegisterParams{SessionID: sess.ID, Tool: "claude", Role: "r"}, nil)
	var wg sync.WaitGroup
	for w := 0; w < 4; w++ {
		wg.Add(1)
		go func(w int) {
			defer wg.Done()
			for i := 1; i <= 100; i++ {
				seq := uint64(w*1000 + i)
				if _, err := s.AppendEvents(bg, a.ID, []EventRow{{seq, time.Now(), "e", `{}`}}, 0); err != nil {
					t.Error(err)
					return
				}
			}
		}(w)
	}
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 100; i++ {
				if _, err := s.ListAgents(bg, sess.ID, true); err != nil {
					t.Error(err)
					return
				}
			}
		}()
	}
	wg.Wait()
	if evs, _ := s.Events(bg, a.ID); len(evs) != 400 {
		t.Errorf("stored %d events, want 400", len(evs))
	}
}
