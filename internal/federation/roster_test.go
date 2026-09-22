package federation

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/thesahibnanda-max/relay/internal/meshnet"
	"github.com/thesahibnanda-max/relay/internal/proto"
	"github.com/thesahibnanda-max/relay/internal/store"
)

// waitForPeerCount polls until a session has exactly n recorded mesh peers.
// The accepting side of a handshake (handleInbound) always records its
// peer/agent state from its own background goroutine, so a test that just
// called Join must not assume the *other* side has caught up yet.
func waitForPeerCount(t *testing.T, s *store.Store, sessionID string, n int) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		peers, _ := s.ListMeshPeers(context.Background(), sessionID)
		if len(peers) == n {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %d peers, have %d", n, len(peers))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// fakeRouter is a minimal, in-memory LocalRouter for tests: a fixed set of
// "local" agents a test can mutate directly, plus a record of every rename
// applied (so a test can assert one happened, and to whom).
type fakeRouter struct {
	mu      sync.Mutex
	agents  map[string]proto.MeshAgentInfo // by agent id
	renamed []renameCall
}

type renameCall struct {
	AgentID, ProposedName, AppliedName string
}

func newFakeRouter(agents ...proto.MeshAgentInfo) *fakeRouter {
	r := &fakeRouter{agents: map[string]proto.MeshAgentInfo{}}
	for _, a := range agents {
		r.agents[a.AgentID] = a
	}
	return r
}

func (r *fakeRouter) LocalAgents(ctx context.Context, sessionID string) ([]proto.MeshAgentInfo, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := make([]proto.MeshAgentInfo, 0, len(r.agents))
	for _, a := range r.agents {
		out = append(out, a)
	}
	return out, nil
}

func (r *fakeRouter) RenameLocalAgent(ctx context.Context, agentID, proposedName string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	a := r.agents[agentID]
	a.Name = proposedName
	a.Version++
	a.LastSeenAt = time.Now()
	r.agents[agentID] = a
	r.renamed = append(r.renamed, renameCall{AgentID: agentID, ProposedName: proposedName, AppliedName: proposedName})
	return proposedName, nil
}

// rename simulates an external rename (e.g. the human running `relay send`
// or whatever future command changes a name) - unlike RenameLocalAgent,
// nothing in production calls this; it exists so a test can change an
// agent's name and then exercise Resync's propagation on its own. It still
// has to bump Version, exactly as a real local-state change would (see
// store.RenameAgent bumping last_seen_at) - an update gossip's version gate
// can't tell apart from a stale duplicate is correctly ignored, by design.
func (r *fakeRouter) rename(id, name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	a := r.agents[id]
	a.Name = name
	a.Version++
	r.agents[id] = a
}

func (r *fakeRouter) renameCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.renamed)
}

func (r *fakeRouter) lastRename() (renameCall, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if len(r.renamed) == 0 {
		return renameCall{}, false
	}
	return r.renamed[len(r.renamed)-1], true
}

// newTestHubWithRouter is like newTestHub (federation_test.go) but also
// wires a LocalRouter, since most of federation_test.go's coverage
// predates roster gossip and deliberately leaves Router nil.
func newTestHubWithRouter(t *testing.T, net *meshnet.FakeNetwork, name string, router LocalRouter) testHub {
	t.Helper()
	h := newTestHub(t, net, name)
	h.opt.Router = router
	return h
}

func TestJoinExchangesRostersInBothDirections(t *testing.T) {
	net := meshnet.NewFakeNetwork()
	aRouter := newFakeRouter(proto.MeshAgentInfo{AgentID: "01AG0000000000000000000A", Name: "lead", Tool: "claude", Role: "orchestrator", Status: "connected", Version: 1})
	bRouter := newFakeRouter(proto.MeshAgentInfo{AgentID: "01AG0000000000000000000B", Name: "coder", Tool: "codex", Role: "developer", Status: "connected", Version: 1})
	a := newTestHubWithRouter(t, net, "a", aRouter)
	b := newTestHubWithRouter(t, net, "b", bRouter)
	ctx := withTimeout(t)

	sessionID := "01SESS0000000000000000001"
	blob, err := a.Invite(ctx, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Join(ctx, blob); err != nil {
		t.Fatal(err)
	}

	// b learned about a's agent from the Welcome.
	bAgents, err := b.store.ListMeshAgents(ctx, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(bAgents) != 1 || bAgents[0].Name != "lead" || bAgents[0].OwnerPeer != a.opt.Identity.PeerID() {
		t.Fatalf("b's known mesh agents: %+v", bAgents)
	}

	// a learned about b's agent from the Hello (handleInbound runs in its
	// own goroutine; poll briefly).
	deadline := time.Now().Add(2 * time.Second)
	for {
		got, _ := a.store.ListMeshAgents(ctx, sessionID)
		if len(got) == 1 {
			if got[0].Name != "coder" || got[0].OwnerPeer != b.opt.Identity.PeerID() {
				t.Fatalf("a's known mesh agents: %+v", got)
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("a never learned about b's agent: %+v", got)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestNameCollisionResolvesDeterministicallyByRenamingTheLaterAgent(t *testing.T) {
	net := meshnet.NewFakeNetwork()
	// Same name on both sides, registered before either daemon has heard of
	// the other - the exact race the plan's walkthrough (d) describes.
	earlier := proto.MeshAgentInfo{AgentID: "01EARLIER00000000000000A", Name: "alice", Tool: "claude", Role: "developer", Status: "connected", Version: 1}
	later := proto.MeshAgentInfo{AgentID: "01LATER000000000000000B", Name: "alice", Tool: "codex", Role: "developer", Status: "connected", Version: 1}
	aRouter := newFakeRouter(earlier)
	bRouter := newFakeRouter(later)
	a := newTestHubWithRouter(t, net, "a", aRouter)
	b := newTestHubWithRouter(t, net, "b", bRouter)
	ctx := withTimeout(t)

	sessionID := "01SESS0000000000000000002"
	blob, err := a.Invite(ctx, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Join(ctx, blob); err != nil {
		t.Fatal(err)
	}

	// b's own later-created agent must have been renamed: it lost the
	// collision to a's earlier one (smaller ULID).
	deadline := time.Now().Add(2 * time.Second)
	for {
		if bRouter.renameCount() > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("b's colliding agent was never renamed")
		}
		time.Sleep(10 * time.Millisecond)
	}
	call, ok := bRouter.lastRename()
	if !ok || call.AgentID != later.AgentID {
		t.Fatalf("wrong agent renamed: %+v", call)
	}
	if call.AppliedName == "alice" {
		t.Fatal("renamed agent still has the colliding name")
	}
	if aRouter.renameCount() != 0 {
		t.Fatal("a's earlier agent must never be renamed: it won the collision")
	}
}

func TestResyncPropagatesARenameToAKnownPeer(t *testing.T) {
	net := meshnet.NewFakeNetwork()
	aRouter := newFakeRouter(proto.MeshAgentInfo{AgentID: "01AG0000000000000000000A", Name: "lead", Version: 1})
	bRouter := newFakeRouter()
	a := newTestHubWithRouter(t, net, "a", aRouter)
	b := newTestHubWithRouter(t, net, "b", bRouter)
	ctx := withTimeout(t)

	sessionID := "01SESS0000000000000000003"
	blob, err := a.Invite(ctx, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Join(ctx, blob); err != nil {
		t.Fatal(err)
	}
	// a's own side of the handshake (handleInbound) runs in a background
	// goroutine; wait for it to have actually recorded b as a peer before
	// resyncing, or a.Resync would find no one to talk to yet.
	waitForPeerCount(t, a.store, sessionID, 1)

	// a's agent is renamed by something outside gossip (e.g. the human), and
	// a proactively resyncs - simulating what the daemon wiring will trigger
	// on any local roster change.
	aRouter.rename("01AG0000000000000000000A", "lead-renamed")
	a.Resync(ctx, sessionID)

	got, err := b.store.GetMeshAgent(ctx, "01AG0000000000000000000A")
	if err != nil {
		t.Fatal(err)
	}
	if got.Name != "lead-renamed" {
		t.Fatalf("b did not pick up the rename via resync: %+v", got)
	}
}

func TestResyncMarksAnUnreachablePeerWithoutFailingOthers(t *testing.T) {
	net := meshnet.NewFakeNetwork()
	a := newTestHub(t, net, "a")
	b := newTestHub(t, net, "b")
	ctx := withTimeout(t)

	sessionID := "01SESS0000000000000000004"
	blob, err := a.Invite(ctx, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Join(ctx, blob); err != nil {
		t.Fatal(err)
	}

	// b goes away.
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	a.Resync(ctx, sessionID) // must not panic or block forever

	peers, err := a.store.ListMeshPeers(ctx, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(peers) != 1 || peers[0].Status != "unreachable" {
		t.Fatalf("%+v", peers)
	}
}

func TestGossipedAgentClaimingToBeUsIsIgnored(t *testing.T) {
	// A peer should never gossip an agent_id we minted ourselves, but a Hub
	// must not blindly trust that either - defence in depth against a buggy
	// or malicious peer trying to shadow one of our own agents.
	net := meshnet.NewFakeNetwork()
	ourAgent := proto.MeshAgentInfo{AgentID: "01OURS00000000000000000A", Name: "lead", Version: 1}
	aRouter := newFakeRouter(ourAgent)
	a := newTestHubWithRouter(t, net, "a", aRouter)
	ctx := withTimeout(t)
	sessionID := "01SESS0000000000000000005"
	if _, err := a.Invite(ctx, sessionID); err != nil {
		t.Fatal(err)
	}

	// Directly exercise applyGossipedAgent as if a peer had claimed our own
	// agent id under a different name.
	if err := a.applyGossipedAgent(ctx, sessionID, proto.MeshAgentInfo{
		AgentID: ourAgent.AgentID, OwnerPeer: "someone-else", Name: "hijacked", Version: 99,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := a.store.GetMeshAgent(ctx, ourAgent.AgentID); err == nil {
		t.Fatal("our own agent id must never be recorded as a gossiped/remote agent")
	}
}
