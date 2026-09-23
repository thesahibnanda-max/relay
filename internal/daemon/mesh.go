package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/thesahibnanda-max/relay/internal/federation"
	"github.com/thesahibnanda-max/relay/internal/meshnet"
	"github.com/thesahibnanda-max/relay/internal/proto"
	"github.com/thesahibnanda-max/relay/internal/store"
)

// meshTransport picks this daemon's mesh transport: the test-injected
// MeshTransport if set, otherwise a real tailcat-backed one - with its own
// diagnostic logging wired in only when explicitly asked for (MeshDebugLog),
// since tailcat's output is otherwise silently discarded (see RELAY_MESH_DEBUG).
func meshTransport(opt Options) meshnet.Transport {
	if opt.MeshTransport != nil {
		return opt.MeshTransport
	}
	rt := meshnet.RealTransport{DERPMapURL: opt.MeshDERPMapURL}
	if opt.MeshDebugLog {
		rt.Logf = func(format string, args ...any) {
			opt.Log.Info("mesh: tailcat", "detail", fmt.Sprintf(format, args...))
		}
	}
	return rt
}

// hub lazily constructs this daemon's federation.Hub, loading (or creating)
// its persisted mesh identity only the first time it's needed - a daemon
// that never invites or joins a session never touches ~/.relay's mesh/
// directory or opens a tailcat listener at all.
func (s *Server) hub() (*federation.Hub, error) {
	s.meshMu.Lock()
	defer s.meshMu.Unlock()
	if s.mesh != nil {
		return s.mesh, nil
	}
	transport := meshTransport(s.opt)
	id, err := meshnet.LoadOrCreateIdentity(s.opt.Paths.MeshIdentityPath())
	if err != nil {
		return nil, err
	}
	s.mesh = federation.New(federation.Options{
		Identity:  id,
		Transport: transport,
		Store:     s.st,
		Log:       s.log,
		Build:     s.opt.Version,
		Router:    s,
	})
	return s.mesh, nil
}

// resumeMeshOnStartup brings this daemon's mesh listener back up if its
// store already has mesh_sessions rows from a previous process - a machine
// restarting must be reachable again at its persisted identity's address
// without a human re-running `relay session invite`/`--join`, or the
// "resilience" half of the mesh's hard requirements would only hold for a
// network blip, not an actual daemon restart. A no-op, never touching
// ~/.relay/mesh or constructing a Hub, for the overwhelming majority of
// daemons that have never used a mesh feature: mesh_sessions is empty until
// Invite or Join creates a row.
func (s *Server) resumeMeshOnStartup(ctx context.Context) {
	sessions, err := s.st.ListMeshSessionIDs(ctx)
	if err != nil || len(sessions) == 0 {
		return
	}
	h, err := s.hub()
	if err != nil {
		s.log.Warn("mesh: resuming on startup", "err", err)
		return
	}
	for _, sid := range sessions {
		s.spawn(func() {
			rctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			h.Resync(rctx, sid)
		})
	}
}

// LocalAgents implements federation.LocalRouter. Version is this agent's
// last_seen_at in milliseconds: every state change already bumps it
// (Touch, SetAgentStatus, RenameAgent), so gossip's version-gate gets a
// free, monotonic counter with no new column needed. OwnerPeer is left
// unset - the Hub stamps it with this daemon's own PeerID, since a
// LocalRouter has no reason to know or guess its own mesh identity.
func (s *Server) LocalAgents(ctx context.Context, sessionID string) ([]proto.MeshAgentInfo, error) {
	agents, err := s.st.ListAgents(ctx, sessionID, false) // non-exited: connected + disconnected
	if err != nil {
		return nil, err
	}
	out := make([]proto.MeshAgentInfo, len(agents))
	for i, a := range agents {
		ci, cb := s.connFlags(a.ID)
		out[i] = proto.MeshAgentInfo{
			AgentID: a.ID, Name: a.Name, Tool: a.Tool, Role: a.Role, Status: a.Status,
			CanInterrupt: ci, CanBroadcast: cb, LastSeenAt: a.LastSeenAt,
			Version: uint64(a.LastSeenAt.UnixMilli()),
		}
	}
	return out, nil
}

// connFlags reads an agent's role policy off its live connection, if any -
// mirrors connected()'s brief lock/unlock, so callers never hold s.mu
// across a store call.
func (s *Server) connFlags(agentID string) (canInterrupt, canBroadcast bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if c := s.conns[agentID]; c != nil {
		return c.canInterrupt, c.canBroadcast
	}
	return false, false
}

// RenameLocalAgent implements federation.LocalRouter: called only when this
// daemon's own agent has lost a mesh name collision to one created earlier
// elsewhere. It renames the agent (retrying on a further local collision,
// via store.RenameAgent) and tells its terminal why, so the human isn't
// left wondering why their agent's name suddenly changed.
func (s *Server) RenameLocalAgent(ctx context.Context, agentID, proposedName string) (string, error) {
	old, err := s.st.GetAgent(ctx, agentID)
	if err != nil {
		return "", err
	}
	newName, err := s.st.RenameAgent(ctx, agentID, proposedName, s.opt.Namer)
	if err != nil {
		return "", err
	}
	n, err := s.st.CreateMessage(ctx, store.Message{
		SessionID: old.SessionID, FromName: "relay", ToAgent: agentID, ToName: newName,
		Kind: store.KindNotify, Priority: store.P2,
		Body: fmt.Sprintf("You were renamed from %s to %s: another machine's agent already held that name in this session.", old.Name, newName),
	})
	if err != nil {
		s.log.Error("mesh: notifying a renamed agent", "err", err)
	} else {
		s.spawn(func() { s.flush(n.ToAgent, false) })
	}
	return newName, nil
}

// DeliverInbound implements federation.LocalRouter: creates (or, on a
// resend, finds) this daemon's authoritative row for a message handed off
// by another daemon to one of this daemon's own agents, applying the same
// hold policy (hop limit, approve-inbound) an ordinary local send would -
// the owning daemon is the only place that can correctly decide this, since
// only it knows its own agent's live approve-inbound flag. Returns the
// row's current state as the handoff's synchronous reply, whether this call
// just created it or it already existed (a resend after a reconnect).
func (s *Server) DeliverInbound(ctx context.Context, sessionID string, hs proto.MeshMsgHandoff) (proto.MeshMsgReceipt, error) {
	body := strings.TrimSpace(hs.Body)
	switch {
	case hs.ID == "" || hs.ToAgent == "":
		return proto.MeshMsgReceipt{}, fmt.Errorf("handoff missing id or to_agent")
	case body == "":
		return proto.MeshMsgReceipt{}, fmt.Errorf("handoff %s: empty body", hs.ID)
	case len(hs.Body) > proto.MaxBodyBytes:
		return proto.MeshMsgReceipt{}, fmt.Errorf("handoff %s: body too large", hs.ID)
	case !utf8.ValidString(hs.Body):
		return proto.MeshMsgReceipt{}, fmt.Errorf("handoff %s: body is not valid UTF-8", hs.ID)
	case !proto.ValidKind(hs.Kind):
		return proto.MeshMsgReceipt{}, fmt.Errorf("handoff %s: bad kind %q", hs.ID, hs.Kind)
	}
	target, err := s.st.GetAgent(ctx, hs.ToAgent)
	if err != nil || target.SessionID != sessionID {
		return proto.MeshMsgReceipt{}, fmt.Errorf("no such local agent %s in session %s", hs.ToAgent, sessionID)
	}
	if target.Status == "exited" {
		return proto.MeshMsgReceipt{ID: hs.ID, State: store.MsgUndeliverable, Detail: "the target agent has exited", Rev: 1}, nil
	}
	prio := hs.Priority
	if prio < store.P0 || prio > store.P3 {
		prio = store.P2
	}
	state, detail := store.MsgQueued, ""
	switch {
	case hs.Hops >= MaxHops && hs.Hops%MaxHops == 0:
		state, detail = store.MsgHeld, fmt.Sprintf("hop limit: this reply chain is %d messages deep; a human must approve it to continue", hs.Hops)
	case target.ApproveInbound:
		state, detail = store.MsgHeld, "awaiting human approval (target runs with --approve-inbound)"
	}
	created, m, err := s.st.ApplyHandoff(ctx, store.Message{
		ID: hs.ID, SessionID: sessionID, FromAgent: hs.FromAgent, FromName: hs.FromName, FromRole: hs.FromRole,
		FromPeer: hs.FromPeer, ToAgent: hs.ToAgent, ToName: target.Name, Kind: hs.Kind, Priority: prio,
		Thread: hs.Thread, ReplyTo: hs.ReplyTo, Body: hs.Body, Hops: hs.Hops, State: state, Detail: detail,
		Origin: store.OriginLocal, CreatedAt: hs.CreatedAt, ExpiresAt: hs.ExpiresAt,
	})
	if err != nil {
		return proto.MeshMsgReceipt{}, err
	}
	if created {
		s.log.Info("mesh: message accepted from peer", "id", m.ID, "from_peer", hs.FromPeer, "to", target.Name, "state", m.State)
		s.spawn(func() { s.flush(target.ID, false) })
		if m.State == store.MsgHeld {
			s.spawn(func() { s.notifyHeld(target.ID, false) })
		}
	}
	return proto.MeshMsgReceipt{ID: m.ID, State: m.State, Detail: m.Detail, Rev: m.Rev}, nil
}

// ApplyReceipt implements federation.LocalRouter: updates this daemon's
// mirror row for a message from an asynchronous receipt sent by the peer
// that actually owns it. fromPeer is the tunnel-verified identity of
// whoever sent the receipt (see federation.Hub.handleReceipt/SendMessage) -
// checked against the mirror row's own ToPeer so one peer can never spoof a
// receipt for a message it was never handed off to.
func (s *Server) ApplyReceipt(ctx context.Context, fromPeer string, r proto.MeshMsgReceipt) error {
	m, err := s.st.GetMessage(ctx, r.ID)
	if err != nil {
		return err
	}
	if m.Origin != store.OriginMirror || m.ToPeer != fromPeer {
		return fmt.Errorf("receipt %s: not a mirror row owned by peer %s", r.ID, fromPeer)
	}
	applied, updated, err := s.st.ApplyReceipt(ctx, r.ID, r.State, r.Detail, r.Rev)
	if err != nil || !applied {
		return err
	}
	s.log.Info("mesh: receipt applied", "id", r.ID, "from_peer", fromPeer, "state", updated.State)
	if store.IsTerminal(updated.State) && updated.State != store.MsgDone && updated.FromAgent != "" {
		s.notifySender(updated, mirrorFailureReason(updated))
	}
	return nil
}

func mirrorFailureReason(m store.Message) string {
	switch m.State {
	case store.MsgRejected:
		return "was rejected by the user"
	case store.MsgExpired:
		return "expired before it was delivered"
	case store.MsgUndeliverable:
		return "could not be delivered: " + m.ToName + " is gone"
	}
	return "could not be delivered"
}

// PendingHandoffs implements federation.LocalRouter: see
// store.PendingMirrorMessages.
func (s *Server) PendingHandoffs(ctx context.Context, sessionID, peerID string) ([]proto.MeshMsgHandoff, error) {
	msgs, err := s.st.PendingMirrorMessages(ctx, sessionID, peerID, time.Now())
	if err != nil {
		return nil, err
	}
	out := make([]proto.MeshMsgHandoff, len(msgs))
	for i, m := range msgs {
		out[i] = proto.MeshMsgHandoff{
			ID: m.ID, SessionID: m.SessionID, FromAgent: m.FromAgent, FromName: m.FromName, FromRole: m.FromRole,
			ToAgent: m.ToAgent, ToName: m.ToName, Kind: m.Kind, Priority: m.Priority, Hops: m.Hops,
			Thread: m.Thread, ReplyTo: m.ReplyTo, Body: m.Body, CreatedAt: m.CreatedAt, ExpiresAt: m.ExpiresAt, Resend: true,
		}
	}
	return out, nil
}

// PendingReceipts implements federation.LocalRouter: see
// store.PendingReceipts.
func (s *Server) PendingReceipts(ctx context.Context, sessionID, peerID string) ([]proto.MeshMsgReceipt, error) {
	msgs, err := s.st.PendingReceipts(ctx, sessionID, peerID, time.Now().Add(-5*time.Minute))
	if err != nil {
		return nil, err
	}
	out := make([]proto.MeshMsgReceipt, len(msgs))
	for i, m := range msgs {
		out[i] = proto.MeshMsgReceipt{ID: m.ID, State: m.State, Detail: m.Detail, Rev: m.Rev}
	}
	return out, nil
}

// sendMeshHandoff makes the first delivery attempt for a freshly created
// mirror message: dial its owning peer directly and hand it off. Best
// effort and fire-and-forget from routeSend's perspective - the RPC has
// already returned "queued" to the sender; if the peer turns out to be
// unreachable right now, the next Resync's PendingHandoffs flush retries it
// once a link to that peer exists again (see federation.Hub.flushPending).
func (s *Server) sendMeshHandoff(m store.Message) {
	s.meshMu.Lock()
	h := s.mesh
	s.meshMu.Unlock()
	if h == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	hs := proto.MeshMsgHandoff{
		ID: m.ID, SessionID: m.SessionID, FromAgent: m.FromAgent, FromName: m.FromName, FromRole: m.FromRole,
		ToAgent: m.ToAgent, ToName: m.ToName, Kind: m.Kind, Priority: m.Priority, Hops: m.Hops,
		Thread: m.Thread, ReplyTo: m.ReplyTo, Body: m.Body, CreatedAt: m.CreatedAt, ExpiresAt: m.ExpiresAt,
	}
	if _, err := h.SendMessage(ctx, m.SessionID, m.ToPeer, hs); err != nil {
		s.log.Warn("mesh: sending handoff", "id", m.ID, "peer", m.ToPeer, "err", err)
	}
}

// pushMeshReceipt tells a message's mirror-holding peer this daemon's
// current state for it, best-effort - fire-and-forget from every call site
// that just changed an authoritative message's state, so the common,
// healthy-network case delivers a receipt immediately; the next Resync's
// PendingReceipts flush is the safety net for when the peer was
// unreachable at that moment.
func (s *Server) pushMeshReceipt(m store.Message) {
	if m.FromPeer == "" {
		return
	}
	s.meshMu.Lock()
	h := s.mesh
	s.meshMu.Unlock()
	if h == nil {
		return // shouldn't happen: FromPeer is only ever set via a mesh handoff, which only happens once hub() has already run
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := h.SendReceipt(ctx, m.SessionID, m.FromPeer, proto.MeshMsgReceipt{ID: m.ID, State: m.State, Detail: m.Detail, Rev: m.Rev}); err != nil {
		s.log.Warn("mesh: sending receipt", "id", m.ID, "peer", m.FromPeer, "err", err)
	}
}

// gossipRoster propagates a local roster change (an agent connected,
// disconnected or exited) to sessionID's mesh peers, if it has any. Cheap
// for the overwhelming majority of sessions that never use mesh features:
// it only does anything once a Hub already exists (s.mesh is set only by
// hub(), called only from a mesh admin request), so a purely local session
// never even constructs one just because an agent connected.
func (s *Server) gossipRoster(sessionID string) {
	s.meshMu.Lock()
	h := s.mesh
	s.meshMu.Unlock()
	if h == nil {
		return
	}
	s.spawn(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		h.Resync(ctx, sessionID)
	})
}

// resyncAllMeshSessions is the periodic safety net for healing a mesh
// partition: gossipRoster only fires off a local agent connecting,
// disconnecting or exiting, so a daemon sitting idle after a peer becomes
// reachable again (or after this daemon itself was offline) would otherwise
// never notice until some unrelated event happened to trigger a Resync.
// Called from sweep(), reusing the existing expiry ticker rather than a
// dedicated one; each session's resync is spawned in the background
// (bounded, like gossipRoster's) so a slow or unreachable mesh peer can
// never delay sweep()'s other duties (reapGone, message expiry) for
// everyone else. A no-op, and never even reading ~/.relay/mesh, for the
// overwhelming majority of daemons that have never used a mesh feature:
// mesh_sessions is empty until Invite or Join creates a row, and s.mesh
// stays nil until hub() has actually been called.
func (s *Server) resyncAllMeshSessions(ctx context.Context) {
	s.meshMu.Lock()
	h := s.mesh
	s.meshMu.Unlock()
	if h == nil {
		return
	}
	sessions, err := s.st.ListMeshSessionIDs(ctx)
	if err != nil {
		s.log.Warn("mesh: listing mesh sessions for periodic resync", "err", err)
		return
	}
	for _, sid := range sessions {
		s.spawn(func() {
			rctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			h.Resync(rctx, sid)
		})
	}
}

func (s *Server) handleMeshInvite(w http.ResponseWriter, r *http.Request) {
	var req proto.InviteMeshSessionRequest
	if r.Body != nil {
		_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req)
	}
	if req.SessionID == "" {
		writeJSON(w, 400, proto.APIError{Error: "session_id is required"})
		return
	}
	if _, err := s.st.GetSession(r.Context(), req.SessionID); err != nil {
		writeJSON(w, 404, proto.APIError{Error: "no such session"})
		return
	}
	h, err := s.hub()
	if err != nil {
		writeJSON(w, 500, proto.APIError{Error: err.Error()})
		return
	}
	blob, err := h.Invite(r.Context(), req.SessionID)
	if err != nil {
		writeJSON(w, 500, proto.APIError{Error: err.Error()})
		return
	}
	writeJSON(w, 200, blob)
}

func (s *Server) handleMeshJoin(w http.ResponseWriter, r *http.Request) {
	var blob proto.JoinBlob
	if r.Body == nil || json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&blob) != nil {
		writeJSON(w, 400, proto.APIError{Error: "malformed join blob"})
		return
	}
	h, err := s.hub()
	if err != nil {
		writeJSON(w, 500, proto.APIError{Error: err.Error()})
		return
	}
	if err := h.Join(r.Context(), blob); err != nil {
		writeJSON(w, 502, proto.APIError{Error: err.Error()})
		return
	}
	writeJSON(w, 200, map[string]string{"status": "joined"})
}

func (s *Server) handleMeshPeers(w http.ResponseWriter, r *http.Request) {
	sessionID := r.PathValue("id")
	if _, err := s.st.GetSession(r.Context(), sessionID); err != nil {
		writeJSON(w, 404, proto.APIError{Error: "no such session"})
		return
	}
	h, err := s.hub()
	if err != nil {
		writeJSON(w, 500, proto.APIError{Error: err.Error()})
		return
	}
	peers, err := h.Peers(r.Context(), sessionID)
	if err != nil {
		writeJSON(w, 500, proto.APIError{Error: "internal error"})
		return
	}
	out := make([]proto.MeshPeerView, len(peers))
	for i, p := range peers {
		out[i] = proto.MeshPeerView{PeerID: p.PeerID, Addr: p.Addr, FirstSeen: p.FirstSeen, LastSeen: p.LastSeen, Status: p.Status}
	}
	writeJSON(w, 200, out)
}
