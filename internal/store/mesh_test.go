package store

import "testing"

func TestEnsureMeshSessionCreatesOnceAndIsIdempotent(t *testing.T) {
	s := open(t)
	sess := newSession(t, s)

	first, err := s.EnsureMeshSession(bg, sess.ID, "s3cr3t", "peerA")
	if err != nil {
		t.Fatal(err)
	}
	if first.JoinSecret != "s3cr3t" || first.SelfPeerID != "peerA" {
		t.Fatalf("%+v", first)
	}

	// A second Ensure with different values must NOT overwrite the first:
	// this is the "any current member can mint a fresh invite" path, which
	// re-embeds the existing secret rather than generating a new one.
	again, err := s.EnsureMeshSession(bg, sess.ID, "different-secret", "peerB")
	if err != nil {
		t.Fatal(err)
	}
	if again.JoinSecret != "s3cr3t" || again.SelfPeerID != "peerA" {
		t.Fatalf("EnsureMeshSession overwrote an existing row: %+v", again)
	}
}

func TestEnsureMeshSessionCreatesTheLocalSessionRowTooIfMissing(t *testing.T) {
	// This is the joining-daemon path: it has never heard of sessionID
	// before (no CreateSession was ever called locally for it), unlike the
	// hosting daemon which already has the row. mesh_sessions has a foreign
	// key to sessions, so this must succeed on its own.
	s := open(t)
	id := "01ARZ3NDEKTSV4RRFFQ69G5FAV"
	if _, err := s.GetSession(bg, id); err != ErrNotFound {
		t.Fatalf("test setup: expected no local session yet, got %v", err)
	}
	if _, err := s.EnsureMeshSession(bg, id, "secret", "self"); err != nil {
		t.Fatal(err)
	}
	sess, err := s.GetSession(bg, id)
	if err != nil {
		t.Fatalf("EnsureMeshSession did not create the local sessions row: %v", err)
	}
	if sess.Kind != "shared" || sess.Status != "active" {
		t.Fatalf("%+v", sess)
	}
}

func TestGetMeshSessionNotFound(t *testing.T) {
	s := open(t)
	if _, err := s.GetMeshSession(bg, "01ARZ3NDEKTSV4RRFFQ69G5FAV"); err != ErrNotFound {
		t.Fatalf("got %v, want ErrNotFound", err)
	}
}

func TestMeshSessionIDIsCaseNormalized(t *testing.T) {
	s := open(t)
	sess := newSession(t, s)
	if _, err := s.EnsureMeshSession(bg, sess.ID, "secret", "self"); err != nil {
		t.Fatal(err)
	}
	// GetMeshSession must find it however the caller cased the id, exactly
	// like the sessions table's own lookups.
	lower, err := s.GetMeshSession(bg, toLowerASCII(sess.ID))
	if err != nil {
		t.Fatal(err)
	}
	if lower.SessionID != sess.ID {
		t.Fatalf("case-insensitive lookup returned %q, want %q", lower.SessionID, sess.ID)
	}
}

func toLowerASCII(s string) string {
	b := []byte(s)
	for i, c := range b {
		if c >= 'A' && c <= 'Z' {
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
}

func TestUpsertMeshPeerCreatesThenUpdates(t *testing.T) {
	s := open(t)
	sess := newSession(t, s)
	if _, err := s.EnsureMeshSession(bg, sess.ID, "secret", "self"); err != nil {
		t.Fatal(err)
	}

	if err := s.UpsertMeshPeer(bg, MeshPeer{SessionID: sess.ID, PeerID: "peerA", Addr: "tcOLD"}); err != nil {
		t.Fatal(err)
	}
	first, err := s.GetMeshPeer(bg, sess.ID, "peerA")
	if err != nil {
		t.Fatal(err)
	}
	if first.Addr != "tcOLD" || first.Status != MeshPeerLinked {
		t.Fatalf("%+v", first)
	}
	if first.FirstSeen.IsZero() || first.LastSeen.IsZero() {
		t.Fatalf("timestamps not set: %+v", first)
	}

	if err := s.UpsertMeshPeer(bg, MeshPeer{SessionID: sess.ID, PeerID: "peerA", Addr: "tcNEW"}); err != nil {
		t.Fatal(err)
	}
	second, err := s.GetMeshPeer(bg, sess.ID, "peerA")
	if err != nil {
		t.Fatal(err)
	}
	if second.Addr != "tcNEW" {
		t.Fatalf("address did not update: %+v", second)
	}
	if !second.FirstSeen.Equal(first.FirstSeen) {
		t.Fatalf("first_seen must not move on an update: %v vs %v", second.FirstSeen, first.FirstSeen)
	}
}

func TestGetMeshPeerNotFound(t *testing.T) {
	s := open(t)
	sess := newSession(t, s)
	if _, err := s.GetMeshPeer(bg, sess.ID, "nobody"); err != ErrNotFound {
		t.Fatalf("got %v, want ErrNotFound", err)
	}
}

func TestSetMeshPeerStatus(t *testing.T) {
	s := open(t)
	sess := newSession(t, s)
	if _, err := s.EnsureMeshSession(bg, sess.ID, "secret", "self"); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertMeshPeer(bg, MeshPeer{SessionID: sess.ID, PeerID: "peerA", Addr: "tcX"}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetMeshPeerStatus(bg, sess.ID, "peerA", MeshPeerUnreachable); err != nil {
		t.Fatal(err)
	}
	p, err := s.GetMeshPeer(bg, sess.ID, "peerA")
	if err != nil {
		t.Fatal(err)
	}
	if p.Status != MeshPeerUnreachable {
		t.Fatalf("status = %q, want unreachable", p.Status)
	}
	if p.Addr != "tcX" {
		t.Fatalf("SetMeshPeerStatus must not touch addr: got %q", p.Addr)
	}
}

func TestSetMeshPeerStatusNotFound(t *testing.T) {
	s := open(t)
	sess := newSession(t, s)
	if err := s.SetMeshPeerStatus(bg, sess.ID, "nobody", MeshPeerUnreachable); err != ErrNotFound {
		t.Fatalf("got %v, want ErrNotFound", err)
	}
}

func TestListMeshPeersOrdersMostRecentFirst(t *testing.T) {
	s := open(t)
	sess := newSession(t, s)
	if _, err := s.EnsureMeshSession(bg, sess.ID, "secret", "self"); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertMeshPeer(bg, MeshPeer{SessionID: sess.ID, PeerID: "peerA", Addr: "tcA"}); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertMeshPeer(bg, MeshPeer{SessionID: sess.ID, PeerID: "peerB", Addr: "tcB"}); err != nil {
		t.Fatal(err)
	}
	// Touch A again so it becomes the most recently seen.
	if err := s.SetMeshPeerStatus(bg, sess.ID, "peerA", MeshPeerLinked); err != nil {
		t.Fatal(err)
	}
	peers, err := s.ListMeshPeers(bg, sess.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(peers) != 2 || peers[0].PeerID != "peerA" {
		t.Fatalf("%+v", peers)
	}
}

func TestDeleteSessionRemovesMeshRows(t *testing.T) {
	s := open(t)
	sess := newSession(t, s)
	if _, err := s.EnsureMeshSession(bg, sess.ID, "secret", "self"); err != nil {
		t.Fatal(err)
	}
	if err := s.UpsertMeshPeer(bg, MeshPeer{SessionID: sess.ID, PeerID: "peerA", Addr: "tcA"}); err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteSession(bg, sess.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetMeshSession(bg, sess.ID); err != ErrNotFound {
		t.Fatalf("mesh_sessions row survived DeleteSession: err=%v", err)
	}
	if _, err := s.GetMeshPeer(bg, sess.ID, "peerA"); err != ErrNotFound {
		t.Fatalf("mesh_peers row survived DeleteSession: err=%v", err)
	}
}
