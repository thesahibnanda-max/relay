package meshnet

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/tailscale/tailcat"
)

func TestLoadOrCreateIdentityPersistsAcrossReloads(t *testing.T) {
	path := filepath.Join(t.TempDir(), "identity.key")

	first, err := LoadOrCreateIdentity(path)
	if err != nil {
		t.Fatal(err)
	}
	if first.NodeKey.IsZero() {
		t.Fatal("fresh identity has a zero node key")
	}
	if first.PSK.IsZero() {
		t.Fatal("fresh identity has a zero pre-shared key")
	}

	second, err := LoadOrCreateIdentity(path)
	if err != nil {
		t.Fatal(err)
	}
	if second.PeerID() != first.PeerID() {
		t.Fatalf("reload produced a different identity: %s vs %s", first.PeerID(), second.PeerID())
	}
	if second.PSK != first.PSK {
		t.Fatal("reload produced a different pre-shared key")
	}
}

func TestLoadOrCreateIdentityGivesEachPathItsOwnIdentity(t *testing.T) {
	a, err := LoadOrCreateIdentity(filepath.Join(t.TempDir(), "a.key"))
	if err != nil {
		t.Fatal(err)
	}
	b, err := LoadOrCreateIdentity(filepath.Join(t.TempDir(), "b.key"))
	if err != nil {
		t.Fatal(err)
	}
	if a.PeerID() == b.PeerID() {
		t.Fatal("two fresh identities collided")
	}
}

func TestLoadOrCreateIdentityRejectsACorruptFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "identity.key")
	if err := os.WriteFile(path, []byte("not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadOrCreateIdentity(path); err == nil {
		t.Fatal("expected an error for a corrupt identity file")
	}
}

func TestIdentityFileIsPrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sub", "identity.key")
	if _, err := LoadOrCreateIdentity(path); err != nil {
		t.Fatal(err)
	}
	st, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if perm := st.Mode().Perm(); perm != 0o600 {
		t.Errorf("identity file mode = %o, want 0600", perm)
	}
	dst, err := os.Stat(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if perm := dst.Mode().Perm(); perm != 0o700 {
		t.Errorf("identity dir mode = %o, want 0700", perm)
	}
}

func TestTwoNodesExchangeBytesOverTheFakeTransport(t *testing.T) {
	network := NewFakeNetwork()
	transport := FakeTransport{Net: network}

	serverID, err := LoadOrCreateIdentity(filepath.Join(t.TempDir(), "server.key"))
	if err != nil {
		t.Fatal(err)
	}
	clientID, err := LoadOrCreateIdentity(filepath.Join(t.TempDir(), "client.key"))
	if err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()

	ln, addr, err := transport.Listen(ctx, serverID)
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()
	if addr == "" {
		t.Fatal("Listen returned an empty address")
	}

	accepted := make(chan error, 1)
	go func() {
		c, verifiedPeerID, err := ln.Accept()
		if err != nil {
			accepted <- err
			return
		}
		defer c.Close()
		if verifiedPeerID != clientID.PeerID() {
			accepted <- fmt.Errorf("Accept reported peer %q, want the dialer's real identity %q", verifiedPeerID, clientID.PeerID())
			return
		}
		line, err := bufio.NewReader(c).ReadString('\n')
		if err != nil {
			accepted <- err
			return
		}
		if line != "hello mesh\n" {
			accepted <- fmt.Errorf("unexpected line: %q", line)
			return
		}
		_, err = io.WriteString(c, "hi back\n")
		accepted <- err
	}()

	conn, err := transport.Dial(ctx, clientID, addr)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	if _, err := io.WriteString(conn, "hello mesh\n"); err != nil {
		t.Fatal(err)
	}
	reply, err := bufio.NewReader(conn).ReadString('\n')
	if err != nil {
		t.Fatal(err)
	}
	if reply != "hi back\n" {
		t.Fatalf("unexpected reply: %q", reply)
	}
	if err := <-accepted; err != nil {
		t.Fatal(err)
	}
}

func TestDialingAnUnlistenedAddressFails(t *testing.T) {
	transport := FakeTransport{Net: NewFakeNetwork()}
	clientID, err := LoadOrCreateIdentity(filepath.Join(t.TempDir(), "client.key"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := transport.Dial(context.Background(), clientID, Addr("fake:nobody-here")); err == nil {
		t.Fatal("expected an error dialing an address nobody is listening on")
	}
}

func TestClosingAListenerLetsTheSameIdentityListenAgain(t *testing.T) {
	network := NewFakeNetwork()
	transport := FakeTransport{Net: network}
	id, err := LoadOrCreateIdentity(filepath.Join(t.TempDir(), "server.key"))
	if err != nil {
		t.Fatal(err)
	}
	ln1, _, err := transport.Listen(context.Background(), id)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := transport.Listen(context.Background(), id); err == nil {
		t.Fatal("expected a second Listen with the same identity to fail while the first is still open")
	}
	if err := ln1.Close(); err != nil {
		t.Fatal(err)
	}
	ln2, _, err := transport.Listen(context.Background(), id)
	if err != nil {
		t.Fatalf("re-listening after Close (simulating a daemon restart) should succeed: %v", err)
	}
	ln2.Close()
}

func TestPeerIDIsStableAndBoundToTheNodeKeyNotThePresharedKey(t *testing.T) {
	id, err := LoadOrCreateIdentity(filepath.Join(t.TempDir(), "id.key"))
	if err != nil {
		t.Fatal(err)
	}
	first := id.PeerID()
	// Same node key, different PSK: PeerID must not change (it's derived
	// from the node key alone, per the TOFU-pinning design).
	id.PSK = tailcat.PresharedKey{}
	if id.PeerID() != first {
		t.Fatal("PeerID changed when only the pre-shared key changed")
	}
}
