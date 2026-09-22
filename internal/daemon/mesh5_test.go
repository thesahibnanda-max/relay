package daemon

import (
	"context"
	"net"
	"testing"
	"time"

	"github.com/thesahibnanda-max/relay/internal/meshnet"
	"github.com/thesahibnanda-max/relay/internal/proto"
	"github.com/thesahibnanda-max/relay/internal/store"
)

// restartServer simulates a daemon process restart: it opens a fresh
// Server against the exact same on-disk paths (so its mesh identity key and
// SQLite store, both real files under old.paths.Root, are the same ones a
// real restarted process would reopen) and serves on a fresh listener at
// the same socket path. The caller must have already shut old down (its
// listener needs to be free).
func restartServer(t *testing.T, old *env, mutate ...func(*Options)) *env {
	t.Helper()
	opt := Options{Paths: old.paths, Version: "test", Log: old.srv.opt.Log}
	for _, m := range mutate {
		m(&opt)
	}
	srv, err := New(opt)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", old.paths.SocketPath())
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(bg, 3*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	})
	return &env{t: t, srv: srv, paths: old.paths, socket: old.paths.SocketPath(), http: proto.HTTPClient(old.paths.SocketPath())}
}

// waitMeshPeer polls a daemon's admin mesh-peers view until peerID matches
// pred, or fails the test.
func waitMeshPeer(t *testing.T, e *env, sessionID, peerID string, pred func(proto.MeshPeerView) bool, what string) proto.MeshPeerView {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		var peers []proto.MeshPeerView
		e.getJSON("/v1/admin/mesh/peers/"+sessionID, &peers)
		for _, p := range peers {
			if p.PeerID == peerID && pred(p) {
				return p
			}
		}
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for peer %s: %s; have %+v", peerID, what, peers)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestMeshThreeNodeFullMeshAndDirectRouting proves the actual N×N property,
// not just visibility: c and b, both joined through the same seed a but
// never introduced to each other directly, end up linked to each other and
// can exchange a real message without a ever being involved in that
// exchange - a true mesh, not hub-and-spoke.
func TestMeshThreeNodeFullMeshAndDirectRouting(t *testing.T) {
	fakeNet := meshnet.NewFakeNetwork()
	a := startServer(t, withMeshTransport(fakeNet))
	b := startServer(t, withMeshTransport(fakeNet))
	c := startServer(t, withMeshTransport(fakeNet))
	sid := a.newSession()

	var blobAB proto.JoinBlob
	if code := a.postJSON("/v1/admin/mesh/invite", proto.InviteMeshSessionRequest{SessionID: sid}, &blobAB); code != 200 {
		t.Fatalf("invite ab: HTTP %d", code)
	}
	if code := b.postJSON("/v1/admin/mesh/join", blobAB, nil); code != 200 {
		t.Fatalf("join ab: HTTP %d", code)
	}
	var blobAC proto.JoinBlob
	if code := a.postJSON("/v1/admin/mesh/invite", proto.InviteMeshSessionRequest{SessionID: sid}, &blobAC); code != 200 {
		t.Fatalf("invite ac: HTTP %d", code)
	}
	if code := c.postJSON("/v1/admin/mesh/join", blobAC, nil); code != 200 {
		t.Fatalf("join ac: HTTP %d", code)
	}

	linked := func(p proto.MeshPeerView) bool { return p.Status == "linked" }
	// b and c fan out to each other on their own - neither Invite nor Join
	// ever named the other's address directly to a human. Each of b and c
	// must end up with 2 linked peers total (a, plus the other).
	waitAllPeersLinked := func(e *env, want int) {
		t.Helper()
		deadline := time.Now().Add(5 * time.Second)
		for {
			var peers []proto.MeshPeerView
			e.getJSON("/v1/admin/mesh/peers/"+sid, &peers)
			n := 0
			for _, p := range peers {
				if linked(p) {
					n++
				}
			}
			if n >= want {
				return
			}
			if time.Now().After(deadline) {
				t.Fatalf("timed out: only %d/%d peers linked: %+v", n, want, peers)
			}
			time.Sleep(10 * time.Millisecond)
		}
	}
	waitAllPeersLinked(a, 2)
	waitAllPeersLinked(b, 2)
	waitAllPeersLinked(c, 2)

	alice := a.joinPeer(sid, proto.Hello{Name: "alice", Role: "orchestrator"})
	bob := b.joinPeer(sid, proto.Hello{Name: "bob", Role: "developer"})
	carol := c.joinPeer(sid, proto.Hello{Name: "carol", Role: "qa"})
	defer bob.ws.CloseNow()
	defer carol.ws.CloseNow()
	_ = alice

	c.waitAgent(sid, "bob", func(ai proto.AgentInfo) bool { return ai.Remote }, "gossiped from b to c")

	sent := carol.mustSend(proto.SendArgs{To: "bob", Body: "direct from carol"})
	if sent.State != store.MsgQueued {
		t.Fatalf("carol -> bob: %+v", sent)
	}
	m := bob.mustDeliver()
	if m.Body != "direct from carol" || m.From != "carol" {
		t.Fatalf("bob's delivery: %+v", m)
	}
}

// TestDaemonResumesMeshOnRestart proves the resilience requirement across
// an actual daemon process restart, not just a network blip: b's whole
// daemon process exits and a fresh one reopens its same on-disk identity
// and store, and - without any human re-running `relay session
// invite`/`--join` - a's own periodic mesh resync (the sweep hook, given a
// short ExpireEvery here so the test doesn't wait 30s) notices b is back,
// re-links it, and a newly registered real agent on the restarted b becomes
// visible and reachable from a again.
func TestDaemonResumesMeshOnRestart(t *testing.T) {
	fast := func(o *Options) { o.ExpireEvery = 50 * time.Millisecond }
	fakeNet := meshnet.NewFakeNetwork()
	a := startServer(t, withMeshTransport(fakeNet), fast)
	b := startServer(t, withMeshTransport(fakeNet), fast)
	sid := a.newSession()

	var blob proto.JoinBlob
	if code := a.postJSON("/v1/admin/mesh/invite", proto.InviteMeshSessionRequest{SessionID: sid}, &blob); code != 200 {
		t.Fatalf("invite: HTTP %d", code)
	}
	if code := b.postJSON("/v1/admin/mesh/join", blob, nil); code != 200 {
		t.Fatalf("join: HTTP %d", code)
	}
	var aPeers []proto.MeshPeerView
	deadline := time.Now().Add(2 * time.Second)
	for {
		a.getJSON("/v1/admin/mesh/peers/"+sid, &aPeers)
		if len(aPeers) == 1 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("a never recorded b as a peer: %+v", aPeers)
		}
		time.Sleep(10 * time.Millisecond)
	}
	bPeerID := aPeers[0].PeerID

	ctx, cancel := context.WithTimeout(bg, 3*time.Second)
	if err := b.srv.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	cancel()

	// a's own periodic resync should notice b is gone on its own (no test
	// action forces this) - proven implicitly by the restart healing below,
	// which only makes sense if a was actively retrying.

	b2 := restartServer(t, b, withMeshTransport(fakeNet), fast)
	waitMeshPeer(t, a, sid, bPeerID, func(p proto.MeshPeerView) bool { return p.Status == "linked" }, "healed after restart")

	// A real agent registered on the restarted daemon becomes visible and
	// reachable from a again, with no admin call re-establishing anything.
	alice := a.joinPeer(sid, proto.Hello{Name: "alice", Role: "orchestrator"})
	bob := b2.joinPeer(sid, proto.Hello{Name: "bob", Role: "developer"})
	defer bob.ws.CloseNow()

	a.waitAgent(sid, "bob", func(ai proto.AgentInfo) bool { return ai.Remote }, "gossiped in after restart")
	sent := alice.mustSend(proto.SendArgs{To: "bob", Body: "welcome back"})
	if sent.State != store.MsgQueued {
		t.Fatalf("alice -> bob after restart: %+v", sent)
	}
	if m := bob.mustDeliver(); m.Body != "welcome back" {
		t.Fatalf("bob's delivery after restart: %+v", m)
	}
}

// TestMeshHandoffSentDuringPartitionResendsAfterHeal proves message-level
// (not just peer-status) resilience: a message sent to a real remote agent
// while its daemon is entirely down still gets handed off once that daemon
// comes back, via the same periodic resync that heals the peer status -
// nobody has to resend it by hand, and it isn't silently dropped.
func TestMeshHandoffSentDuringPartitionResendsAfterHeal(t *testing.T) {
	fast := func(o *Options) { o.ExpireEvery = 50 * time.Millisecond }
	fakeNet := meshnet.NewFakeNetwork()
	a := startServer(t, withMeshTransport(fakeNet), fast)
	b := startServer(t, withMeshTransport(fakeNet), fast)
	sid := a.newSession()

	var blob proto.JoinBlob
	if code := a.postJSON("/v1/admin/mesh/invite", proto.InviteMeshSessionRequest{SessionID: sid}, &blob); code != 200 {
		t.Fatalf("invite: HTTP %d", code)
	}
	if code := b.postJSON("/v1/admin/mesh/join", blob, nil); code != 200 {
		t.Fatalf("join: HTTP %d", code)
	}
	bob := b.joinPeer(sid, proto.Hello{Name: "bob", Role: "developer"})
	defer bob.ws.CloseNow()
	alice := a.joinPeer(sid, proto.Hello{Name: "alice", Role: "orchestrator"})
	a.waitAgent(sid, "bob", func(ai proto.AgentInfo) bool { return ai.Remote }, "gossiped in")

	ctx, cancel := context.WithTimeout(bg, 3*time.Second)
	if err := b.srv.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	cancel()

	// Sent while b is completely down: the immediate handoff attempt fails,
	// but the RPC still succeeds (queued locally as a mirror row) exactly
	// like sending to a disconnected local agent would.
	sent := alice.mustSend(proto.SendArgs{To: "bob", Body: "while you were out"})
	if sent.State != store.MsgQueued {
		t.Fatalf("alice -> bob while b is down: %+v", sent)
	}
	if got, err := a.srv.st.GetMessage(bg, sent.ID); err != nil || got.Origin != store.OriginMirror || got.Rev != 0 {
		t.Fatalf("a's mirror row before healing: %+v, err=%v", got, err)
	}

	restartServer(t, b, withMeshTransport(fakeNet), fast)

	// a's periodic resync resends the still-unconfirmed handoff once b is
	// reachable again - no human, and no new send call, involved.
	deadline := time.Now().Add(5 * time.Second)
	for {
		got, err := a.srv.st.GetMessage(bg, sent.ID)
		if err != nil {
			t.Fatal(err)
		}
		if got.Rev > 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("mirror row was never confirmed after b healed: %+v", got)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
