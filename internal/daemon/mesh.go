package daemon

import (
	"encoding/json"
	"net/http"

	"github.com/thesahibnanda-max/relay/internal/federation"
	"github.com/thesahibnanda-max/relay/internal/meshnet"
	"github.com/thesahibnanda-max/relay/internal/proto"
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
	})
	return s.mesh, nil
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
