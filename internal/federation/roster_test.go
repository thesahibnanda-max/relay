package federation

import (
	"context"
	"fmt"
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
	mu        sync.Mutex
	agents    map[string]proto.MeshAgentInfo // by agent id
	renamed   []renameCall
	messages  map[string]*fakeMessage
	delivered []proto.MeshMsgHandoff // every handoff DeliverInbound was ever called with (including resends)
}

// fakeMessage is a minimal in-memory message row backing fakeRouter's
// DeliverInbound/ApplyReceipt/Pending* implementations - enough to exercise
// the wire mechanics (handoff/receipt round trip, idempotent resend,
// rev-gated staleness) without needing a real store.Store. Real hold-policy
// decisions (hop limit, approve-inbound) are a daemon-level concern
// (internal/daemon's DeliverInbound), not exercised here.
type fakeMessage struct {
	proto.MeshMsgHandoff
	state, detail string
	rev           uint64
	mirror        bool   // true = this fake daemon sent it and is awaiting a receipt; false = it owns the row
	ownerPeer     string // mirror row's destination peer, or the authoritative row's sender peer
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

func (r *fakeRouter) DeliverInbound(ctx context.Context, sessionID string, h proto.MeshMsgHandoff) (proto.MeshMsgReceipt, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.delivered = append(r.delivered, h)
	if r.messages == nil {
		r.messages = map[string]*fakeMessage{}
	}
	if m, ok := r.messages[h.ID]; ok {
		return proto.MeshMsgReceipt{ID: m.ID, State: m.state, Detail: m.detail, Rev: m.rev}, nil
	}
	m := &fakeMessage{MeshMsgHandoff: h, state: "queued", rev: 1, ownerPeer: h.FromPeer}
	r.messages[h.ID] = m
	return proto.MeshMsgReceipt{ID: m.ID, State: m.state, Rev: m.rev}, nil
}

func (r *fakeRouter) ApplyReceipt(ctx context.Context, fromPeer string, rc proto.MeshMsgReceipt) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	m, ok := r.messages[rc.ID]
	if !ok {
		return fmt.Errorf("fakeRouter: no such message %s", rc.ID)
	}
	if m.ownerPeer != fromPeer {
		return fmt.Errorf("fakeRouter: receipt for %s from unexpected peer %s (want %s)", rc.ID, fromPeer, m.ownerPeer)
	}
	if m.rev >= rc.Rev {
		return nil // stale or duplicate; ignored, same as store.ApplyReceipt
	}
	m.state, m.detail, m.rev = rc.State, rc.Detail, rc.Rev
	return nil
}

func (r *fakeRouter) PendingHandoffs(ctx context.Context, sessionID, peerID string) ([]proto.MeshMsgHandoff, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []proto.MeshMsgHandoff
	for _, m := range r.messages {
		if m.mirror && m.ownerPeer == peerID && m.rev == 0 {
			out = append(out, m.MeshMsgHandoff)
		}
	}
	return out, nil
}

func (r *fakeRouter) PendingReceipts(ctx context.Context, sessionID, peerID string) ([]proto.MeshMsgReceipt, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []proto.MeshMsgReceipt
	for _, m := range r.messages {
		if !m.mirror && m.ownerPeer == peerID {
			out = append(out, proto.MeshMsgReceipt{ID: m.ID, State: m.state, Detail: m.detail, Rev: m.rev})
		}
	}
	return out, nil
}

// sendMirror simulates routeSend creating a mirror row for a message this
// fake daemon is sending to a remote agent - a test helper, not part of
// LocalRouter.
func (r *fakeRouter) sendMirror(h proto.MeshMsgHandoff, toPeer string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.messages == nil {
		r.messages = map[string]*fakeMessage{}
	}
	r.messages[h.ID] = &fakeMessage{MeshMsgHandoff: h, state: "queued", mirror: true, ownerPeer: toPeer}
}

func (r *fakeRouter) messageState(id string) (state string, rev uint64, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	m, ok := r.messages[id]
	if !ok {
		return "", 0, false
	}
	return m.state, m.rev, true
}

func (r *fakeRouter) deliveredCount() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.delivered)
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

func TestHandoffRoundTripDeliversAndAppliesReceipt(t *testing.T) {
	net := meshnet.NewFakeNetwork()
	aRouter := newFakeRouter()
	bRouter := newFakeRouter()
	a := newTestHubWithRouter(t, net, "a", aRouter)
	b := newTestHubWithRouter(t, net, "b", bRouter)
	ctx := withTimeout(t)

	sessionID := "01SESS0000000000000000010"
	blob, err := a.Invite(ctx, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Join(ctx, blob); err != nil {
		t.Fatal(err)
	}
	bPeerID := b.opt.Identity.PeerID()

	hs := proto.MeshMsgHandoff{ID: "01MSG0000000000000000000A", SessionID: sessionID, ToAgent: "bob", ToName: "bob", FromName: "alice", Body: "hi"}
	aRouter.sendMirror(hs, bPeerID)

	receipt, err := a.SendMessage(ctx, sessionID, bPeerID, hs)
	if err != nil {
		t.Fatal(err)
	}
	if receipt.State != "queued" || receipt.Rev != 1 {
		t.Fatalf("receipt: %+v", receipt)
	}
	if bRouter.deliveredCount() != 1 {
		t.Fatalf("b.DeliverInbound calls = %d, want 1", bRouter.deliveredCount())
	}
	if state, rev, ok := aRouter.messageState(hs.ID); !ok || state != "queued" || rev != 1 {
		t.Fatalf("a's mirror row after the round trip: state=%q rev=%d ok=%v", state, rev, ok)
	}

	// bob answers it; b tells a about the final state asynchronously.
	if err := b.SendReceipt(ctx, sessionID, a.opt.Identity.PeerID(), proto.MeshMsgReceipt{ID: hs.ID, State: "done", Rev: 3}); err != nil {
		t.Fatal(err)
	}
	if state, rev, ok := aRouter.messageState(hs.ID); !ok || state != "done" || rev != 3 {
		t.Fatalf("a's mirror row after the async receipt: state=%q rev=%d ok=%v", state, rev, ok)
	}
}

func TestHandoffResendIsIdempotent(t *testing.T) {
	net := meshnet.NewFakeNetwork()
	aRouter := newFakeRouter()
	bRouter := newFakeRouter()
	a := newTestHubWithRouter(t, net, "a", aRouter)
	b := newTestHubWithRouter(t, net, "b", bRouter)
	ctx := withTimeout(t)

	sessionID := "01SESS0000000000000000011"
	blob, err := a.Invite(ctx, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Join(ctx, blob); err != nil {
		t.Fatal(err)
	}
	bPeerID := b.opt.Identity.PeerID()

	hs := proto.MeshMsgHandoff{ID: "01MSG0000000000000000000B", SessionID: sessionID, ToAgent: "bob", ToName: "bob", FromName: "alice", Body: "hi"}
	aRouter.sendMirror(hs, bPeerID)
	for i := 0; i < 3; i++ {
		hs.Resend = i > 0
		if _, err := a.SendMessage(ctx, sessionID, bPeerID, hs); err != nil {
			t.Fatalf("attempt %d: %v", i, err)
		}
	}
	if bRouter.deliveredCount() != 3 {
		t.Fatalf("DeliverInbound calls = %d, want 3 (fakeRouter records every call)", bRouter.deliveredCount())
	}
	if len(bRouter.messages) != 1 {
		t.Fatalf("resending the same handoff must never create a second row: %d rows", len(bRouter.messages))
	}
}

func TestReceiptFromTheWrongPeerIsRejected(t *testing.T) {
	aRouter := newFakeRouter()
	ctx := context.Background()

	aRouter.sendMirror(proto.MeshMsgHandoff{ID: "01MSG0000000000000000000C"}, "nodekey:the-real-owner")

	// A different peer than the one this mirror row was actually handed off
	// to must not be able to move its state.
	err := aRouter.ApplyReceipt(ctx, "nodekey:some-impostor", proto.MeshMsgReceipt{ID: "01MSG0000000000000000000C", State: "done", Rev: 5})
	if err == nil {
		t.Fatal("expected the receipt to be rejected")
	}
	if state, _, _ := aRouter.messageState("01MSG0000000000000000000C"); state != "queued" {
		t.Fatalf("state must be unchanged after a rejected receipt, got %q", state)
	}
}

func TestHandoffFromAPeerNeverLinkedToTheSessionIsRejected(t *testing.T) {
	net := meshnet.NewFakeNetwork()
	bRouter := newFakeRouter()
	a := newTestHub(t, net, "a")
	b := newTestHubWithRouter(t, net, "b", bRouter)
	ctx := withTimeout(t)
	sessionID := "01SESS0000000000000000013"

	// b brings up its listener without a ever having joined it - no
	// mesh_peers row exists on b's side for a's identity, simulating a
	// stranger dialing in cold.
	bAddr, err := b.ensureListening(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := a.store.EnsureMeshSession(ctx, sessionID, "secret", a.opt.Identity.PeerID()); err != nil {
		t.Fatal(err)
	}
	if err := a.store.UpsertMeshPeer(ctx, store.MeshPeer{
		SessionID: sessionID, PeerID: b.opt.Identity.PeerID(), Addr: string(bAddr), Status: store.MeshPeerLinked,
	}); err != nil {
		t.Fatal(err)
	}

	_, err = a.SendMessage(ctx, sessionID, b.opt.Identity.PeerID(), proto.MeshMsgHandoff{ID: "01MSG0000000000000000000D", SessionID: sessionID, ToAgent: "bob"})
	if err == nil {
		t.Fatal("expected the handoff to be refused")
	}
	if bRouter.deliveredCount() != 0 {
		t.Fatal("b must never have called DeliverInbound for a peer it never linked")
	}
}

func TestHandoffClaimingAWrongFromPeerIsRejected(t *testing.T) {
	net := meshnet.NewFakeNetwork()
	bRouter := newFakeRouter()
	a := newTestHub(t, net, "a")
	b := newTestHubWithRouter(t, net, "b", bRouter)
	ctx := withTimeout(t)
	sessionID := "01SESS0000000000000000014"

	blob, err := b.Invite(ctx, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Join(ctx, blob); err != nil {
		t.Fatal(err)
	}
	waitForPeerCount(t, b.store, sessionID, 1)

	// a dials in honestly (the tunnel verifies it as a), but the handoff
	// payload claims to be from some other peer - a lie no honest
	// SendMessage would ever tell, since it always stamps FromPeer with the
	// dialer's own identity.
	conn, err := (meshnet.FakeTransport{Net: net}).Dial(ctx, a.opt.Identity, meshnet.Addr(blob.PeerAddr))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	b64, err := proto.MeshMarshal(proto.MeshTypeMsgHandoff, proto.MeshMsgHandoff{
		ID: "01MSG0000000000000000000E", SessionID: sessionID, ToAgent: "bob", FromPeer: "nodekey:not-really-a",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := writeFrame(conn, b64); err != nil {
		t.Fatal(err)
	}
	env := readErrorReply(t, conn)
	if env.Code != proto.MeshCodeKeyChanged {
		t.Fatalf("code = %q, want %q", env.Code, proto.MeshCodeKeyChanged)
	}
	if bRouter.deliveredCount() != 0 {
		t.Fatal("a handoff claiming a mismatched from_peer must never be delivered")
	}
}

func TestResyncFlushesPendingHandoffsAndReceiptsToAReconnectedPeer(t *testing.T) {
	net := meshnet.NewFakeNetwork()
	aRouter := newFakeRouter()
	bRouter := newFakeRouter()
	a := newTestHubWithRouter(t, net, "a", aRouter)
	b := newTestHubWithRouter(t, net, "b", bRouter)
	ctx := withTimeout(t)

	sessionID := "01SESS0000000000000000015"
	blob, err := a.Invite(ctx, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Join(ctx, blob); err != nil {
		t.Fatal(err)
	}
	waitForPeerCount(t, a.store, sessionID, 1)
	bPeerID := b.opt.Identity.PeerID()
	aPeerID := a.opt.Identity.PeerID()

	// a has a mirror row that was created but never actually sent (e.g. this
	// daemon restarted before its first delivery attempt) - Resync must
	// still pick it up and deliver it.
	pending := proto.MeshMsgHandoff{ID: "01MSG0000000000000000000F", SessionID: sessionID, ToAgent: "bob", ToName: "bob", FromName: "alice", Body: "catch me up"}
	aRouter.sendMirror(pending, bPeerID)

	// b, symmetrically, is authoritative for a message from a's agent whose
	// very first handoff+receipt already succeeded (a's mirror row is at
	// rev 1, so PendingHandoffs won't also try to resend it as unsent) but
	// whose state has since moved on to something a was never told about.
	aRouter.mu.Lock()
	aRouter.messages["01MSG0000000000000000000G"] = &fakeMessage{
		MeshMsgHandoff: proto.MeshMsgHandoff{ID: "01MSG0000000000000000000G", SessionID: sessionID},
		state:          "queued", rev: 1, mirror: true, ownerPeer: bPeerID,
	}
	aRouter.mu.Unlock()
	bRouter.mu.Lock()
	bRouter.messages = map[string]*fakeMessage{
		"01MSG0000000000000000000G": {
			MeshMsgHandoff: proto.MeshMsgHandoff{ID: "01MSG0000000000000000000G", SessionID: sessionID, ToAgent: "bob"},
			state:          "held", rev: 2, ownerPeer: aPeerID,
		},
	}
	bRouter.mu.Unlock()

	// Each daemon's own Resync only flushes ITS OWN outstanding outbox to a
	// peer - a's pending handoff is a's job to push, b's pending receipt is
	// b's job to push. In production both fire independently off their own
	// local roster/message events; a test exercising both directions has to
	// invoke both sides.
	a.Resync(ctx, sessionID)
	b.Resync(ctx, sessionID)

	if bRouter.deliveredCount() != 1 {
		t.Fatalf("b.DeliverInbound calls = %d, want 1 (the pending handoff)", bRouter.deliveredCount())
	}
	if state, rev, ok := aRouter.messageState(pending.ID); !ok || state != "queued" || rev != 1 {
		t.Fatalf("a's mirror row after resync: state=%q rev=%d ok=%v", state, rev, ok)
	}
	if state, rev, ok := aRouter.messageState("01MSG0000000000000000000G"); !ok || state != "held" || rev != 2 {
		t.Fatalf("a should have learned b's pending receipt via resync: state=%q rev=%d ok=%v", state, rev, ok)
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
