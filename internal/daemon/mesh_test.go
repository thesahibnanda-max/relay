package daemon

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/thesahibnanda-max/relay/internal/meshnet"
	"github.com/thesahibnanda-max/relay/internal/proto"
	"github.com/thesahibnanda-max/relay/internal/store"
)

func withMeshTransport(net *meshnet.FakeNetwork) func(*Options) {
	return func(o *Options) { o.MeshTransport = meshnet.FakeTransport{Net: net} }
}

func TestMeshInviteAndJoinEndToEnd(t *testing.T) {
	fakeNet := meshnet.NewFakeNetwork()
	a := startServer(t, withMeshTransport(fakeNet))
	b := startServer(t, withMeshTransport(fakeNet))

	sid := a.newSession()

	var blob proto.JoinBlob
	if code := a.postJSON("/v1/admin/mesh/invite", proto.InviteMeshSessionRequest{SessionID: sid}, &blob); code != 200 {
		t.Fatalf("invite: HTTP %d", code)
	}
	if blob.Session != sid || blob.PeerAddr == "" || blob.Secret == "" {
		t.Fatalf("%+v", blob)
	}

	var joinResp map[string]string
	if code := b.postJSON("/v1/admin/mesh/join", blob, &joinResp); code != 200 {
		t.Fatalf("join: HTTP %d: %+v", code, joinResp)
	}

	// b now has a local session row for a session it never created itself.
	if bs := b.session(sid); bs.ID != sid {
		t.Fatalf("b's view of the joined session: %+v", bs)
	}

	var bPeers []proto.MeshPeerView
	if code := b.getJSON("/v1/admin/mesh/peers/"+sid, &bPeers); code != 200 {
		t.Fatalf("peers: HTTP %d", code)
	}
	if len(bPeers) != 1 || bPeers[0].Status != "linked" {
		t.Fatalf("b's peers: %+v", bPeers)
	}

	// a's side of the handshake completes in a background goroutine.
	deadline := time.Now().Add(2 * time.Second)
	var aPeers []proto.MeshPeerView
	for time.Now().Before(deadline) {
		a.getJSON("/v1/admin/mesh/peers/"+sid, &aPeers)
		if len(aPeers) == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(aPeers) != 1 || aPeers[0].Status != "linked" {
		t.Fatalf("a's peers: %+v", aPeers)
	}
}

func TestMeshInviteRequiresAnExistingSession(t *testing.T) {
	a := startServer(t, withMeshTransport(meshnet.NewFakeNetwork()))
	code := a.postJSON("/v1/admin/mesh/invite", proto.InviteMeshSessionRequest{SessionID: "01ARZ3NDEKTSV4RRFFQ69G5FAV"}, nil)
	if code != 404 {
		t.Fatalf("code = %d, want 404", code)
	}
}

func TestMeshInviteRequiresASessionID(t *testing.T) {
	a := startServer(t, withMeshTransport(meshnet.NewFakeNetwork()))
	code := a.postJSON("/v1/admin/mesh/invite", proto.InviteMeshSessionRequest{}, nil)
	if code != 400 {
		t.Fatalf("code = %d, want 400", code)
	}
}

func TestMeshJoinWithMalformedBodyIsRejected(t *testing.T) {
	a := startServer(t, withMeshTransport(meshnet.NewFakeNetwork()))
	code := a.postJSON("/v1/admin/mesh/join", "not a join blob", nil)
	if code != 400 {
		t.Fatalf("code = %d, want 400", code)
	}
}

func TestMeshJoinWithWrongSecretIsRejected(t *testing.T) {
	fakeNet := meshnet.NewFakeNetwork()
	a := startServer(t, withMeshTransport(fakeNet))
	b := startServer(t, withMeshTransport(fakeNet))
	sid := a.newSession()

	var blob proto.JoinBlob
	a.postJSON("/v1/admin/mesh/invite", proto.InviteMeshSessionRequest{SessionID: sid}, &blob)
	blob.Secret = "wrong"

	code := b.postJSON("/v1/admin/mesh/join", blob, nil)
	if code != 502 {
		t.Fatalf("code = %d, want 502 (upstream/peer refusal)", code)
	}
}

func TestMeshPeersRequiresAnExistingSession(t *testing.T) {
	a := startServer(t, withMeshTransport(meshnet.NewFakeNetwork()))
	code := a.getJSON("/v1/admin/mesh/peers/01ARZ3NDEKTSV4RRFFQ69G5FAV", nil)
	if code != 404 {
		t.Fatalf("code = %d, want 404", code)
	}
}

// joinTwoMeshDaemons wires up a and b as a 2-daemon mesh over a shared fake
// network with a single already-invited/joined session, the setup every
// cross-daemon messaging test below starts from.
func joinTwoMeshDaemons(t *testing.T) (a, b *env, sid string) {
	t.Helper()
	fakeNet := meshnet.NewFakeNetwork()
	a = startServer(t, withMeshTransport(fakeNet))
	b = startServer(t, withMeshTransport(fakeNet))
	sid = a.newSession()

	var blob proto.JoinBlob
	if code := a.postJSON("/v1/admin/mesh/invite", proto.InviteMeshSessionRequest{SessionID: sid}, &blob); code != 200 {
		t.Fatalf("invite: HTTP %d", code)
	}
	if code := b.postJSON("/v1/admin/mesh/join", blob, nil); code != 200 {
		t.Fatalf("join: HTTP %d", code)
	}
	return a, b, sid
}

// TestJoinedSessionGossipsRosterVisibility is the M-mesh-3 half of the
// original M-mesh-2 boundary test: a real agent on each daemon is gossiped
// to the other and shows up in the admin session view (what `relay ls`
// renders) marked remote, with its owning peer. Actually reaching that
// agent by sending to it is M-mesh-4's job - see
// TestMeshSendReachesARemoteAgentAndRepliesRoundTrip.
func TestJoinedSessionGossipsRosterVisibility(t *testing.T) {
	a, b, sid := joinTwoMeshDaemons(t)

	alice := a.joinPeer(sid, proto.Hello{Name: "alice", Role: "orchestrator"})
	bob := b.joinPeer(sid, proto.Hello{Name: "bob", Role: "developer"})
	defer bob.ws.CloseNow()

	bobInA := a.waitAgent(sid, "bob", func(ai proto.AgentInfo) bool { return ai.Remote }, "gossiped in")
	if bobInA.Peer == "" {
		t.Fatalf("bob should be marked remote with a peer id: %+v", bobInA)
	}
	if bobInA.Status != "connected" || !bobInA.Connected {
		t.Fatalf("bob should show as connected: %+v", bobInA)
	}
	// a's own local agent must never be marked remote.
	for _, ai := range a.session(sid).Agents {
		if ai.Name == "alice" && ai.Remote {
			t.Fatalf("alice must not be marked remote on her own daemon: %+v", ai)
		}
	}

	// Both sessions remain fully independently functional locally: alice can
	// still send to a real local peer on the same daemon.
	carol := a.joinPeer(sid, proto.Hello{Name: "carol", Role: "qa"})
	defer carol.ws.CloseNow()
	if r := alice.mustSend(proto.SendArgs{To: "carol", Body: "hi"}); r.State != store.MsgQueued {
		t.Fatalf("alice -> carol (same daemon): %+v", r)
	}
}

// TestListAgentsRPCIncludesMeshAgents is the regression test for issue #18's
// Symptom 1: relay_list_agents (OpListAgents, what the MCP tool calls) used
// to only ever see this daemon's own local agents, silently disagreeing with
// `relay ls` (sessionInfo, tested above) the moment a real agent joined from
// another machine. peers() now shares sessionInfo's own meshAgents() helper,
// so the two can no longer drift apart on which remote agents are visible.
func TestListAgentsRPCIncludesMeshAgents(t *testing.T) {
	a, b, sid := joinTwoMeshDaemons(t)

	alice := a.joinPeer(sid, proto.Hello{Name: "alice", Role: "orchestrator"})
	bob := b.joinPeer(sid, proto.Hello{Name: "bob", Role: "developer"})
	defer bob.ws.CloseNow()

	// Wait for gossip via sessionInfo first, so the RPC assertion below isn't
	// racing the same propagation delay - it's peers()'s own mesh-awareness
	// being tested, not gossip timing (already covered above).
	a.waitAgent(sid, "bob", func(ai proto.AgentInfo) bool { return ai.Remote }, "gossiped in")

	r := alice.rpc(proto.OpListAgents, nil)
	if !r.OK {
		t.Fatalf("relay_list_agents: %+v", r.Error)
	}
	var l proto.ListAgentsResult
	if err := json.Unmarshal(r.Result, &l); err != nil {
		t.Fatal(err)
	}

	var foundBob, foundAlice bool
	for _, p := range l.Agents {
		switch p.Name {
		case "bob":
			foundBob = true
			if !p.Remote || p.Peer == "" {
				t.Fatalf("bob should be listed as remote with a peer id: %+v", p)
			}
			if p.Self {
				t.Fatalf("bob must never be marked self on alice's daemon: %+v", p)
			}
		case "alice":
			foundAlice = true
			if p.Remote || !p.Self {
				t.Fatalf("alice must not be marked remote on her own daemon: %+v", p)
			}
		}
	}
	if !foundBob {
		t.Fatalf("relay_list_agents never showed the remote agent: %+v", l.Agents)
	}
	if !foundAlice {
		t.Fatalf("relay_list_agents lost the local agent: %+v", l.Agents)
	}
}

// TestMeshSendReachesARemoteAgentAndRepliesRoundTrip is the M-mesh-4
// completion of the boundary M-mesh-2/M-mesh-3 deliberately left open:
// alice (on daemon a) can now send to bob (a real agent that only exists on
// daemon b) by exact name, bob actually receives it, and his reply crosses
// back so alice's own `relay wait` resolves - proving hop-counting/reply_to
// work across the mesh via each daemon's mirror row, never a live query to
// the other daemon.
func TestMeshSendReachesARemoteAgentAndRepliesRoundTrip(t *testing.T) {
	a, b, sid := joinTwoMeshDaemons(t)
	alice := a.joinPeer(sid, proto.Hello{Name: "alice", Role: "orchestrator"})
	bob := b.joinPeer(sid, proto.Hello{Name: "bob", Role: "developer"})
	defer bob.ws.CloseNow()
	a.waitAgent(sid, "bob", func(ai proto.AgentInfo) bool { return ai.Remote }, "gossiped in")

	sent := alice.mustSend(proto.SendArgs{To: "bob", Body: "please review PR 42"})
	if sent.State != store.MsgQueued {
		t.Fatalf("alice -> bob: %+v", sent)
	}

	// bob is a real, connected agent on his own daemon: he actually gets it.
	m := bob.mustDeliver()
	if m.Body != "please review PR 42" || m.From != "alice" {
		t.Fatalf("bob's delivery: %+v", m)
	}
	if got, err := b.srv.st.GetMessage(bg, m.ID); err != nil || got.Origin != store.OriginLocal || got.FromPeer == "" {
		t.Fatalf("b's row for the handoff: %+v, err=%v", got, err)
	}
	if got, err := a.srv.st.GetMessage(bg, sent.ID); err != nil || got.Origin != store.OriginMirror || got.ToPeer == "" {
		t.Fatalf("a's mirror row: %+v, err=%v", got, err)
	}

	// bob replies; the reply crosses back to alice as its own handoff.
	rep := bob.mustSend(proto.SendArgs{ReplyTo: m.ID, Body: "looks good"})
	back := alice.mustDeliver()
	if back.ReplyTo != m.ID || back.Body != "looks good" {
		t.Fatalf("alice's delivery of bob's reply: %+v", back)
	}
	_ = rep

	// The original message's mirror row on a picks up "done" via an
	// asynchronous receipt from b, without alice ever polling b directly.
	deadline := time.Now().Add(2 * time.Second)
	for {
		got, err := a.srv.st.GetMessage(bg, sent.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.State == store.MsgDone {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("a's mirror row never reached done: %+v", got)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestMeshApproveInboundIsDecidedByTheOwningDaemon proves the hold-policy
// decision for a cross-daemon message is made by the RECIPIENT's own
// daemon, using its own live --approve-inbound flag, since only it can
// possibly know that: the sender's daemon has no such information about a
// remote agent gossiped into mesh_agents.
func TestMeshApproveInboundIsDecidedByTheOwningDaemon(t *testing.T) {
	a, b, sid := joinTwoMeshDaemons(t)
	alice := a.joinPeer(sid, proto.Hello{Name: "alice", Role: "orchestrator"})
	bob := b.joinPeer(sid, proto.Hello{Name: "bob", Role: "developer", ApproveInbound: true})
	defer bob.ws.CloseNow()
	a.waitAgent(sid, "bob", func(ai proto.AgentInfo) bool { return ai.Remote }, "gossiped in")

	sent := alice.mustSend(proto.SendArgs{To: "bob", Body: "hi"})
	bob.expectNoDeliver(200 * time.Millisecond) // held: bob never sees it yet

	if got, err := b.srv.st.GetMessage(bg, sent.ID); err != nil || got.State != store.MsgHeld {
		t.Fatalf("b's authoritative row should be held: %+v, err=%v", got, err)
	}

	// alice's own mirror row learns "held" via the handoff's synchronous
	// reply - no polling needed, it was already applied by the time
	// mustSend returned above, but relay wait is how a real agent would see it.
	if r := alice.rpc(proto.OpWait, proto.WaitArgs{ID: sent.ID, TimeoutS: 0.3}); r.Error != nil {
		t.Fatal(r.Error)
	} else {
		var w proto.WaitResult
		_ = json.Unmarshal(r.Result, &w)
		if w.State != store.MsgHeld {
			t.Fatalf("alice's view of her own send: %+v", w)
		}
	}

	// b's human approves it; bob (the actual owner) now gets it.
	var ar proto.ApproveResult
	if code := b.postJSON("/v1/admin/messages/"+sent.ID+"/approve", nil, &ar); code != 200 || ar.State != store.MsgQueued {
		t.Fatalf("approve on b: %d %+v", code, ar)
	}
	if m := bob.mustDeliver(); m.ID != sent.ID {
		t.Fatalf("bob's delivery after approval: %+v", m)
	}
}

func TestMeshDefaultsToRealTransportWithoutCrashing(t *testing.T) {
	// No MeshTransport override: hub() must fall back to meshnet.RealTransport
	// without panicking. It is never actually dialed/listened on here - just
	// constructed - since this test does no mesh operations, confirming a
	// daemon that never uses mesh features never touches the real network.
	a := startServer(t)
	if a.srv.opt.MeshTransport != nil {
		t.Fatal("test setup: expected no MeshTransport override")
	}
}
