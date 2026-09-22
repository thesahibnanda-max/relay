package store

import (
	"testing"
	"time"
)

func meshAgentFixture(sessionID string) MeshAgent {
	return MeshAgent{
		AgentID: "01AG00000000000000000001", SessionID: sessionID, OwnerPeer: "peerA",
		Name: "coder", Tool: "codex", Role: "developer", Status: "connected", Version: 1,
	}
}

func TestUpsertMeshAgentIfNewerCreatesOnFirstSeen(t *testing.T) {
	s := open(t)
	sess := newSession(t, s)
	if _, err := s.EnsureMeshSession(bg, sess.ID, "secret", "self"); err != nil {
		t.Fatal(err)
	}
	a := meshAgentFixture(sess.ID)
	applied, err := s.UpsertMeshAgentIfNewer(bg, a)
	if err != nil {
		t.Fatal(err)
	}
	if !applied {
		t.Fatal("first-ever insert should be applied")
	}
	got, err := s.GetMeshAgent(bg, a.AgentID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "coder" || got.Version != 1 || got.Tombstoned {
		t.Fatalf("%+v", got)
	}
}

func TestUpsertMeshAgentIfNewerIgnoresStaleAndEqualVersions(t *testing.T) {
	s := open(t)
	sess := newSession(t, s)
	s.EnsureMeshSession(bg, sess.ID, "secret", "self")
	a := meshAgentFixture(sess.ID)
	a.Version = 5
	if _, err := s.UpsertMeshAgentIfNewer(bg, a); err != nil {
		t.Fatal(err)
	}

	stale := a
	stale.Version = 3
	stale.Name = "should-not-apply"
	applied, err := s.UpsertMeshAgentIfNewer(bg, stale)
	if err != nil {
		t.Fatal(err)
	}
	if applied {
		t.Fatal("a lower version must not be applied")
	}

	same := a
	same.Name = "also-should-not-apply"
	applied, err = s.UpsertMeshAgentIfNewer(bg, same)
	if err != nil {
		t.Fatal(err)
	}
	if applied {
		t.Fatal("an equal version must not be applied (it's a duplicate, not new information)")
	}

	got, err := s.GetMeshAgent(bg, a.AgentID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "coder" || got.Version != 5 {
		t.Fatalf("stale/equal updates leaked through: %+v", got)
	}
}

func TestUpsertMeshAgentIfNewerAppliesAHigherVersion(t *testing.T) {
	s := open(t)
	sess := newSession(t, s)
	s.EnsureMeshSession(bg, sess.ID, "secret", "self")
	a := meshAgentFixture(sess.ID)
	s.UpsertMeshAgentIfNewer(bg, a)

	newer := a
	newer.Version = 2
	newer.Name = "coder-renamed"
	newer.Status = "disconnected"
	applied, err := s.UpsertMeshAgentIfNewer(bg, newer)
	if err != nil {
		t.Fatal(err)
	}
	if !applied {
		t.Fatal("a strictly higher version must be applied")
	}
	got, err := s.GetMeshAgent(bg, a.AgentID)
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "coder-renamed" || got.Status != "disconnected" || got.Version != 2 {
		t.Fatalf("%+v", got)
	}
}

func TestListMeshAgentsExcludesTombstoned(t *testing.T) {
	s := open(t)
	sess := newSession(t, s)
	s.EnsureMeshSession(bg, sess.ID, "secret", "self")
	live := meshAgentFixture(sess.ID)
	s.UpsertMeshAgentIfNewer(bg, live)

	gone := meshAgentFixture(sess.ID)
	gone.AgentID = "01AG00000000000000000002"
	gone.Name = "old-agent"
	gone.Tombstoned = true
	s.UpsertMeshAgentIfNewer(bg, gone)

	list, err := s.ListMeshAgents(bg, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(list) != 1 || list[0].AgentID != live.AgentID {
		t.Fatalf("%+v", list)
	}
	// but GetMeshAgent must still find the tombstoned one, so a caller can
	// tell "tombstoned" apart from "never heard of it".
	tomb, err := s.GetMeshAgent(bg, gone.AgentID)
	if err != nil {
		t.Fatal(err)
	}
	if !tomb.Tombstoned {
		t.Fatal("expected the tombstoned row back from GetMeshAgent")
	}
}

func TestGetMeshAgentNotFound(t *testing.T) {
	s := open(t)
	if _, err := s.GetMeshAgent(bg, "nobody"); err != ErrNotFound {
		t.Fatalf("got %v, want ErrNotFound", err)
	}
}

func TestDeleteSessionRemovesMeshAgents(t *testing.T) {
	s := open(t)
	sess := newSession(t, s)
	s.EnsureMeshSession(bg, sess.ID, "secret", "self")
	a := meshAgentFixture(sess.ID)
	s.UpsertMeshAgentIfNewer(bg, a)
	if err := s.DeleteSession(bg, sess.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetMeshAgent(bg, a.AgentID); err != ErrNotFound {
		t.Fatalf("mesh_agents row survived DeleteSession: err=%v", err)
	}
}

func TestRenameAgentChangesTheLocalName(t *testing.T) {
	s := open(t)
	sess := newSession(t, s)
	agent, _, err := s.RegisterAgent(bg, RegisterParams{SessionID: sess.ID, Tool: "codex", Role: "developer", Name: "alice"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.RenameAgent(bg, agent.ID, "alice-x1", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got != "alice-x1" {
		t.Fatalf("RenameAgent returned %q", got)
	}
	fresh, err := s.GetAgent(bg, agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Name != "alice-x1" {
		t.Fatalf("name did not persist: %+v", fresh)
	}
}

func TestRenameAgentRetriesOnCollisionWithAnotherLocalAgent(t *testing.T) {
	s := open(t)
	sess := newSession(t, s)
	s.RegisterAgent(bg, RegisterParams{SessionID: sess.ID, Tool: "codex", Role: "developer", Name: "taken"}, nil)
	loser, _, err := s.RegisterAgent(bg, RegisterParams{SessionID: sess.ID, Tool: "codex", Role: "developer", Name: "alice"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	got, err := s.RenameAgent(bg, loser.ID, "taken", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got == "taken" {
		t.Fatal("RenameAgent must not silently collide with an existing name")
	}
	fresh, err := s.GetAgent(bg, loser.ID)
	if err != nil {
		t.Fatal(err)
	}
	if fresh.Name != got {
		t.Fatalf("returned name %q does not match what was persisted %q", got, fresh.Name)
	}
}

func TestRenameAgentBumpsLastSeenAt(t *testing.T) {
	// Gossip uses last_seen_at as the agent's version number; a rename must
	// look strictly newer than whatever a peer already cached, or the
	// corrected name would never win.
	s := open(t)
	sess := newSession(t, s)
	agent, _, err := s.RegisterAgent(bg, RegisterParams{SessionID: sess.ID, Tool: "codex", Role: "developer", Name: "alice"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond) // ensure a distinguishable millisecond tick
	if _, err := s.RenameAgent(bg, agent.ID, "alice-x1", nil); err != nil {
		t.Fatal(err)
	}
	fresh, err := s.GetAgent(bg, agent.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !fresh.LastSeenAt.After(agent.LastSeenAt) {
		t.Fatalf("last_seen_at did not advance: before=%v after=%v", agent.LastSeenAt, fresh.LastSeenAt)
	}
}

func TestRenameAgentNotFound(t *testing.T) {
	s := open(t)
	if _, err := s.RenameAgent(bg, "nobody", "x", nil); err != ErrNotFound {
		t.Fatalf("got %v, want ErrNotFound", err)
	}
}
