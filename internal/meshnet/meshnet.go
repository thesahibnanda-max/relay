// Package meshnet is the only package that imports github.com/tailscale/tailcat.
// tailcat's own README warns its "Go API, the CLI flags and output, and the
// wire format may all change... best effort, without SLAs" — isolating it
// here means a breaking upstream change is a one-package fix, and nothing
// else in Relay ever sees a tailcat type directly.
//
// A daemon that never calls Listen or Dial never touches the network or
// generates a key: mesh capability is completely inert until used.
package meshnet

import (
	"context"
	"net"
)

// Addr is Relay's copy of tailcat's compact, URL-safe reachability string
// (a node's public key plus DERP/endpoint hints, not a specific connection).
// It identifies a daemon, not a live link: the same Addr can be dialed many
// times, by many peers, across restarts of the dialer.
type Addr string

func (a Addr) String() string { return string(a) }

// Transport is the seam between meshnet and the rest of Relay: everything in
// internal/federation is written against this interface, never against
// tailcat or even RealTransport directly, so tests can swap in NewFakeNetwork
// and CI never dials real tailcat/DERP.
type Transport interface {
	// Listen brings up a listener identified by id, returning the address
	// peers should Dial to reach it. The listener accepts one net.Conn per
	// inbound mesh link; the caller is responsible for running a protocol
	// (see internal/proto's Mesh* types) over each accepted connection.
	Listen(ctx context.Context, id *Identity) (net.Listener, Addr, error)

	// Dial opens one stream to peer, authenticated by the tunnel layer as
	// the identity in id (see Identity.PeerID). It does not itself prove
	// anything about which *session* the dialer may join — that is an
	// application-level check performed over the returned net.Conn.
	Dial(ctx context.Context, id *Identity, peer Addr) (net.Conn, error)
}
