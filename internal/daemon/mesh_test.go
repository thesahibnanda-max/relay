package daemon

import (
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

// TestJoinedSessionGossipsVisibilityButNotMessageRouting is the M-mesh-3
// update of the M-mesh-2 boundary test: a real agent on each daemon is now
// gossiped to the other (visible via the admin session endpoint, i.e. what
// `relay ls` shows), but `relay_send` still can't reach a name that only
// exists on the *other* daemon - mesh_agents is a read-only cache for
// display, routeSend still only ever resolves against the local agents
// table. Cross-daemon message hand-off is M-mesh-4, not this one.
func TestJoinedSessionGossipsVisibilityButNotMessageRouting(t *testing.T) {
	fakeNet := meshnet.NewFakeNetwork()
	a := startServer(t, withMeshTransport(fakeNet))
	b := startServer(t, withMeshTransport(fakeNet))
	sid := a.newSession()

	var blob proto.JoinBlob
	if code := a.postJSON("/v1/admin/mesh/invite", proto.InviteMeshSessionRequest{SessionID: sid}, &blob); code != 200 {
		t.Fatalf("invite: HTTP %d", code)
	}
	if code := b.postJSON("/v1/admin/mesh/join", blob, nil); code != 200 {
		t.Fatalf("join: HTTP %d", code)
	}

	alice := a.joinPeer(sid, proto.Hello{Name: "alice", Role: "orchestrator"})
	bob := b.joinPeer(sid, proto.Hello{Name: "bob", Role: "developer"})
	defer bob.ws.CloseNow()

	// a's own admin view (what `relay ls` on a's machine shows) must now
	// include bob, marked remote, once gossip has caught up.
	waitForAgent(t, a, sid, "bob", func(ai proto.AgentInfo) bool { return ai.Remote })
	view := a.session(sid)
	var bobInA *proto.AgentInfo
	for i, ai := range view.Agents {
		if ai.Name == "bob" {
			bobInA = &view.Agents[i]
		}
	}
	if bobInA == nil {
		t.Fatalf("bob not in a's session view: %+v", view.Agents)
	}
	if !bobInA.Remote || bobInA.Peer == "" {
		t.Fatalf("bob should be marked remote with a peer id: %+v", bobInA)
	}
	if bobInA.Status != "connected" || !bobInA.Connected {
		t.Fatalf("bob should show as connected: %+v", bobInA)
	}
	// a's own local agent must never be marked remote.
	for _, ai := range view.Agents {
		if ai.Name == "alice" && ai.Remote {
			t.Fatalf("alice must not be marked remote on her own daemon: %+v", ai)
		}
	}

	// alice still can't reach bob by sending to him: bob is visible now, but
	// mesh_agents is a display-only cache - routeSend still only resolves
	// against a's own local agents table. This is the expected, documented
	// M-mesh-3 boundary, not a bug.
	if r := alice.rpc(proto.OpSend, proto.SendArgs{To: "bob", Body: "hi"}); r.Error == nil || r.Error.Code != proto.CodeUnknownAgent {
		t.Fatalf("alice -> bob: %+v, want %s", r.Error, proto.CodeUnknownAgent)
	}

	// Both sessions remain fully independently functional locally: alice can
	// still send to a real local peer on the same daemon.
	carol := a.joinPeer(sid, proto.Hello{Name: "carol", Role: "qa"})
	defer carol.ws.CloseNow()
	if r := alice.mustSend(proto.SendArgs{To: "carol", Body: "hi"}); r.State != store.MsgQueued {
		t.Fatalf("alice -> carol (same daemon): %+v", r)
	}
}

// waitForAgent polls a session's admin view until an agent named `name`
// satisfies pred, or fails the test - gossip lands via a background
// goroutine (handleInbound/Resync), so it can't be assumed synchronous with
// whatever triggered it.
func waitForAgent(t *testing.T, e *env, sessionID, name string, pred func(proto.AgentInfo) bool) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for {
		view := e.session(sessionID)
		for _, ai := range view.Agents {
			if ai.Name == name && pred(ai) {
				return
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for agent %q: %+v", name, view.Agents)
		}
		time.Sleep(10 * time.Millisecond)
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
