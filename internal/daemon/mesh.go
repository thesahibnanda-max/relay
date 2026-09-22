package daemon

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"time"

	"github.com/thesahibnanda-max/relay/internal/federation"
	"github.com/thesahibnanda-max/relay/internal/meshnet"
	"github.com/thesahibnanda-max/relay/internal/proto"
	"github.com/thesahibnanda-max/relay/internal/store"
)

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
	transport := s.opt.MeshTransport
	if transport == nil {
		transport = meshnet.RealTransport{DERPMapURL: s.opt.MeshDERPMapURL}
	}
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
