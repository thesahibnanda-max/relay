package meshnet

import (
	"context"
	"fmt"
	"net"

	"github.com/tailscale/tailcat"
)

// meshPort is the fixed TCP port every Relay daemon's mesh listener serves
// on, within tailcat's own private per-node virtual address space (not a
// port on the host's real network stack, so it can never collide with
// anything else running on the machine). One protocol, one well-known port:
// there is nothing to discover or negotiate.
const meshPort = 7889

// RealTransport is the production Transport, backed by actual tailcat
// servers/clients (real WireGuard tunnels, real DERP fallback).
type RealTransport struct {
	// DERPMapURL, if set, overrides tailcat's default DERP map (a
	// third-party, free, rate-limited relay at https://tailcat.dev). See
	// docs/SECURITY.md for the disclosure of that default's trade-offs.
	DERPMapURL string

	// Logf receives tailcat's own debug logging. Nil discards it.
	Logf func(format string, args ...any)
}

func (t RealTransport) logf() func(string, ...any) {
	if t.Logf != nil {
		return t.Logf
	}
	return func(string, ...any) {}
}

// Listen brings up a tailcat server using id's persisted key and pre-shared
// key (so its address survives a daemon restart), and returns a listener for
// the fixed mesh port plus the address peers dial to reach it.
func (t RealTransport) Listen(ctx context.Context, id *Identity) (net.Listener, Addr, error) {
	srv := &tailcat.Server{
		Key:          id.NodeKey,
		PresharedKey: id.PSK,
		DERPMapURL:   t.DERPMapURL,
		Logf:         t.logf(),
	}
	ln, err := srv.Listen(ctx, "tcp", fmt.Sprintf(":%d", meshPort))
	if err != nil {
		return nil, "", fmt.Errorf("meshnet: tailcat listen: %w", err)
	}
	return &closeServerListener{Listener: ln, srv: srv}, Addr(srv.TailcatAddr()), nil
}

// closeServerListener closes the underlying tailcat.Server (tearing down its
// WireGuard engine and DERP connection) when the listener is closed, since
// tailcat.Server.Listen does not itself take ownership of that lifetime.
type closeServerListener struct {
	net.Listener
	srv *tailcat.Server
}

func (l *closeServerListener) Close() error {
	lerr := l.Listener.Close()
	serr := l.srv.Close()
	if lerr != nil {
		return lerr
	}
	return serr
}

// Dial opens one stream to peer over a tailcat client using id's node key as
// its own identity (the same key used for Listen, so a daemon presents one
// consistent PeerID whether it's dialing out or being dialed into).
func (t RealTransport) Dial(ctx context.Context, id *Identity, peer Addr) (net.Conn, error) {
	cl := &tailcat.Client{
		Server:     tailcat.Addr(peer),
		Key:        id.NodeKey,
		DERPMapURL: t.DERPMapURL,
		Logf:       t.logf(),
	}
	c, err := cl.DialTCPPort(ctx, meshPort)
	if err != nil {
		cl.Close()
		return nil, fmt.Errorf("meshnet: tailcat dial %s: %w", peer, err)
	}
	return &closeClientConn{Conn: c, cl: cl}, nil
}

// closeClientConn closes the underlying tailcat.Client alongside the
// connection it dialed, for the same reason closeServerListener exists.
type closeClientConn struct {
	net.Conn
	cl *tailcat.Client
}

func (c *closeClientConn) Close() error {
	cerr := c.Conn.Close()
	lerr := c.cl.Close()
	if cerr != nil {
		return cerr
	}
	return lerr
}
