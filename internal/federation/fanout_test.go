package federation

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/thesahibnanda-max/relay/internal/meshnet"
	"github.com/thesahibnanda-max/relay/internal/store"
)

// waitForLinkedPeer polls until s has peerID recorded linked for sessionID,
// or fails the test. Fan-out and healing both happen via a Hub's own
// background goroutines (Join's post-welcome Resync, or a caller's own
// Resync call racing other work), so a test can't assume synchronous
// completion.
func waitForLinkedPeer(t *testing.T, s *store.Store, sessionID, peerID string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if p, err := s.GetMeshPeer(context.Background(), sessionID, peerID); err == nil && p.Status == store.MeshPeerLinked {
			return
		}
		if time.Now().After(deadline) {
			p, _ := s.GetMeshPeer(context.Background(), sessionID, peerID)
			t.Fatalf("timed out waiting for peer %s to be linked: %+v", peerID, p)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestJoinFansOutToPeersLearnedFromTheSeed is the core M-mesh-5 property: a
// daemon joining a session through one seed ends up directly linked to
// every OTHER member the seed already knew about too, without the seed's
// continued participation - a true N×N mesh, not hub-and-spoke.
func TestJoinFansOutToPeersLearnedFromTheSeed(t *testing.T) {
	net := meshnet.NewFakeNetwork()
	a := newTestHub(t, net, "a")
	b := newTestHub(t, net, "b")
	c := newTestHub(t, net, "c")
	ctx := withTimeout(t)
	sessionID := "01SESS0000000000000000020"

	blobAB, err := a.Invite(ctx, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Join(ctx, blobAB); err != nil {
		t.Fatal(err)
	}
	waitForPeerCount(t, a.store, sessionID, 1) // a's side of the a<->b handshake

	// c joins through a, the same as b did - a's Welcome to c names b (the
	// only peer a currently knows), initially unreachable to c.
	blobAC, err := a.Invite(ctx, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Join(ctx, blobAC); err != nil {
		t.Fatal(err)
	}

	// c reaches b directly on its own - a is never dialed again for this.
	waitForLinkedPeer(t, c.store, sessionID, b.opt.Identity.PeerID())
	waitForLinkedPeer(t, b.store, sessionID, c.opt.Identity.PeerID())

	// Full mesh: everyone has recorded everyone else linked.
	for _, pair := range []struct {
		s          testHub
		wantPeerID string
	}{
		{a, b.opt.Identity.PeerID()}, {a, c.opt.Identity.PeerID()},
		{b, a.opt.Identity.PeerID()}, {b, c.opt.Identity.PeerID()},
		{c, a.opt.Identity.PeerID()}, {c, b.opt.Identity.PeerID()},
	} {
		waitForLinkedPeer(t, pair.s.store, sessionID, pair.wantPeerID)
	}
}

// TestSeedDisappearingAfterFanOutDoesNotPreventReachingOthers proves the
// plan's walkthrough (c): a join's seed is a one-time bootstrap aid. c
// learns about b from a's one Welcome; a is closed immediately afterward,
// before c's own background fan-out necessarily finishes - c must still
// reach b on its own.
func TestSeedDisappearingAfterFanOutDoesNotPreventReachingOthers(t *testing.T) {
	net := meshnet.NewFakeNetwork()
	a := newTestHub(t, net, "a")
	b := newTestHub(t, net, "b")
	c := newTestHub(t, net, "c")
	ctx := withTimeout(t)
	sessionID := "01SESS0000000000000000022"

	blobAB, err := a.Invite(ctx, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Join(ctx, blobAB); err != nil {
		t.Fatal(err)
	}
	waitForPeerCount(t, a.store, sessionID, 1)

	blobAC, err := a.Invite(ctx, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.Join(ctx, blobAC); err != nil {
		t.Fatal(err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}

	waitForLinkedPeer(t, c.store, sessionID, b.opt.Identity.PeerID())
	waitForLinkedPeer(t, b.store, sessionID, c.opt.Identity.PeerID())
}

// TestResyncHealsAPeerAfterItRestarts simulates a partition heal: b goes
// away (its Hub closes, mirroring a crashed/offline daemon), a's Resync
// correctly marks it unreachable without failing, and once a fresh Hub for
// the exact same identity comes back up (a restarted daemon reopening its
// existing store), a's next Resync heals the peer back to linked.
func TestResyncHealsAPeerAfterItRestarts(t *testing.T) {
	net := meshnet.NewFakeNetwork()
	a := newTestHub(t, net, "a")
	ctx := withTimeout(t)
	sessionID := "01SESS0000000000000000023"

	bIdentity, err := meshnet.LoadOrCreateIdentity(filepath.Join(t.TempDir(), "b.key"))
	if err != nil {
		t.Fatal(err)
	}
	bStore, err := store.Open(filepath.Join(t.TempDir(), "b.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { bStore.Close() })
	newB := func() *Hub {
		return New(Options{Identity: bIdentity, Transport: meshnet.FakeTransport{Net: net}, Store: bStore, Build: "test"})
	}

	b1 := newB()
	blob, err := a.Invite(ctx, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if err := b1.Join(ctx, blob); err != nil {
		t.Fatal(err)
	}
	waitForPeerCount(t, a.store, sessionID, 1)

	if err := b1.Close(); err != nil {
		t.Fatal(err)
	}
	a.Resync(ctx, sessionID)
	if p, err := a.store.GetMeshPeer(ctx, sessionID, bIdentity.PeerID()); err != nil || p.Status != store.MeshPeerUnreachable {
		t.Fatalf("b should be marked unreachable after closing: %+v, err=%v", p, err)
	}

	b2 := newB() // "restart": same identity and store, a fresh Hub value
	defer b2.Close()
	// A restarted daemon's own startup brings its listener back up and
	// resyncs every mesh session it finds in its (persisted) store - see
	// daemon.Server.New's mesh auto-resume, which this simulates directly.
	b2.Resync(ctx, sessionID)
	a.Resync(ctx, sessionID)
	if p, err := a.store.GetMeshPeer(ctx, sessionID, bIdentity.PeerID()); err != nil || p.Status != store.MeshPeerLinked {
		t.Fatalf("b should be healed back to linked: %+v, err=%v", p, err)
	}
}
