package meshnet

import (
	"context"
	"fmt"
	"net"
	"strings"
	"sync"
)

// FakeNetwork is an in-memory Transport for tests: no tailcat, no DERP, no
// real network I/O. Multiple FakeTransport handles sharing one FakeNetwork
// simulate multiple daemons on one mesh; closing and re-listening with the
// same Identity simulates a daemon restart.
type FakeNetwork struct {
	mu        sync.Mutex
	listeners map[string]*fakeListener // keyed by PeerID
}

// NewFakeNetwork returns an empty shared network for a test to hand to
// several FakeTransport values.
func NewFakeNetwork() *FakeNetwork {
	return &FakeNetwork{listeners: map[string]*fakeListener{}}
}

// FakeTransport implements Transport against a shared FakeNetwork.
type FakeTransport struct {
	Net *FakeNetwork
}

func (t FakeTransport) Listen(ctx context.Context, id *Identity) (Listener, Addr, error) {
	n := t.Net
	n.mu.Lock()
	defer n.mu.Unlock()
	peerID := id.PeerID()
	if _, exists := n.listeners[peerID]; exists {
		return nil, "", fmt.Errorf("meshnet: fake network already has a listener for %s", peerID)
	}
	ln := &fakeListener{net: n, peerID: peerID, incoming: make(chan pendingConn), closed: make(chan struct{})}
	n.listeners[peerID] = ln
	return ln, Addr("fake:" + peerID), nil
}

// Dial connects to peer as id. Because a fake network's ends are ordinary
// net.Pipe conns (no real cryptography), the dialer's verified identity is
// handed to the accept side out of band, as a plain value alongside the
// conn, rather than encoded onto the wire the way RealTransport derives it
// from the actual tunnel - the two transports differ here in mechanism
// only, not in the guarantee callers see: Listener.Accept's returned
// identity always reflects who actually dialed, never a self-reported claim
// on the wire.
func (t FakeTransport) Dial(ctx context.Context, id *Identity, peer Addr) (net.Conn, error) {
	peerID, ok := strings.CutPrefix(string(peer), "fake:")
	if !ok {
		return nil, fmt.Errorf("meshnet: %q is not a fake address", peer)
	}
	n := t.Net
	n.mu.Lock()
	ln, ok := n.listeners[peerID]
	n.mu.Unlock()
	if !ok {
		return nil, fmt.Errorf("meshnet: no listener for %s", peer)
	}
	a, b := net.Pipe()
	select {
	case ln.incoming <- pendingConn{conn: a, peerID: id.PeerID()}:
		return b, nil
	case <-ln.closed:
		a.Close()
		b.Close()
		return nil, fmt.Errorf("meshnet: listener for %s is closed", peer)
	case <-ctx.Done():
		a.Close()
		b.Close()
		return nil, ctx.Err()
	}
}

type pendingConn struct {
	conn   net.Conn
	peerID string
}

type fakeListener struct {
	net      *FakeNetwork
	peerID   string
	incoming chan pendingConn
	once     sync.Once
	closed   chan struct{}
}

func (l *fakeListener) Accept() (net.Conn, string, error) {
	select {
	case pc := <-l.incoming:
		return pc.conn, pc.peerID, nil
	case <-l.closed:
		return nil, "", fmt.Errorf("meshnet: listener closed")
	}
}

func (l *fakeListener) Close() error {
	l.once.Do(func() {
		l.net.mu.Lock()
		if l.net.listeners[l.peerID] == l {
			delete(l.net.listeners, l.peerID)
		}
		l.net.mu.Unlock()
		close(l.closed)
	})
	return nil
}
