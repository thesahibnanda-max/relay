// Package federation is a daemon's participation in a multi-machine mesh:
// accepting inbound links from other daemons (via a lazily-started
// meshnet.Server, never opened unless this daemon is actually invited into
// or joins a session) and dialing out to join others. It reads and writes
// only the mesh_sessions/mesh_peers/mesh_agents tables in internal/store -
// never the local agents/messages tables directly. The one exception is
// LocalRouter: a small callback interface that lets a Hub ask about, and
// rename, this daemon's own agents, which is the only thing outside
// internal/store's mesh_* tables that gossip and name-collision resolution
// ever need to touch.
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
	"strings"
	"sync"
	"time"

	"github.com/thesahibnanda-max/relay/internal/ids"
	"github.com/thesahibnanda-max/relay/internal/meshnet"
	"github.com/thesahibnanda-max/relay/internal/proto"
	"github.com/thesahibnanda-max/relay/internal/store"
)

// LocalRouter is how a Hub reaches this daemon's own agents. A daemon that
// never uses mesh features never has either method called.
type LocalRouter interface {
	// LocalAgents returns this daemon's own current agents in sessionID,
	// shaped for gossip (OwnerPeer and Version are filled in by the Hub
	// itself, not the caller - see rosterFor - so implementations only need
	// to report Name/Tool/Role/Status/capabilities and a LastSeenAt the Hub
	// can turn into a monotonic version).
	LocalAgents(ctx context.Context, sessionID string) ([]proto.MeshAgentInfo, error)

	// RenameLocalAgent is called only when this daemon's own agent has lost
	// a name collision to one created earlier elsewhere (a smaller ULID -
	// see applyGossipedAgent). It renames the agent, tells its terminal why,
	// and returns the name actually applied (implementations may need to
	// retry proposedName on a further local collision).
	RenameLocalAgent(ctx context.Context, agentID, proposedName string) (string, error)

	// DeliverInbound creates (or, on a resend, finds) this daemon's
	// authoritative message row for a handoff addressed to one of its own
	// agents, applying whatever hold policy an ordinary local send would
	// (hop limit, approve-inbound) - the owning daemon is the only place
	// that can correctly decide this, since only it knows its own agent's
	// live approve-inbound flag. Returns the row's current state as the
	// handoff's synchronous reply.
	DeliverInbound(ctx context.Context, sessionID string, h proto.MeshMsgHandoff) (proto.MeshMsgReceipt, error)

	// ApplyReceipt updates this daemon's mirror row for a message from an
	// asynchronous receipt sent by fromPeer, the tunnel-verified identity of
	// whoever actually sent it (checked against the mirror row's own owner
	// so one peer can never spoof a receipt for a message it was never
	// handed off to).
	ApplyReceipt(ctx context.Context, fromPeer string, r proto.MeshMsgReceipt) error

	// PendingHandoffs returns this daemon's messages addressed to an agent
	// owned by peerID that have never had a receipt applied - i.e. may not
	// have reached peerID yet. Used to resend on reconnect.
	PendingHandoffs(ctx context.Context, sessionID, peerID string) ([]proto.MeshMsgHandoff, error)

	// PendingReceipts returns this daemon's authoritative messages sent by
	// an agent owned by peerID whose current state peerID might not have:
	// still live, or finished recently enough that a receipt could have
	// been missed across a disconnect. Used to resend on reconnect,
	// mirroring the "resend full current truth" idiom mesh_agents gossip
	// already uses for the roster, rather than tracking per-peer
	// acknowledgement state.
	PendingReceipts(ctx context.Context, sessionID, peerID string) ([]proto.MeshMsgReceipt, error)
}

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

	// Router lets the Hub see and rename this daemon's own agents (see
	// LocalRouter). Nil is valid: Hello/Welcome round-trip with an empty
	// local roster, which is all tests that don't care about it need.
	Router LocalRouter
}

// Hub owns one daemon's mesh state. The zero value is not usable; construct
// with New.
type Hub struct {
	opt Options

	mu       sync.Mutex
	listener meshnet.Listener
	addr     meshnet.Addr
	closing  bool
	wg       sync.WaitGroup // background work spawned by this Hub (e.g. a post-collision Resync); Close waits for it
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
//
// Refuses once Close has begun (h.closing), the same guard spawn already
// applies to background work: a Resync goroutine that Close's own wg.Wait
// is currently waiting on could otherwise call this right after Close just
// removed the listener, re-creating a "ghost" one nothing will ever close
// again - starving out any later Hub value constructed for the same
// identity (e.g. a restarted daemon reopening the same mesh session, see
// M-mesh-5) with a permanent "already has a listener" error.
func (h *Hub) ensureListening(ctx context.Context) (meshnet.Addr, error) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.closing {
		return "", errClosing
	}
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

var errClosing = errors.New("federation: hub is closing")

// Close stops accepting new mesh links and waits for any background work
// this Hub spawned (see spawn) to finish or hit its own timeout. It does not
// touch mesh_sessions or mesh_peers: what this daemon has recorded about a
// session survives, so a later Invite/Join on the same session resumes
// from it.
func (h *Hub) Close() error {
	h.mu.Lock()
	h.closing = true
	ln := h.listener
	h.listener = nil
	h.mu.Unlock()
	var err error
	if ln != nil {
		err = ln.Close()
	}
	h.wg.Wait()
	return err
}

// spawn runs fn in the background, tracked so Close can wait for it rather
// than leave it to touch a store that may be closed out from under it
// (e.g. applyGossipedAgent's post-collision Resync). Returns false, running
// nothing, once Close has already been called.
func (h *Hub) spawn(fn func()) bool {
	h.mu.Lock()
	if h.closing {
		h.mu.Unlock()
		return false
	}
	h.wg.Add(1)
	h.mu.Unlock()
	go func() {
		defer h.wg.Done()
		fn()
	}()
	return true
}

func (h *Hub) acceptLoop(ln meshnet.Listener) {
	for {
		conn, verifiedPeerID, err := ln.Accept()
		if err != nil {
			return // listener closed
		}
		if !h.spawn(func() { h.handleInbound(conn, verifiedPeerID) }) {
			conn.Close() // Close raced us: don't process a handshake nobody will wait for
		}
	}
}

// handleInbound answers one mesh link's opening frame. Every kind of mesh
// link this daemon ever accepts - a join/resync handshake, a message
// handoff, or an asynchronous receipt - is exactly one short-lived
// connection carrying one request frame and one reply frame (see the
// package doc's "no persistent connection architecture" note), so the only
// thing that differs between them is which frame type arrives first.
//
// verifiedPeerID is not any frame's own self-reported peer/from-peer field
// (that is only ever a claim the peer makes about itself) - it is what the
// transport itself cryptographically proved for this exact connection (see
// meshnet.Listener). Every handler below requires its frame's own claimed
// identity to agree with this before trusting it for anything.
func (h *Hub) handleInbound(conn net.Conn, verifiedPeerID string) {
	defer conn.Close()
	log := h.opt.Log.With("peer", verifiedPeerID)
	ctx, cancel := context.WithTimeout(context.Background(), helloTimeout)
	defer cancel()
	conn.SetDeadline(time.Now().Add(helloTimeout))

	frame, err := readFrame(conn)
	if err != nil {
		log.Warn("mesh: reading inbound frame", "err", err)
		return
	}
	env, err := proto.MeshUnmarshal(frame)
	if err != nil {
		log.Warn("mesh: malformed inbound frame", "err", err)
		return
	}
	switch env.Type {
	case proto.MeshTypeHello:
		h.handleHello(ctx, conn, verifiedPeerID, env, log)
	case proto.MeshTypeMsgHandoff:
		h.handleHandoff(ctx, conn, verifiedPeerID, env, log)
	case proto.MeshTypeMsgReceipt:
		h.handleReceipt(ctx, conn, verifiedPeerID, env, log)
	default:
		log.Warn("mesh: unexpected inbound frame type", "type", env.Type)
	}
}

// handleHello answers a join/resync handshake: verify the session's join
// secret, record the peer, apply its roster, reply MeshWelcome.
func (h *Hub) handleHello(ctx context.Context, conn net.Conn, verifiedPeerID string, env proto.MeshEnvelope, log *slog.Logger) {
	var hello proto.MeshHello
	if err := json.Unmarshal(env.Payload, &hello); err != nil {
		log.Warn("mesh: malformed inbound hello", "err", err)
		return
	}
	if hello.Session == "" || hello.Secret == "" {
		log.Warn("mesh: inbound hello missing session or secret")
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
	// Defense in depth: Join already refuses to dial your own address
	// before this is ever reached, but a self-connection reaching here by
	// any other path (e.g. NAT hairpinning) must never record this daemon
	// as a "peer" of its own session - see the agent-level equivalent in
	// applyGossipedAgent.
	if verifiedPeerID == h.opt.Identity.PeerID() {
		log.Warn("mesh: inbound hello's verified identity is our own")
		writeMeshError(conn, proto.MeshCodeSelfJoin, "that's you")
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
	for _, a := range hello.Agents {
		if err := h.applyGossipedAgent(ctx, hello.Session, a); err != nil {
			log.Warn("mesh: applying gossiped agent", "agent", a.AgentID, "err", err)
		}
	}

	peers, err := h.opt.Store.ListMeshPeers(ctx, hello.Session)
	if err != nil {
		log.Error("mesh: listing peers", "err", err)
		return
	}
	agents, err := h.rosterFor(ctx, hello.Session)
	if err != nil {
		log.Warn("mesh: gathering roster for welcome", "err", err)
	}
	welcome := proto.MeshWelcome{
		PeerID: sess.SelfPeerID,
		Addr:   string(h.addr),
		Peers:  toMeshPeerInfos(peers),
		Agents: agents,
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

// handleHandoff answers an inbound MeshMsgHandoff: a message addressed to
// one of THIS daemon's own agents, handed off by the daemon that accepted
// it from its sender. Unlike Hello, a handoff carries no join secret of its
// own - it is only ever accepted from a peer already recorded linked for
// this session (a real join secret was already checked on that peer's very
// first Hello), so a stranger who never joined the session at all cannot
// inject one just by guessing an agent id.
func (h *Hub) handleHandoff(ctx context.Context, conn net.Conn, verifiedPeerID string, env proto.MeshEnvelope, log *slog.Logger) {
	var hs proto.MeshMsgHandoff
	if err := json.Unmarshal(env.Payload, &hs); err != nil || hs.ID == "" || hs.SessionID == "" || hs.ToAgent == "" {
		log.Warn("mesh: malformed inbound handoff")
		return
	}
	if _, err := h.opt.Store.GetMeshPeer(ctx, hs.SessionID, verifiedPeerID); err != nil {
		log.Warn("mesh: handoff from a peer never linked to this session", "session", hs.SessionID)
		writeMeshError(conn, proto.MeshCodeUnknownSession, "this daemon does not know you as a member of that session")
		return
	}
	if hs.FromPeer != verifiedPeerID {
		log.Warn("mesh: handoff claimed a different from_peer than the tunnel verified", "claimed", hs.FromPeer)
		writeMeshError(conn, proto.MeshCodeKeyChanged, "identity mismatch")
		return
	}
	if h.opt.Router == nil {
		log.Warn("mesh: no local router configured; cannot deliver")
		return
	}
	receipt, err := h.opt.Router.DeliverInbound(ctx, hs.SessionID, hs)
	if err != nil {
		log.Warn("mesh: delivering inbound handoff", "id", hs.ID, "err", err)
		writeMeshError(conn, proto.MeshCodeUnknownSession, "could not deliver")
		return
	}
	b, err := proto.MeshMarshal(proto.MeshTypeMsgReceipt, receipt)
	if err != nil {
		log.Error("mesh: encoding receipt", "err", err)
		return
	}
	if err := writeFrame(conn, b); err != nil {
		log.Warn("mesh: sending receipt reply", "err", err)
	}
}

// handleReceipt answers an inbound MeshMsgReceipt: an asynchronous update
// from the daemon that owns a message this one holds the mirror row for.
// verifiedPeerID stands in for the sender the same way it does everywhere
// else in this file; LocalRouter.ApplyReceipt is what actually checks it
// against the mirror row's own recorded owner before applying anything.
func (h *Hub) handleReceipt(ctx context.Context, conn net.Conn, verifiedPeerID string, env proto.MeshEnvelope, log *slog.Logger) {
	var r proto.MeshMsgReceipt
	if err := json.Unmarshal(env.Payload, &r); err != nil || r.ID == "" {
		log.Warn("mesh: malformed inbound receipt")
		return
	}
	if h.opt.Router == nil {
		log.Warn("mesh: no local router configured; cannot apply receipt")
		return
	}
	if err := h.opt.Router.ApplyReceipt(ctx, verifiedPeerID, r); err != nil {
		log.Warn("mesh: applying inbound receipt", "id", r.ID, "err", err)
		return
	}
	b, err := proto.MeshMarshal(proto.MeshTypeMsgAck, proto.MeshMsgAck{ID: r.ID})
	if err != nil {
		return
	}
	_ = writeFrame(conn, b)
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
// using blob.Secret, and records the resulting peer plus every peer and
// agent the welcome mentioned (other peers are marked unreachable until
// this daemon actually dials them itself - fanning out to reach them
// directly is M-mesh-5; for now a human can inspect what was learned via
// `relay session peers`).
//
// Join also brings up this daemon's own listener, so whoever it just joined
// (and, later, everyone in the resync) can dial back in: a mesh member is
// symmetric from the moment it joins, not just an outbound-only client.
func (h *Hub) Join(ctx context.Context, blob proto.JoinBlob) error {
	ourAddr, err := h.ensureListening(ctx)
	if err != nil {
		return err
	}
	// A daemon that already hosts this session minted this very blob from
	// this same ensureListening address (see Invite) - dialing it back is
	// never useful (you already know your own local agents without any
	// mesh machinery) and, over a real transport, is a same-identity
	// WireGuard loopback nothing has ever exercised. Reject before any
	// network I/O rather than let it silently succeed or misbehave.
	if blob.PeerAddr == string(ourAddr) {
		return fmt.Errorf("federation: this daemon already hosts session %s; use --session=%s instead of --join", blob.Session, blob.Session)
	}
	ourPeerID := h.opt.Identity.PeerID()
	if _, err := h.opt.Store.EnsureMeshSession(ctx, blob.Session, blob.Secret, ourPeerID); err != nil {
		return fmt.Errorf("federation: recording mesh session: %w", err)
	}
	w, err := h.handshake(ctx, blob.Session, blob.Secret, ourAddr, blob.PeerAddr)
	if err != nil {
		return fmt.Errorf("federation: %w", err)
	}
	if err := h.applyWelcome(ctx, blob.Session, ourPeerID, w); err != nil {
		return fmt.Errorf("federation: %w", err)
	}
	// Fan out to every other peer the seed's Welcome just told us about,
	// right away rather than waiting for some unrelated later trigger (an
	// agent connecting, the periodic sweep) to get around to it - a session
	// is a true N×N mesh from the moment of joining, not hub-and-spoke until
	// something else happens to notice. Detached from ctx (a request-scoped
	// deadline that ends once this HTTP call returns) and tracked via spawn
	// so Close waits for it instead of leaving it to touch a closing store.
	h.spawn(func() {
		rctx, cancel := context.WithTimeout(context.Background(), fanOutTimeout)
		defer cancel()
		h.Resync(rctx, blob.Session)
	})
	return nil
}

// fanOutTimeout bounds the background full-mesh resync Join kicks off -
// generous relative to helloTimeout since it may need several sequential
// rounds to reach a peer only discovered transitively (see Resync).
const fanOutTimeout = 30 * time.Second

// Resync brings this daemon's view of a session's mesh into full N×N
// convergence: it dials every peer it currently knows about, and - because
// each successful handshake's Welcome may itself name a peer this daemon
// has never heard of before (e.g. a peer C only known to B, discovered by
// dialing B) - repeats until a full round dials nothing new. A session's
// mesh is fully connected after this returns as long as the set of peers is
// reachable at all, however many hops of secondhand introduction it took to
// learn about all of them; the seed a daemon joined through is only ever
// needed for that first hop (see the mesh plan's walkthrough (c) - a seed
// that vanishes right after handing over one Welcome is never missed).
//
// Also propagates any local roster change (an agent registered, was
// renamed, or changed status) and flushes any outstanding message
// hand-off/receipt to each peer reached (see flushPending). Best-effort and
// concurrent within each round: a peer that can't be reached right now is
// marked unreachable and retried on the next Resync (the periodic sweep -
// see daemon.Server - or the next local event that triggers one); one
// unreachable peer never stops the rest, and Resync itself never returns an
// error for the same reason callers use it fire-and-forget.
func (h *Hub) Resync(ctx context.Context, sessionID string) {
	sess, err := h.opt.Store.GetMeshSession(ctx, sessionID)
	if err != nil {
		return // not a mesh session, or this daemon isn't a member: nothing to do
	}
	ourAddr, err := h.ensureListening(ctx)
	if err != nil {
		h.opt.Log.Warn("mesh: resync: listening", "err", err)
		return
	}
	dialed := map[string]bool{}
	for {
		peers, err := h.opt.Store.ListMeshPeers(ctx, sessionID)
		if err != nil {
			h.opt.Log.Warn("mesh: resync: listing peers", "err", err)
			return
		}
		var round []store.MeshPeer
		for _, p := range peers {
			if !dialed[p.PeerID] {
				round = append(round, p)
			}
		}
		if len(round) == 0 {
			return // fixed point: every peer we know about has been dialed this call
		}
		var wg sync.WaitGroup
		for _, p := range round {
			dialed[p.PeerID] = true
			wg.Add(1)
			go func(p store.MeshPeer) {
				defer wg.Done()
				w, err := h.handshake(ctx, sessionID, sess.JoinSecret, ourAddr, p.Addr)
				if err != nil {
					h.opt.Log.Warn("mesh: resync with peer failed", "peer", p.PeerID, "err", err)
					_ = h.opt.Store.SetMeshPeerStatus(ctx, sessionID, p.PeerID, store.MeshPeerUnreachable)
					return
				}
				// applyWelcome may record a peer neither in round nor
				// previously known - it gets picked up next round.
				if err := h.applyWelcome(ctx, sessionID, sess.SelfPeerID, w); err != nil {
					h.opt.Log.Warn("mesh: resync: applying welcome", "peer", p.PeerID, "err", err)
				}
				h.flushPending(ctx, sessionID, p.PeerID)
			}(p)
		}
		wg.Wait()
		if ctx.Err() != nil {
			return // out of time: whatever converged so far stands; retried next Resync
		}
	}
}

// flushPending resends anything still outstanding for peerID right after a
// successful reconnect with it - the at-least-once guarantee for both
// directions of message hand-off. Best-effort like the rest of Resync: a
// failure here logs and moves on, picked up again on the next Resync.
func (h *Hub) flushPending(ctx context.Context, sessionID, peerID string) {
	if h.opt.Router == nil {
		return
	}
	handoffs, err := h.opt.Router.PendingHandoffs(ctx, sessionID, peerID)
	if err != nil {
		h.opt.Log.Warn("mesh: listing pending handoffs", "peer", peerID, "err", err)
	}
	for _, hs := range handoffs {
		hs.Resend = true
		if _, err := h.SendMessage(ctx, sessionID, peerID, hs); err != nil {
			h.opt.Log.Warn("mesh: resending handoff", "id", hs.ID, "peer", peerID, "err", err)
		}
	}
	receipts, err := h.opt.Router.PendingReceipts(ctx, sessionID, peerID)
	if err != nil {
		h.opt.Log.Warn("mesh: listing pending receipts", "peer", peerID, "err", err)
	}
	for _, r := range receipts {
		if err := h.SendReceipt(ctx, sessionID, peerID, r); err != nil {
			h.opt.Log.Warn("mesh: resending receipt", "id", r.ID, "peer", peerID, "err", err)
		}
	}
}

// SendMessage hands msg off to its recipient's owning daemon: dials toPeer
// directly (its address must already be known from mesh_peers - a prior
// Join/Resync recorded it), sends a MeshMsgHandoff frame, and applies the
// MeshMsgReceipt it gets back as the immediate reply, the same way any
// other receipt is applied. Used right after routeSend creates a mirror row
// (an immediate best-effort attempt) and again, for anything that never got
// a first receipt, on every subsequent Resync with that peer (see
// flushPending / store.PendingMirrorMessages).
//
// FromPeer on hs is always overwritten with this daemon's own identity
// before sending - never trusted from the caller - the same way rosterFor
// stamps OwnerPeer itself rather than trusting a LocalRouter to know its
// own mesh identity.
func (h *Hub) SendMessage(ctx context.Context, sessionID, toPeer string, hs proto.MeshMsgHandoff) (proto.MeshMsgReceipt, error) {
	hs.FromPeer = h.opt.Identity.PeerID()
	peer, err := h.opt.Store.GetMeshPeer(ctx, sessionID, toPeer)
	if err != nil {
		return proto.MeshMsgReceipt{}, fmt.Errorf("unknown peer %s: %w", toPeer, err)
	}
	conn, err := h.dial(ctx, sessionID, toPeer, peer.Addr)
	if err != nil {
		return proto.MeshMsgReceipt{}, err
	}
	defer conn.Close()
	b, err := proto.MeshMarshal(proto.MeshTypeMsgHandoff, hs)
	if err != nil {
		return proto.MeshMsgReceipt{}, err
	}
	if err := writeFrame(conn, b); err != nil {
		return proto.MeshMsgReceipt{}, fmt.Errorf("sending handoff to %s: %w", toPeer, err)
	}
	frame, err := readFrame(conn)
	if err != nil {
		return proto.MeshMsgReceipt{}, fmt.Errorf("reading %s's receipt: %w", toPeer, err)
	}
	env, err := proto.MeshUnmarshal(frame)
	if err != nil {
		return proto.MeshMsgReceipt{}, fmt.Errorf("malformed reply from %s: %w", toPeer, err)
	}
	switch env.Type {
	case proto.MeshTypeError:
		var e struct{ Code, Message string }
		_ = json.Unmarshal(env.Payload, &e)
		return proto.MeshMsgReceipt{}, fmt.Errorf("%s refused (%s): %s", toPeer, e.Code, e.Message)
	case proto.MeshTypeMsgReceipt:
		var r proto.MeshMsgReceipt
		if err := json.Unmarshal(env.Payload, &r); err != nil {
			return proto.MeshMsgReceipt{}, err
		}
		if h.opt.Router != nil {
			if err := h.opt.Router.ApplyReceipt(ctx, toPeer, r); err != nil {
				h.opt.Log.Warn("mesh: applying receipt from handoff reply", "id", r.ID, "err", err)
			}
		}
		return r, nil
	default:
		return proto.MeshMsgReceipt{}, fmt.Errorf("unexpected reply type %q from %s", env.Type, toPeer)
	}
}

// SendReceipt tells toPeer this daemon's current state for a message it
// holds the mirror row for - the asynchronous half of the exchange, fired
// whenever a message this daemon is authoritative for changes state (see
// daemon.Server.pushMeshReceipt), and again on Resync for anything that
// might have been missed across a disconnect (see PendingReceipts).
func (h *Hub) SendReceipt(ctx context.Context, sessionID, toPeer string, r proto.MeshMsgReceipt) error {
	peer, err := h.opt.Store.GetMeshPeer(ctx, sessionID, toPeer)
	if err != nil {
		return fmt.Errorf("unknown peer %s: %w", toPeer, err)
	}
	conn, err := h.dial(ctx, sessionID, toPeer, peer.Addr)
	if err != nil {
		return err
	}
	defer conn.Close()
	b, err := proto.MeshMarshal(proto.MeshTypeMsgReceipt, r)
	if err != nil {
		return err
	}
	if err := writeFrame(conn, b); err != nil {
		return fmt.Errorf("sending receipt to %s: %w", toPeer, err)
	}
	_, _ = readFrame(conn) // best-effort ack; correctness never depends on reading it (see MeshMsgAck's doc comment)
	return nil
}

// dial opens a short-lived connection to a known peer, marking it
// unreachable in mesh_peers on failure so the next Resync (rather than an
// immediate retry loop here) picks it back up once it's reachable again.
func (h *Hub) dial(ctx context.Context, sessionID, peerID, addr string) (net.Conn, error) {
	conn, err := h.opt.Transport.Dial(ctx, h.opt.Identity, meshnet.Addr(addr))
	if err != nil {
		_ = h.opt.Store.SetMeshPeerStatus(ctx, sessionID, peerID, store.MeshPeerUnreachable)
		return nil, fmt.Errorf("dialing %s: %w", peerID, err)
	}
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(helloTimeout)
	}
	conn.SetDeadline(deadline)
	return conn, nil
}

// handshake dials peerAddr, sends our current MeshHello (roster included),
// and returns the parsed MeshWelcome - shared by Join (first contact) and
// Resync (an already-known peer), so both stay byte-for-byte consistent in
// how they speak the protocol.
func (h *Hub) handshake(ctx context.Context, sessionID, secret string, ourAddr meshnet.Addr, peerAddr string) (proto.MeshWelcome, error) {
	conn, err := h.opt.Transport.Dial(ctx, h.opt.Identity, meshnet.Addr(peerAddr))
	if err != nil {
		return proto.MeshWelcome{}, fmt.Errorf("dialing %s: %w", peerAddr, err)
	}
	defer conn.Close()
	deadline, ok := ctx.Deadline()
	if !ok {
		deadline = time.Now().Add(helloTimeout)
	}
	conn.SetDeadline(deadline)

	agents, err := h.rosterFor(ctx, sessionID)
	if err != nil {
		h.opt.Log.Warn("mesh: gathering local roster for hello", "err", err)
	}
	hello := proto.MeshHello{
		MeshVersion: proto.MeshVersion, Session: sessionID, Secret: secret,
		PeerID: h.opt.Identity.PeerID(), Addr: string(ourAddr), DaemonBuild: h.opt.Build, Agents: agents,
	}
	helloBytes, err := proto.MeshMarshal(proto.MeshTypeHello, hello)
	if err != nil {
		return proto.MeshWelcome{}, fmt.Errorf("encoding hello: %w", err)
	}
	if err := writeFrame(conn, helloBytes); err != nil {
		return proto.MeshWelcome{}, fmt.Errorf("sending hello: %w", err)
	}

	frame, err := readFrame(conn)
	if err != nil {
		return proto.MeshWelcome{}, fmt.Errorf("reading %s's reply: %w", peerAddr, err)
	}
	env, err := proto.MeshUnmarshal(frame)
	if err != nil {
		return proto.MeshWelcome{}, fmt.Errorf("malformed reply from %s: %w", peerAddr, err)
	}
	switch env.Type {
	case proto.MeshTypeError:
		var e struct{ Code, Message string }
		_ = json.Unmarshal(env.Payload, &e)
		return proto.MeshWelcome{}, fmt.Errorf("%s refused (%s): %s", peerAddr, e.Code, e.Message)
	case proto.MeshTypeWelcome:
		var w proto.MeshWelcome
		if err := json.Unmarshal(env.Payload, &w); err != nil {
			return proto.MeshWelcome{}, fmt.Errorf("malformed welcome from %s: %w", peerAddr, err)
		}
		// Unlike the accepting side, there is no separate verified-identity
		// value to compare w.PeerID against here: dialing peerAddr at all
		// only succeeds if the far end holds the private key that address's
		// own embedded public key names, so the tunnel itself already
		// authenticated exactly which daemon we're talking to before we
		// ever sent a byte.
		if w.PeerID == "" {
			return proto.MeshWelcome{}, fmt.Errorf("%s's welcome did not name itself", peerAddr)
		}
		return w, nil
	default:
		return proto.MeshWelcome{}, fmt.Errorf("unexpected reply type %q from %s", env.Type, peerAddr)
	}
}

// applyWelcome records everything a MeshWelcome told us: the peer we just
// linked to directly, every other peer it already knew about (marked
// unreachable until this daemon dials them itself), and its agent roster.
func (h *Hub) applyWelcome(ctx context.Context, sessionID, ourPeerID string, w proto.MeshWelcome) error {
	// Defense in depth, mirroring the secondhand-peers skip below: Join
	// already refuses to dial our own address before a handshake is ever
	// attempted, and handleHello refuses a self-identity on the accepting
	// side - this guards the primary peer too, in case either is ever
	// reached some other way.
	if w.PeerID == ourPeerID {
		return fmt.Errorf("federation: welcome claims to be us (%s)", w.PeerID)
	}
	if err := h.opt.Store.UpsertMeshPeer(ctx, store.MeshPeer{
		SessionID: sessionID, PeerID: w.PeerID, Addr: w.Addr, Status: store.MeshPeerLinked,
	}); err != nil {
		return fmt.Errorf("recording %s: %w", w.PeerID, err)
	}
	for _, p := range w.Peers {
		if p.PeerID == ourPeerID || p.PeerID == w.PeerID {
			continue // that's us, or the peer we just linked to directly
		}
		// A secondhand mention only ever creates a new row (as unreachable,
		// until we dial it ourselves); it must never downgrade a peer we
		// already have more direct knowledge of - see UpsertMeshPeerIfNew.
		if err := h.opt.Store.UpsertMeshPeerIfNew(ctx, store.MeshPeer{
			SessionID: sessionID, PeerID: p.PeerID, Addr: p.Addr, Status: store.MeshPeerUnreachable,
		}); err != nil {
			return fmt.Errorf("recording %s: %w", p.PeerID, err)
		}
	}
	for _, a := range w.Agents {
		if err := h.applyGossipedAgent(ctx, sessionID, a); err != nil {
			h.opt.Log.Warn("mesh: applying gossiped agent", "agent", a.AgentID, "err", err)
		}
	}
	return nil
}

// rosterFor returns everything this daemon would tell a peer about a
// session's agents: its own current ones (via Router, with OwnerPeer
// stamped as this daemon's own identity - LocalRouter implementations don't
// need to know or guess their own PeerID) plus every other peer's agent
// already cached in mesh_agents. Gossip propagates transitively this way,
// without every daemon needing to dial every other one directly.
func (h *Hub) rosterFor(ctx context.Context, sessionID string) ([]proto.MeshAgentInfo, error) {
	var out []proto.MeshAgentInfo
	if h.opt.Router != nil {
		own, err := h.opt.Router.LocalAgents(ctx, sessionID)
		if err != nil {
			return nil, fmt.Errorf("local agents: %w", err)
		}
		selfID := h.opt.Identity.PeerID()
		for _, a := range own {
			a.OwnerPeer = selfID
			out = append(out, a)
		}
	}
	known, err := h.opt.Store.ListMeshAgents(ctx, sessionID)
	if err != nil {
		return out, fmt.Errorf("known mesh agents: %w", err)
	}
	for _, a := range known {
		out = append(out, proto.MeshAgentInfo{
			AgentID: a.AgentID, OwnerPeer: a.OwnerPeer, Name: a.Name, Tool: a.Tool, Role: a.Role,
			Status: a.Status, CanInterrupt: a.CanInterrupt, CanBroadcast: a.CanBroadcast,
			LastSeenAt: a.LastSeenAt, Version: a.Version,
		})
	}
	return out, nil
}

// applyGossipedAgent records what a peer told us about one of its agents,
// then - if that update was actually new information, and this daemon has
// a Router - checks it against our own local agents for a name collision.
//
// Only OUR OWN agent is ever renamed here. If a remote agent's name
// collides with a local one, comparing agent_id (a ULID: lexicographically
// smaller was created first) tells us who loses; if it's ours, we rename it
// and re-gossip immediately so the correction propagates. If it's theirs,
// we do nothing - that daemon will independently reach the identical
// conclusion once gossip reaches it too, and rename its own agent itself.
// This is what keeps collision resolution deterministic without any
// daemon ever needing permission to touch another's agent.
func (h *Hub) applyGossipedAgent(ctx context.Context, sessionID string, info proto.MeshAgentInfo) error {
	if info.AgentID == "" || info.OwnerPeer == "" {
		return nil // malformed/empty entry: ignore rather than fail the whole handshake
	}
	if h.opt.Router != nil {
		// A peer should never gossip an agent_id we minted ourselves, but a
		// Hub must not blindly trust that either - defence in depth against
		// a buggy or malicious peer trying to shadow one of our own agents.
		own, err := h.opt.Router.LocalAgents(ctx, sessionID)
		if err == nil {
			for _, a := range own {
				if a.AgentID == info.AgentID {
					return nil
				}
			}
		}
	}
	applied, err := h.opt.Store.UpsertMeshAgentIfNewer(ctx, store.MeshAgent{
		AgentID: info.AgentID, SessionID: sessionID, OwnerPeer: info.OwnerPeer,
		Name: info.Name, Tool: info.Tool, Role: info.Role, Status: info.Status,
		CanInterrupt: info.CanInterrupt, CanBroadcast: info.CanBroadcast,
		LastSeenAt: info.LastSeenAt, Version: info.Version, Tombstoned: info.Tombstoned,
	})
	if err != nil || !applied || h.opt.Router == nil {
		return err
	}
	ours, err := h.opt.Router.LocalAgents(ctx, sessionID)
	if err != nil {
		return nil // best-effort: the gossip itself was already applied successfully above
	}
	for _, local := range ours {
		if local.AgentID == info.AgentID || !strings.EqualFold(local.Name, info.Name) {
			continue
		}
		if local.AgentID <= info.AgentID {
			break // the remote agent loses; its own daemon renames it, not us
		}
		suffix := local.AgentID
		if len(suffix) > 4 {
			suffix = suffix[len(suffix)-4:]
		}
		newName, err := h.opt.Router.RenameLocalAgent(ctx, local.AgentID, local.Name+"-"+strings.ToLower(suffix))
		if err != nil {
			h.opt.Log.Warn("mesh: renaming a locally colliding agent", "agent", local.AgentID, "err", err)
			break
		}
		h.opt.Log.Info("mesh: renamed a local agent after a name collision",
			"agent", local.AgentID, "old_name", local.Name, "new_name", newName, "lost_to", info.AgentID)
		// Propagate the correction promptly rather than waiting for whatever
		// unrelated event next triggers a Resync. Detached from ctx/this
		// call stack (Resync -> handshake -> applyWelcome -> here would
		// otherwise recurse) with its own bounded timeout, and tracked so
		// Close waits for it instead of leaving it to touch a closed store.
		h.spawn(func() {
			rctx, cancel := context.WithTimeout(context.Background(), 2*helloTimeout)
			defer cancel()
			h.Resync(rctx, sessionID)
		})
		break
	}
	return nil
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
