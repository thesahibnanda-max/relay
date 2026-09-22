// Package federation is a daemon's participation in a multi-machine mesh:
// accepting inbound links from other daemons (via a lazily-started
// meshnet.Server, never opened unless this daemon is actually invited into
// or joins a session) and dialing out to join others. It reads and writes
// only the mesh_sessions/mesh_peers tables in internal/store - never the
// local agents/messages tables directly, which stay daemon.Server's own
// job (reached through a router interface in later mesh milestones, once
// gossip and message hand-off exist).
//
// See MEMORY.md section 15 for the full design and the milestone list this
// package is being built across.
package federation

import (
	"bufio"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"sync"
	"time"

	"github.com/thesahibnanda-max/relay/internal/ids"
	"github.com/thesahibnanda-max/relay/internal/meshnet"
	"github.com/thesahibnanda-max/relay/internal/proto"
	"github.com/thesahibnanda-max/relay/internal/store"
)

// helloTimeout bounds both the handshake's network I/O and, absent a
// caller-supplied deadline, how long Join waits overall.
const helloTimeout = 5 * time.Second

// maxFrame bounds one handshake frame. A Hello/Welcome is small; this is a
// defensive limit against a hostile or buggy peer, not a real protocol cap.
const maxFrame = 64 << 10

type Options struct {
	Identity  *meshnet.Identity
	Transport meshnet.Transport
	Store     *store.Store
	Log       *slog.Logger
	Build     string // this daemon's version, informational (MeshHello.DaemonBuild)
}

// Hub owns one daemon's mesh state. The zero value is not usable; construct
// with New.
type Hub struct {
	opt Options

	mu       sync.Mutex
	listener meshnet.Listener
	addr     meshnet.Addr
}

func New(opt Options) *Hub {
	if opt.Log == nil {
		opt.Log = slog.Default()
	}
	return &Hub{opt: opt}
}

// ensureListening lazily brings up this daemon's mesh listener. A daemon
// that never calls Invite or Join never opens one: mesh capability is
// completely inert until used. Safe to call concurrently; idempotent.
func (h *Hub) ensureListening(ctx context.Context) (meshnet.Addr, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.listener != nil {
		return h.addr, nil
	}
	ln, addr, err := h.opt.Transport.Listen(ctx, h.opt.Identity)
	if err != nil {
		return "", fmt.Errorf("federation: listen: %w", err)
	}
	h.listener, h.addr = ln, addr
	go h.acceptLoop(ln)
	return addr, nil
}

// Close stops accepting new mesh links. It does not touch mesh_sessions or
// mesh_peers: what this daemon has recorded about a session survives, so a
// later Invite/Join on the same session resumes from it.
func (h *Hub) Close() error {
	h.mu.Lock()
	ln := h.listener
	h.listener = nil
	h.mu.Unlock()
	if ln == nil {
		return nil
	}
	return ln.Close()
}

func (h *Hub) acceptLoop(ln meshnet.Listener) {
	for {
		conn, verifiedPeerID, err := ln.Accept()
		if err != nil {
			return // listener closed
		}
		go h.handleInbound(conn, verifiedPeerID)
	}
}

// handleInbound answers one mesh link's opening handshake: read MeshHello,
// verify the session's join secret, record the peer, reply MeshWelcome.
//
// verifiedPeerID is not the Hello payload's own PeerID field (that is only
// ever a claim the peer makes about itself) - it is what the transport
// itself cryptographically proved for this exact connection (see
// meshnet.Listener). Requiring the two to agree is what makes recording a
// peer under a given identity trustworthy rather than a matter of taking a
// stranger's word for who they are.
func (h *Hub) handleInbound(conn net.Conn, verifiedPeerID string) {
	defer conn.Close()
	log := h.opt.Log.With("peer", verifiedPeerID)
	ctx, cancel := context.WithTimeout(context.Background(), helloTimeout)
	defer cancel()
	conn.SetDeadline(time.Now().Add(helloTimeout))

	hello, err := readHello(conn)
	if err != nil {
		log.Warn("mesh: bad inbound hello", "err", err)
		return
	}
	if hello.MeshVersion != proto.MeshVersion {
		// Refuses only this one link; every other link and every local
		// session this daemon has are completely unaffected.
		writeMeshError(conn, proto.MeshCodeVersionMismatch, fmt.Sprintf("this daemon speaks mesh v%d", proto.MeshVersion))
		return
	}
	if hello.PeerID != "" && hello.PeerID != verifiedPeerID {
		log.Warn("mesh: hello claimed a different identity than the tunnel verified", "claimed", hello.PeerID)
		writeMeshError(conn, proto.MeshCodeKeyChanged, "identity mismatch")
		return
	}

	sess, err := h.opt.Store.GetMeshSession(ctx, hello.Session)
	if err != nil {
		writeMeshError(conn, proto.MeshCodeUnknownSession, "this daemon is not a member of that session")
		return
	}
	if subtle.ConstantTimeCompare([]byte(hello.Secret), []byte(sess.JoinSecret)) != 1 {
		log.Warn("mesh: wrong join secret", "session", hello.Session)
		writeMeshError(conn, proto.MeshCodeBadSecret, "bad secret")
		return
	}

	// Record the peer under verifiedPeerID - the transport-proven identity,
	// not the claim - keyed by (session, peer_id) so a machine that later
	// rotates its own key simply becomes a distinct, visible new row rather
	// than silently overwriting this one. That structural property is the
	// TOFU pin: there is no separate "did the key change" check to get
	// wrong, because a changed key is a different primary key.
	if err := h.opt.Store.UpsertMeshPeer(ctx, store.MeshPeer{
		SessionID: hello.Session, PeerID: verifiedPeerID, Addr: hello.Addr, Status: store.MeshPeerLinked,
	}); err != nil {
		log.Error("mesh: recording peer", "err", err)
		return
	}

	peers, err := h.opt.Store.ListMeshPeers(ctx, hello.Session)
	if err != nil {
		log.Error("mesh: listing peers", "err", err)
		return
	}
	welcome := proto.MeshWelcome{
		PeerID: sess.SelfPeerID,
		Addr:   string(h.addr),
		Peers:  toMeshPeerInfos(peers),
		// Agents is intentionally left empty: roster gossip is M-mesh-3.
	}
	b, err := proto.MeshMarshal(proto.MeshTypeWelcome, welcome)
	if err != nil {
		log.Error("mesh: encoding welcome", "err", err)
		return
	}
	if err := writeFrame(conn, b); err != nil {
		log.Warn("mesh: sending welcome", "err", err)
	}
}

// Invite makes this daemon reachable (starting its listener on first use,
// if it isn't already) and returns a fresh join blob for sessionID. Any
// current member may call this, not just whoever created the session - a
// blob's minting peer is a one-time bootstrap aid for the new joiner, never
// an ongoing dependency once the join has completed.
func (h *Hub) Invite(ctx context.Context, sessionID string) (proto.JoinBlob, error) {
	addr, err := h.ensureListening(ctx)
	if err != nil {
		return proto.JoinBlob{}, err
	}
	// A fresh secret is generated in case this is the first Invite for the
	// session; EnsureMeshSession keeps the existing one if a row already
	// exists, so this is never used past that first call.
	sess, err := h.opt.Store.EnsureMeshSession(ctx, sessionID, ids.Token(), h.opt.Identity.PeerID())
	if err != nil {
		return proto.JoinBlob{}, fmt.Errorf("federation: %w", err)
	}
	return proto.JoinBlob{Session: sess.SessionID, PeerAddr: string(addr), Secret: sess.JoinSecret}, nil
}

// Join dials blob.PeerAddr, performs the MeshHello/MeshWelcome handshake
// using blob.Secret, and records the resulting peer plus every peer the
// welcome's resync mentioned (marked unreachable until this daemon actually
// dials them itself - fanning out to reach them directly is M-mesh-5; for
// now a human can inspect what was learned via `relay session peers`).
//
// Join also brings up this daemon's own listener, so whoever it just joined
// (and, later, everyone in the resync) can dial back in: a mesh member is
// symmetric from the moment it joins, not just an outbound-only client.
func (h *Hub) Join(ctx context.Context, blob proto.JoinBlob) error {
	ourAddr, err := h.ensureListening(ctx)
	if err != nil {
		return err
	}
	ourPeerID := h.opt.Identity.PeerID()
	if _, err := h.opt.Store.EnsureMeshSession(ctx, blob.Session, blob.Secret, ourPeerID); err != nil {
		return fmt.Errorf("federation: recording mesh session: %w", err)
	}

	conn, err := h.opt.Transport.Dial(ctx, h.opt.Identity, meshnet.Addr(blob.PeerAddr))
	if err != nil {
		return fmt.Errorf("federation: dialing %s: %w", blob.PeerAddr, err)
	}
	defer conn.Close()
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(helloTimeout)
	}
	conn.SetDeadline(deadline)

	hello := proto.MeshHello{
		MeshVersion: proto.MeshVersion, Session: blob.Session, Secret: blob.Secret,
		PeerID: ourPeerID, Addr: string(ourAddr), DaemonBuild: h.opt.Build,
	}
	helloBytes, err := proto.MeshMarshal(proto.MeshTypeHello, hello)
	if err != nil {
		return fmt.Errorf("federation: encoding hello: %w", err)
	}
	if err := writeFrame(conn, helloBytes); err != nil {
		return fmt.Errorf("federation: sending hello: %w", err)
	}

	frame, err := readFrame(conn)
	if err != nil {
		return fmt.Errorf("federation: reading %s's reply: %w", blob.PeerAddr, err)
	}
	env, err := proto.MeshUnmarshal(frame)
	if err != nil {
		return fmt.Errorf("federation: malformed reply from %s: %w", blob.PeerAddr, err)
	}
	switch env.Type {
	case proto.MeshTypeError:
		var e struct{ Code, Message string }
		_ = json.Unmarshal(env.Payload, &e)
		return fmt.Errorf("federation: %s refused the join (%s): %s", blob.PeerAddr, e.Code, e.Message)
	case proto.MeshTypeWelcome:
		var w proto.MeshWelcome
		if err := json.Unmarshal(env.Payload, &w); err != nil {
			return fmt.Errorf("federation: malformed welcome from %s: %w", blob.PeerAddr, err)
		}
		// Unlike the accepting side, there is no separate verified-identity
		// value to compare w.PeerID against here: dialing blob.PeerAddr at
		// all only succeeds if the far end holds the private key that
		// address's own embedded public key names, so the tunnel itself
		// already authenticated exactly which daemon we're talking to
		// before we ever sent a byte.
		if w.PeerID == "" {
			return fmt.Errorf("federation: %s's welcome did not name itself", blob.PeerAddr)
		}
		if err := h.opt.Store.UpsertMeshPeer(ctx, store.MeshPeer{
			SessionID: blob.Session, PeerID: w.PeerID, Addr: w.Addr, Status: store.MeshPeerLinked,
		}); err != nil {
			return fmt.Errorf("federation: recording %s: %w", blob.PeerAddr, err)
		}
		for _, p := range w.Peers {
			if p.PeerID == ourPeerID || p.PeerID == w.PeerID {
				continue // that's us, or the peer we just linked to directly
			}
			if err := h.opt.Store.UpsertMeshPeer(ctx, store.MeshPeer{
				SessionID: blob.Session, PeerID: p.PeerID, Addr: p.Addr, Status: store.MeshPeerUnreachable,
			}); err != nil {
				return fmt.Errorf("federation: recording %s: %w", p.PeerID, err)
			}
		}
		return nil
	default:
		return fmt.Errorf("federation: unexpected reply type %q from %s", env.Type, blob.PeerAddr)
	}
}

// Peers returns everything this daemon has recorded about a session's mesh
// members, most recently seen first.
func (h *Hub) Peers(ctx context.Context, sessionID string) ([]store.MeshPeer, error) {
	return h.opt.Store.ListMeshPeers(ctx, sessionID)
}

func toMeshPeerInfos(peers []store.MeshPeer) []proto.MeshPeerInfo {
	out := make([]proto.MeshPeerInfo, len(peers))
	for i, p := range peers {
		out[i] = proto.MeshPeerInfo{PeerID: p.PeerID, Addr: p.Addr, LastSeen: p.LastSeen}
	}
	return out
}

func readHello(conn net.Conn) (proto.MeshHello, error) {
	frame, err := readFrame(conn)
	if err != nil {
		return proto.MeshHello{}, err
	}
	env, err := proto.MeshUnmarshal(frame)
	if err != nil {
		return proto.MeshHello{}, err
	}
	if env.Type != proto.MeshTypeHello {
		return proto.MeshHello{}, fmt.Errorf("expected %s, got %q", proto.MeshTypeHello, env.Type)
	}
	var hello proto.MeshHello
	if err := json.Unmarshal(env.Payload, &hello); err != nil {
		return proto.MeshHello{}, fmt.Errorf("malformed hello: %w", err)
	}
	if hello.Session == "" || hello.Secret == "" {
		return proto.MeshHello{}, errors.New("hello missing session or secret")
	}
	return hello, nil
}

func writeMeshError(conn net.Conn, code, msg string) {
	b, err := proto.MeshMarshal(proto.MeshTypeError, struct{ Code, Message string }{code, msg})
	if err != nil {
		return
	}
	_ = writeFrame(conn, b)
}

// readFrame/writeFrame use single-JSON-line framing, the same convention
// internal/ctl uses for its own request/reply exchange: a JSON value never
// contains a raw newline byte (strings escape it), so '\n' is an
// unambiguous delimiter.
func readFrame(conn net.Conn) ([]byte, error) {
	return bufio.NewReaderSize(conn, maxFrame).ReadBytes('\n')
}

func writeFrame(conn net.Conn, b []byte) error {
	_, err := conn.Write(append(b, '\n'))
	return err
}
