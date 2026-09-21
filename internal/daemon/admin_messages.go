package daemon

import (
	"encoding/json"
	"net/http"
	"strconv"
	"strings"

	"github.com/thesahibnanda-max/relay/internal/ids"
	"github.com/thesahibnanda-max/relay/internal/proto"
	"github.com/thesahibnanda-max/relay/internal/store"
)

func (s *Server) routeAdminMessages(mux *http.ServeMux) {
	mux.HandleFunc("GET /v1/admin/messages", s.handleListMessages)
	mux.HandleFunc("POST /v1/admin/messages/{id}/approve", func(w http.ResponseWriter, r *http.Request) { s.handleDecide(w, r, true) })
	mux.HandleFunc("POST /v1/admin/messages/{id}/reject", func(w http.ResponseWriter, r *http.Request) { s.handleDecide(w, r, false) })
	mux.HandleFunc("POST /v1/admin/send", s.handleAdminSend)
}

// writeErr maps a protocol error to an HTTP reply.
func writeErr(w http.ResponseWriter, e *proto.Error) {
	code := http.StatusBadRequest
	switch e.Code {
	case proto.CodeNotFound, proto.CodeSessionNotFound, proto.CodeUnknownAgent:
		code = http.StatusNotFound
	case proto.CodeRateLimited:
		code = http.StatusTooManyRequests
	case proto.CodeInternal:
		code = http.StatusInternalServerError
	}
	msg := e.Message
	if len(e.Agents) > 0 {
		names := make([]string, 0, len(e.Agents))
		for _, a := range e.Agents {
			names = append(names, a.Name+" ("+a.Role+")")
		}
		msg += "; agents in this session: " + strings.Join(names, ", ")
	}
	writeJSON(w, code, proto.APIError{Error: msg})
}

func (s *Server) handleListMessages(w http.ResponseWriter, r *http.Request) {
	q := r.URL.Query()
	f := store.MessageFilter{Session: q.Get("session")}
	if v := q.Get("state"); v != "" {
		f.States = strings.Split(v, ",")
	}
	if v := q.Get("limit"); v != "" {
		f.Limit, _ = strconv.Atoi(v)
	}
	if name := q.Get("agent"); name != "" {
		if f.Session == "" {
			writeJSON(w, 400, proto.APIError{Error: "agent filter needs a session"})
			return
		}
		a, err := s.st.FindAgent(r.Context(), f.Session, name)
		if err != nil {
			writeJSON(w, 404, proto.APIError{Error: "no such agent in that session"})
			return
		}
		f.Agent = a.ID
	}
	msgs, err := s.st.ListMessages(r.Context(), f)
	if err != nil {
		writeJSON(w, 500, proto.APIError{Error: "internal error"})
		return
	}
	out := make([]proto.MessageView, 0, len(msgs))
	for _, m := range msgs {
		out = append(out, view(m))
	}
	writeJSON(w, 200, out)
}

func (s *Server) handleDecide(w http.ResponseWriter, r *http.Request, approve bool) {
	res, perr := s.decideHeld(r.Context(), "", "", r.PathValue("id"), approve)
	if perr != nil {
		writeErr(w, perr)
		return
	}
	writeJSON(w, 200, res)
}

// handleAdminSend delivers a message from the human user to an agent.
func (s *Server) handleAdminSend(w http.ResponseWriter, r *http.Request) {
	var req proto.AdminSend
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 2*proto.MaxBodyBytes)).Decode(&req) != nil {
		writeJSON(w, 400, proto.APIError{Error: "bad request"})
		return
	}
	session := req.Session
	if session == "" {
		sessions, err := s.st.ListSessions(r.Context(), false) // active shared sessions
		if err != nil {
			writeJSON(w, 500, proto.APIError{Error: "internal error"})
			return
		}
		if len(sessions) != 1 {
			writeJSON(w, 400, proto.APIError{Error: "say which session: --session=<id> (" + strconv.Itoa(len(sessions)) + " active shared sessions)"})
			return
		}
		session = sessions[0].ID
	} else {
		session = ids.Normalize(session)
	}
	res, perr := s.routeSend(r.Context(), session, sender{name: "user", human: true, canInterrupt: true, canBroadcast: true},
		proto.SendArgs{To: req.To, Body: req.Body, Kind: req.Kind, Priority: req.Priority})
	if perr != nil {
		writeErr(w, perr)
		return
	}
	writeJSON(w, 200, res)
}
