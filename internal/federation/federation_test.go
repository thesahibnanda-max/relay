package federation

import (
	"context"
	"encoding/json"
	"net"
	"path/filepath"
	"testing"
	"time"

	"github.com/thesahibnanda-max/relay/internal/ids"
	"github.com/thesahibnanda-max/relay/internal/meshnet"
	"github.com/thesahibnanda-max/relay/internal/proto"
	"github.com/thesahibnanda-max/relay/internal/store"
)

var bg = context.Background()

// testHub bundles a Hub with the store backing it, so tests can assert on
// what got recorded without going through the network at all.
type testHub struct {
	*Hub
	store *store.Store
}

func newTestHub(t *testing.T, net *meshnet.FakeNetwork, name string) testHub {
	t.Helper()
	st, err := store.Open(filepath.Join(t.TempDir(), name+".db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	id, err := meshnet.LoadOrCreateIdentity(filepath.Join(t.TempDir(), name+".key"))
	if err != nil {
		t.Fatal(err)
	}
	h := New(Options{
		Identity:  id,
		Transport: meshnet.FakeTransport{Net: net},
		Store:     st,
		Build:     "test",
	})
	t.Cleanup(func() { h.Close() })
	return testHub{Hub: h, store: st}
}

func withTimeout(t *testing.T) context.Context {
	t.Helper()
	ctx, cancel := context.WithTimeout(bg, 5*time.Second)
	t.Cleanup(cancel)
	return ctx
}

func TestJoinRecordsBothPeersAsLinked(t *testing.T) {
	net := meshnet.NewFakeNetwork()
	a := newTestHub(t, net, "a")
	b := newTestHub(t, net, "b")
	ctx := withTimeout(t)

	sessionID := ids.New()
	blob, err := a.Invite(ctx, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if blob.Session != sessionID || blob.Secret == "" || blob.PeerAddr == "" {
		t.Fatalf("%+v", blob)
	}

	if err := b.Join(ctx, blob); err != nil {
		t.Fatal(err)
	}

	bPeers, err := b.store.ListMeshPeers(ctx, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(bPeers) != 1 || bPeers[0].PeerID != a.opt.Identity.PeerID() || bPeers[0].Status != store.MeshPeerLinked {
		t.Fatalf("b's view of its peers: %+v", bPeers)
	}

	// a's handleInbound runs in a goroutine; give it a moment to record b.
	var aPeers []store.MeshPeer
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		aPeers, _ = a.store.ListMeshPeers(ctx, sessionID)
		if len(aPeers) == 1 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(aPeers) != 1 || aPeers[0].PeerID != b.opt.Identity.PeerID() || aPeers[0].Status != store.MeshPeerLinked {
		t.Fatalf("a's view of its peers: %+v", aPeers)
	}

	// Both sides must also now have a local `sessions` row for it (b never
	// had one before joining).
	if _, err := b.store.GetSession(ctx, sessionID); err != nil {
		t.Fatalf("b has no local session row: %v", err)
	}
}

func TestJoinWithWrongSecretIsRejected(t *testing.T) {
	net := meshnet.NewFakeNetwork()
	a := newTestHub(t, net, "a")
	b := newTestHub(t, net, "b")
	ctx := withTimeout(t)

	blob, err := a.Invite(ctx, ids.New())
	if err != nil {
		t.Fatal(err)
	}
	blob.Secret = "not-the-real-secret"
	if err := b.Join(ctx, blob); err == nil {
		t.Fatal("expected an error joining with the wrong secret")
	}
	peers, _ := b.store.ListMeshPeers(ctx, blob.Session)
	if len(peers) != 0 {
		t.Fatalf("a rejected peer should not have been recorded: %+v", peers)
	}
}

func TestJoinAgainstUnknownSessionIsRejected(t *testing.T) {
	net := meshnet.NewFakeNetwork()
	a := newTestHub(t, net, "a")
	b := newTestHub(t, net, "b")
	ctx := withTimeout(t)

	// a has never Invited or otherwise created this session.
	addr, err := a.ensureListening(ctx)
	if err != nil {
		t.Fatal(err)
	}
	blob := proto.JoinBlob{Session: ids.New(), PeerAddr: string(addr), Secret: "whatever"}
	if err := b.Join(ctx, blob); err == nil {
		t.Fatal("expected an error joining a session the target daemon doesn't know about")
	}
}

func TestJoinLearnsAboutOtherPeersFromTheWelcome(t *testing.T) {
	net := meshnet.NewFakeNetwork()
	a := newTestHub(t, net, "a")
	b := newTestHub(t, net, "b")
	c := newTestHub(t, net, "c")
	ctx := withTimeout(t)

	sessionID := ids.New()
	blobFromA, err := a.Invite(ctx, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if err := b.Join(ctx, blobFromA); err != nil {
		t.Fatal(err)
	}

	// b, already a member, mints a fresh invite for the same session - the
	// "any current member can onboard someone new" property.
	blobFromB, err := b.Invite(ctx, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if blobFromB.Secret != blobFromA.Secret {
		t.Fatal("b minted a different secret than the one it joined with")
	}
	if err := c.Join(ctx, blobFromB); err != nil {
		t.Fatal(err)
	}

	cPeers, err := c.store.ListMeshPeers(ctx, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(cPeers) != 2 {
		t.Fatalf("c should know about both a and b: %+v", cPeers)
	}
	var sawA, sawALinked bool
	for _, p := range cPeers {
		if p.PeerID == a.opt.Identity.PeerID() {
			sawA = true
			sawALinked = p.Status == store.MeshPeerLinked
		}
	}
	if !sawA {
		t.Fatalf("c never learned about a via b's welcome: %+v", cPeers)
	}
	if sawALinked {
		t.Fatal("c has not dialed a directly yet (that's M-mesh-5 fan-out); a must not be marked linked")
	}
}

func TestInviteIsIdempotent(t *testing.T) {
	net := meshnet.NewFakeNetwork()
	a := newTestHub(t, net, "a")
	ctx := withTimeout(t)

	sessionID := ids.New()
	first, err := a.Invite(ctx, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	second, err := a.Invite(ctx, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if first != second {
		t.Fatalf("Invite was not idempotent: %+v vs %+v", first, second)
	}
}

func TestCloseStopsAcceptingNewLinks(t *testing.T) {
	net := meshnet.NewFakeNetwork()
	a := newTestHub(t, net, "a")
	b := newTestHub(t, net, "b")
	ctx := withTimeout(t)

	blob, err := a.Invite(ctx, ids.New())
	if err != nil {
		t.Fatal(err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if err := b.Join(ctx, blob); err == nil {
		t.Fatal("expected an error joining a closed listener")
	}
}

// --- raw handshake tests: things an honest Join() can never produce, since
// it always tells the truth about its own identity and speaks the current
// MeshVersion. These exercise the acceptor's own defenses directly, by
// speaking the wire protocol by hand.

func TestVersionMismatchRefusesOnlyThatLink(t *testing.T) {
	net := meshnet.NewFakeNetwork()
	a := newTestHub(t, net, "a")
	attacker, err := meshnet.LoadOrCreateIdentity(filepath.Join(t.TempDir(), "attacker.key"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := withTimeout(t)

	sessionID := ids.New()
	blob, err := a.Invite(ctx, sessionID)
	if err != nil {
		t.Fatal(err)
	}

	conn, err := (meshnet.FakeTransport{Net: net}).Dial(ctx, attacker, meshnet.Addr(blob.PeerAddr))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	hello := proto.MeshHello{MeshVersion: proto.MeshVersion + 1, Session: blob.Session, Secret: blob.Secret, PeerID: attacker.PeerID()}
	b, _ := proto.MeshMarshal(proto.MeshTypeHello, hello)
	if err := writeFrame(conn, b); err != nil {
		t.Fatal(err)
	}
	env := readErrorReply(t, conn)
	if env.Code != proto.MeshCodeVersionMismatch {
		t.Fatalf("code = %q, want %q", env.Code, proto.MeshCodeVersionMismatch)
	}

	// a's own local session and its listener must be completely unaffected.
	if _, err := a.store.GetSession(ctx, sessionID); err != nil {
		t.Fatalf("a's local session was affected: %v", err)
	}
}

func TestClaimedIdentityMismatchIsRejected(t *testing.T) {
	net := meshnet.NewFakeNetwork()
	a := newTestHub(t, net, "a")
	attacker, err := meshnet.LoadOrCreateIdentity(filepath.Join(t.TempDir(), "attacker.key"))
	if err != nil {
		t.Fatal(err)
	}
	victim, err := meshnet.LoadOrCreateIdentity(filepath.Join(t.TempDir(), "victim.key"))
	if err != nil {
		t.Fatal(err)
	}
	ctx := withTimeout(t)

	sessionID := ids.New()
	blob, err := a.Invite(ctx, sessionID)
	if err != nil {
		t.Fatal(err)
	}

	// attacker dials (so the transport verifies IT as the peer) but the
	// Hello payload claims to be victim - a lie no honest Join() would ever
	// tell, since Join always fills PeerID from its own dialing identity.
	conn, err := (meshnet.FakeTransport{Net: net}).Dial(ctx, attacker, meshnet.Addr(blob.PeerAddr))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	hello := proto.MeshHello{MeshVersion: proto.MeshVersion, Session: blob.Session, Secret: blob.Secret, PeerID: victim.PeerID()}
	b, _ := proto.MeshMarshal(proto.MeshTypeHello, hello)
	if err := writeFrame(conn, b); err != nil {
		t.Fatal(err)
	}
	env := readErrorReply(t, conn)
	if env.Code != proto.MeshCodeKeyChanged {
		t.Fatalf("code = %q, want %q", env.Code, proto.MeshCodeKeyChanged)
	}

	for _, claimed := range []string{attacker.PeerID(), victim.PeerID()} {
		if _, err := a.store.GetMeshPeer(ctx, sessionID, claimed); err != store.ErrNotFound {
			t.Fatalf("peer %q must not have been recorded from a rejected hello: err=%v", claimed, err)
		}
	}
}

func readErrorReply(t *testing.T, conn net.Conn) struct{ Code, Message string } {
	t.Helper()
	frame, err := readFrame(conn)
	if err != nil {
		t.Fatal(err)
	}
	env, err := proto.MeshUnmarshal(frame)
	if err != nil {
		t.Fatal(err)
	}
	if env.Type != proto.MeshTypeError {
		t.Fatalf("expected a %s reply, got type %q", proto.MeshTypeError, env.Type)
	}
	var e struct{ Code, Message string }
	if err := json.Unmarshal(env.Payload, &e); err != nil {
		t.Fatal(err)
	}
	return e
}
