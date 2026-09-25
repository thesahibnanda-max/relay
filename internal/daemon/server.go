// Package daemon is relayd: the per-user process that owns sessions, the
// agent registry and durable storage. Agents connect to it over a WebSocket on
// a private unix socket; the CLI talks to its admin API on the same socket.
package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"

	"github.com/thesahibnanda-max/relay/internal/naming"
	"github.com/thesahibnanda-max/relay/internal/peercred"
	"github.com/thesahibnanda-max/relay/internal/proto"
	"github.com/thesahibnanda-max/relay/internal/relayhome"
	"github.com/thesahibnanda-max/relay/internal/store"
)

const (
	maxBatch     = 1000 // events per message
	writeTimeout = 10 * time.Second
)

type Options struct {
	Paths        relayhome.Paths
	Version      string
	Log          *slog.Logger
	Namer        *naming.Namer
	PingEvery    time.Duration // default 15s
	PongTimeout  time.Duration // default 10s
	HelloTimeout time.Duration // default 5s
	ExpireEvery  time.Duration // how often expired messages are swept (default 30s)
	MessageTTL   time.Duration // how long an undelivered message lives (default 1h)

	// Quotas (0 = default).
	MaxAgentsPerSession int           // live agents in one session (default 32)
	MaxRawBytes         int64         // raw terminal log kept per agent (default 1 GiB); beyond it output is no longer recorded
	RPCLimit            int           // agent RPCs allowed per RPCWindow (default 200 per 10 s)
	RPCWindow           time.Duration //
	WriteTimeout        time.Duration // a peer that cannot take a frame this fast is disconnected (default 10 s)
	DisconnectGrace     time.Duration // an agent gone this long is treated as exited (default 15 min)
	PairLimit           int           // messages per sender->target pair per minute (default 20)
	SenderLimit         int           // messages per sender per minute (default 60)

	// OnShutdownRequested, if set, is called by POST /v1/admin/shutdown to
	// begin graceful shutdown - the RPC-triggered equivalent of Run's ctx
	// being cancelled by a signal. Windows has no SIGTERM to send a detached
	// daemon, so lifecycle_windows.go's Stop calls this endpoint instead;
	// registering it is harmless on every platform (Unix's Stop still uses
	// SIGTERM, unchanged).
	OnShutdownRequested context.CancelFunc
}

type Server struct {
	opt     Options
	st      *store.Store
	log     *slog.Logger
	started time.Time
	http    *http.Server

	mu    sync.Mutex
	conns map[string]*agentConn // by agent id
	live  map[string]liveState  // by agent id: what its tool is doing
	wg    sync.WaitGroup

	closing              bool // set at Shutdown: no new background work starts
	pairs, senders, rpcs slidingWindow
	compress             bgCompress
	stop                 context.CancelFunc
}

type agentConn struct {
	id           string
	name, role   string
	sessionID    string
	ws           *websocket.Conn
	canInterrupt bool
	canBroadcast bool

	ready    atomic.Bool // welcome sent: safe to push messages
	flushMu  sync.Mutex  // serialises pushes so order is kept
	rpcSlots chan struct{}
}

// New opens the store. Any agent recorded as connected belonged to a previous
// daemon process, so it is marked disconnected until it reconnects.
func New(opt Options) (*Server, error) {
	if opt.Log == nil {
		opt.Log = slog.New(slog.NewTextHandler(os.Stderr, nil))
	}
	if opt.PingEvery == 0 {
		opt.PingEvery = 15 * time.Second
	}
	if opt.PongTimeout == 0 {
		opt.PongTimeout = 10 * time.Second
	}
	if opt.HelloTimeout == 0 {
		opt.HelloTimeout = 5 * time.Second
	}
	if opt.DisconnectGrace == 0 {
		opt.DisconnectGrace = 15 * time.Minute
	}
	if opt.PairLimit == 0 {
		opt.PairLimit = pairLimit
	}
	if opt.SenderLimit == 0 {
		opt.SenderLimit = senderLimit
	}
	if opt.MaxAgentsPerSession == 0 {
		opt.MaxAgentsPerSession = 32
	}
	if opt.MaxRawBytes == 0 {
		opt.MaxRawBytes = 1 << 30
	}
	if opt.RPCLimit == 0 {
		opt.RPCLimit = 200
	}
	if opt.RPCWindow == 0 {
		opt.RPCWindow = 10 * time.Second
	}
	if opt.WriteTimeout == 0 {
		opt.WriteTimeout = writeTimeout
	}
	if opt.MessageTTL == 0 {
		opt.MessageTTL = store.DefaultMessageTTL
	}
	if opt.ExpireEvery == 0 {
		opt.ExpireEvery = 30 * time.Second
	}
	if opt.Namer == nil {
		opt.Namer = naming.New()
	}
	if err := opt.Paths.Ensure(); err != nil {
		return nil, err
	}
	st, err := store.Open(opt.Paths.DBPath())
	if err != nil {
		return nil, fmt.Errorf("open store: %w", err)
	}
	if err := st.MarkAllDisconnected(context.Background()); err != nil {
		st.Close()
		return nil, err
	}
	s := &Server{opt: opt, st: st, log: opt.Log, started: time.Now(), conns: map[string]*agentConn{}, live: map[string]liveState{}}
	bg, stop := context.WithCancel(context.Background())
	s.stop = stop
	s.wg.Add(1)
	go func() { defer s.wg.Done(); s.expireLoop(bg) }()

	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/agent", s.handleAgent)
	mux.HandleFunc("GET /v1/admin/status", s.handleStatus)
	mux.HandleFunc("GET /v1/admin/sessions", s.handleListSessions)
	mux.HandleFunc("POST /v1/admin/sessions", s.handleCreateSession)
	mux.HandleFunc("GET /v1/admin/sessions/{id}", s.handleGetSession)
	mux.HandleFunc("POST /v1/admin/sessions/{id}/end", s.handleEndSession)
	mux.HandleFunc("POST /v1/admin/gc", s.handleGC)
	mux.HandleFunc("POST /v1/admin/shutdown", s.handleShutdown)
	s.routeAdminMessages(mux)
	s.http = &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	return s, nil
}

// Serve accepts connections until Shutdown. Connections from other users are
// dropped (defence in depth; the socket is also 0600 in a 0700 directory).
func (s *Server) Serve(ln net.Listener) error {
	err := s.http.Serve(&peerListener{Listener: ln, uid: uint32(os.Getuid()), log: s.log})
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Shutdown stops accepting, disconnects agents, waits for handlers and closes the store.
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	s.closing = true
	s.mu.Unlock()
	s.stop()
	err := s.http.Shutdown(ctx)
	s.mu.Lock()
	for _, c := range s.conns {
		c.ws.CloseNow() // agents reconnect to the next daemon; no time for a close handshake per connection
	}
	s.mu.Unlock()
	done := make(chan struct{})
	go func() { s.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-ctx.Done():
	}
	s.compress.wg.Wait() // let background compression finish before the process goes
	if cerr := s.st.Close(); err == nil {
		err = cerr
	}
	return err
}

type peerListener struct {
	net.Listener
	uid uint32
	log *slog.Logger
}

func (p *peerListener) Accept() (net.Conn, error) {
	for {
		c, err := p.Listener.Accept()
		if err != nil {
			return nil, err
		}
		if uid, err := peercred.UID(c); err == nil && uid != p.uid {
			p.log.Warn("rejected connection from another user", "uid", uid)
			c.Close()
			continue
		}
		// If credentials are unavailable on this OS we fall back to socket permissions.
		return c, nil
	}
}

// ---- agent WebSocket -------------------------------------------------------

func (s *Server) sendJSON(ctx context.Context, ws *websocket.Conn, typ string, payload any) error {
	b, err := proto.Marshal(typ, payload)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, s.opt.WriteTimeout)
	defer cancel()
	return ws.Write(ctx, websocket.MessageText, b)
}

func (s *Server) reject(ctx context.Context, ws *websocket.Conn, code, msg string) {
	_ = s.sendJSON(ctx, ws, proto.TypeError, proto.Error{Code: code, Message: msg})
	ws.Close(websocket.StatusPolicyViolation, code)
}

func (s *Server) handleAgent(w http.ResponseWriter, r *http.Request) {
	// A browser always sends Origin; a Relay agent never does. Refusing any
	// Origin stops web pages from talking to the daemon.
	if r.Header.Get("Origin") != "" {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	ws, err := websocket.Accept(w, r, nil)
	if err != nil {
		return
	}
	ws.SetReadLimit(4 << 20)
	if !s.enter() { // shutting down: do not start something Shutdown will not wait for
		ws.Close(websocket.StatusGoingAway, "daemon shutting down")
		return
	}
	defer s.wg.Done()

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	// ---- handshake
	hctx, hcancel := context.WithTimeout(ctx, s.opt.HelloTimeout)
	_, data, err := ws.Read(hctx)
	hcancel()
	if err != nil {
		ws.CloseNow()
		return
	}
	env, err := proto.Unmarshal(data)
	var hello proto.Hello
	if err != nil || env.Type != proto.TypeHello || json.Unmarshal(env.Payload, &hello) != nil {
		s.reject(ctx, ws, proto.CodeBadRequest, "expected a hello message")
		return
	}
	if hello.Proto != proto.Version {
		s.reject(ctx, ws, proto.CodeProtoMismatch, fmt.Sprintf("protocol mismatch: agent speaks v%d, daemon v%d (run `relay daemon stop` and retry)", hello.Proto, proto.Version))
		return
	}
	if !proto.ValidTool(hello.Tool) || !proto.ValidRole(hello.Role) || len(hello.Cwd) > 4096 ||
		len(hello.Client) > 64 || len(hello.Token) > 128 || len(hello.Session) > 64 || len(hello.RoleSource) > 4096 {
		s.reject(ctx, ws, proto.CodeBadRequest, "invalid tool, role, cwd or version string")
		return
	}
	hello.Cwd = stripControl(hello.Cwd) // it is shown in listings
	hello.RoleSource = stripControl(hello.RoleSource)

	sess, agent, token, resumed, perr := s.admit(ctx, hello)
	if perr != nil {
		s.reject(ctx, ws, perr.Code, perr.Message)
		return
	}
	log := s.log.With("agent", agent.Name, "agent_id", agent.ID, "session", sess.ID)

	// Replace any stale connection for this agent, then register ours.
	s.mu.Lock()
	if s.closing {
		// Shutdown has already enumerated the connections it will close; this
		// one would outlive it as a zombie whose store is gone.
		s.mu.Unlock()
		_ = s.st.SetAgentStatus(context.Background(), agent.ID, "disconnected", nil)
		ws.CloseNow()
		return
	}
	if old := s.conns[agent.ID]; old != nil {
		old.ws.CloseNow()
	}
	me := s.newConn(agent.ID, agent.Name, agent.Role, hello, sess.ID, ws)
	s.conns[agent.ID] = me
	s.mu.Unlock()

	exited := false
	defer func() {
		s.mu.Lock()
		current := s.conns[agent.ID] == me
		if current {
			delete(s.conns, agent.ID)
		}
		s.mu.Unlock()
		if current && !exited {
			_ = s.st.SetAgentStatus(context.Background(), agent.ID, "disconnected", nil)
			log.Info("agent disconnected")
		}
		ws.CloseNow()
	}()

	rw, err := openRaw(s.opt.Paths.RawDir(), sess.ID, agent.ID, s.opt.MaxRawBytes)
	if rw != nil {
		rw.bg = &s.compress
	}
	if err != nil {
		log.Error("open raw store", "err", err)
		s.reject(ctx, ws, proto.CodeInternal, "cannot open storage")
		return
	}
	defer func() {
		if n := rw.Dropped(); n > 0 {
			log.Warn("raw terminal log quota reached; output no longer recorded", "events_dropped", n)
		}
		rw.Close()
	}()

	welcome := proto.Welcome{
		Session:  proto.SessionRef{ID: sess.ID, Name: sess.Name, Kind: sess.Kind},
		Agent:    proto.AgentRef{ID: agent.ID, Name: agent.Name, Tool: agent.Tool, Role: agent.Role},
		Token:    token,
		AckedSeq: agent.AckedSeq,
		Server:   s.opt.Version,
		Resumed:  resumed,
	}
	if err := s.sendJSON(ctx, ws, proto.TypeWelcome, welcome); err != nil {
		return
	}
	log.Info("agent connected", "tool", agent.Tool, "role", agent.Role, "resumed", resumed)
	me.ready.Store(true)
	s.spawn(func() { s.flush(agent.ID, true) })      // anything queued while it was away
	s.spawn(func() { s.notifyHeld(agent.ID, true) }) // only if something is waiting for a human

	// Liveness: ping periodically; a missing pong closes the connection.
	go func() {
		t := time.NewTicker(s.opt.PingEvery)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				pctx, pcancel := context.WithTimeout(ctx, s.opt.PongTimeout)
				err := ws.Ping(pctx)
				pcancel()
				if err != nil {
					ws.CloseNow()
					return
				}
			}
		}
	}()

	// ---- message loop
	acked := agent.AckedSeq // events at or below this are already stored
	for {
		_, data, err := ws.Read(ctx)
		if err != nil {
			return
		}
		env, err := proto.Unmarshal(data)
		if err != nil {
			s.reject(ctx, ws, proto.CodeBadRequest, "malformed message")
			return
		}
		switch env.Type {
		case proto.TypeEvents:
			var ev proto.Events
			if json.Unmarshal(env.Payload, &ev) != nil || len(ev.Events) > maxBatch {
				s.reject(ctx, ws, proto.CodeBadRequest, "bad events batch")
				return
			}
			var err error
			acked, err = s.persist(ctx, agent.ID, rw, ev.Events, acked)
			if err != nil {
				log.Error("persist events", "err", err)
				s.reject(ctx, ws, proto.CodeInternal, "cannot store events")
				return
			}
			if err := s.sendJSON(ctx, ws, proto.TypeAck, proto.Ack{Seq: acked}); err != nil {
				return
			}
		case proto.TypeRPC:
			var req proto.RPC
			if json.Unmarshal(env.Payload, &req) != nil || req.ID == "" {
				s.reject(ctx, ws, proto.CodeBadRequest, "bad rpc")
				return
			}
			select {
			case me.rpcSlots <- struct{}{}:
			default:
				_ = s.sendJSON(ctx, ws, proto.TypeResult, proto.Result{ID: req.ID, Error: rpcErr(proto.CodeRateLimited, "too many requests in flight")})
				continue
			}
			if !s.spawn(func() {
				defer func() { <-me.rpcSlots }()
				s.handleRPC(ctx, me, req)
			}) {
				<-me.rpcSlots
				return
			}
		case proto.TypeBye:
			var bye proto.Bye
			_ = json.Unmarshal(env.Payload, &bye)
			exited = true
			// Deregister first so the agent never reads "exited" yet "connected".
			s.mu.Lock()
			if s.conns[agent.ID] == me {
				delete(s.conns, agent.ID)
			}
			s.mu.Unlock()
			_ = s.st.SetAgentStatus(context.Background(), agent.ID, "exited", &bye.ExitCode)
			s.failFor(context.Background(), agent.ID)
			if sess.Kind == "solo" {
				_ = s.st.EndSession(context.Background(), sess.ID)
			}
			log.Info("agent exited", "code", bye.ExitCode)
			ws.Close(websocket.StatusNormalClosure, "bye")
			return
		default:
			s.reject(ctx, ws, proto.CodeBadRequest, "unexpected message type "+env.Type)
			return
		}
	}
}

// persist stores a batch: raw events to the segment file, the rest to SQLite,
// and advances the acked seq. The raw file is flushed before the commit so an
// ack never claims more than is on disk.
func (s *Server) persist(ctx context.Context, agentID string, rw *rawWriter, evs []proto.Event, alreadyAcked uint64) (uint64, error) {
	var rows []store.EventRow
	var fresh []proto.Event
	max := alreadyAcked
	for _, e := range evs {
		if e.Seq == 0 {
			return 0, errors.New("event with seq 0")
		}
		if e.Seq <= alreadyAcked {
			continue // resend of something already stored
		}
		if e.Seq > max {
			max = e.Seq
		}
		fresh = append(fresh, e)
		if e.IsRaw() {
			continue
		}
		payload := "{}"
		if len(e.Meta) > 0 {
			payload = string(e.Meta)
		}
		rows = append(rows, store.EventRow{Seq: e.Seq, TS: e.T, Type: e.Type, Payload: payload})
	}
	if err := rw.Write(fresh); err != nil {
		return 0, err
	}
	if err := rw.Flush(); err != nil {
		return 0, err
	}
	return s.st.AppendEvents(ctx, agentID, rows, max)
}

// admit resolves the session and registers or resumes the agent.
func (s *Server) admit(ctx context.Context, h proto.Hello) (sess store.Session, agent store.Agent, token string, resumed bool, perr *proto.Error) {
	fail := func(err error) (store.Session, store.Agent, string, bool, *proto.Error) {
		return store.Session{}, store.Agent{}, "", false, mapErr(err)
	}
	var err error
	created := false // did this call create the session?
	switch {
	case h.Token != "": // resume
		if h.Session == "" || h.Session == proto.SessionNew || h.Name == "" {
			return fail(store.ErrNotFound)
		}
		if sess, err = s.st.GetSession(ctx, h.Session); err != nil {
			return fail(err)
		}
		if agent, err = s.st.ResumeAgent(ctx, sess.ID, h.Name, h.Token); err != nil {
			return fail(err)
		}
		return sess, agent, "", true, nil
	case h.Session == "": // private solo session
		if sess, err = s.st.CreateSession(ctx, "solo", ""); err != nil {
			return fail(err)
		}
		created = true
	case h.Session == proto.SessionNew:
		if sess, err = s.st.CreateSession(ctx, "shared", ""); err != nil {
			return fail(err)
		}
		created = true
	default:
		if sess, err = s.st.GetSession(ctx, h.Session); err != nil {
			return fail(err)
		}
	}
	if live, lerr := s.st.ListAgents(ctx, sess.ID, false); lerr == nil && len(live) >= s.opt.MaxAgentsPerSession {
		if created {
			_ = s.st.EndSession(ctx, sess.ID)
		}
		return store.Session{}, store.Agent{}, "", false, &proto.Error{Code: proto.CodeSessionFull,
			Message: fmt.Sprintf("this session already has %d agents (the limit)", len(live))}
	}
	agent, token, err = s.st.RegisterAgent(ctx, store.RegisterParams{
		SessionID: sess.ID, Tool: h.Tool, Role: h.Role, RoleSource: h.RoleSource, Name: h.Name,
		ApproveInbound: h.ApproveInbound, PID: h.PID, Cwd: h.Cwd,
	}, s.opt.Namer)
	if err != nil {
		if created {
			_ = s.st.EndSession(ctx, sess.ID) // don't leave an empty session behind
		}
		return fail(err)
	}
	return sess, agent, token, false, nil
}

func mapErr(err error) *proto.Error {
	switch {
	case errors.Is(err, store.ErrNotFound):
		return &proto.Error{Code: proto.CodeSessionNotFound, Message: "no such session or agent"}
	case errors.Is(err, store.ErrSessionEnded):
		return &proto.Error{Code: proto.CodeSessionEnded, Message: "that session has ended"}
	case errors.Is(err, store.ErrNameTaken):
		return &proto.Error{Code: proto.CodeNameTaken, Message: "that name is already used in this session"}
	case errors.Is(err, store.ErrBadName):
		return &proto.Error{Code: proto.CodeBadName, Message: err.Error()}
	case errors.Is(err, store.ErrBadToken):
		return &proto.Error{Code: proto.CodeBadToken, Message: "resume token rejected"}
	case errors.Is(err, store.ErrAgentLive):
		return &proto.Error{Code: proto.CodeAgentLive, Message: "that agent is still connected"}
	default:
		return &proto.Error{Code: proto.CodeInternal, Message: "internal error"}
	}
}

// ---- admin API -------------------------------------------------------------

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	_ = json.NewEncoder(w).Encode(v)
}

func (s *Server) connected(agentID string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	_, ok := s.conns[agentID]
	return ok
}

func (s *Server) sessionInfo(ctx context.Context, sess store.Session, withExited bool) (proto.SessionInfo, error) {
	agents, err := s.st.ListAgents(ctx, sess.ID, withExited)
	if err != nil {
		return proto.SessionInfo{}, err
	}
	info := proto.SessionInfo{ID: sess.ID, Name: sess.Name, Kind: sess.Kind, Status: sess.Status, CreatedAt: sess.CreatedAt, Agents: []proto.AgentInfo{}}
	for _, a := range agents {
		info.Agents = append(info.Agents, proto.AgentInfo{
			ID: a.ID, Name: a.Name, Tool: a.Tool, Role: a.Role, Status: a.Status, Connected: s.connected(a.ID),
			ApproveInbound: a.ApproveInbound, PID: a.PID, Cwd: a.Cwd, JoinedAt: a.CreatedAt, LastSeen: a.LastSeenAt, ExitCode: a.ExitCode,
		})
	}
	return info, nil
}

// handleShutdown begins the same shutdown path Run's ctx.Done() triggers -
// not a separate ad-hoc mechanism, so Windows and Unix daemons stop through
// identical internal code, differing only in how the trigger is delivered
// (a signal cancelling ctx directly, vs. this RPC calling OnShutdownRequested).
func (s *Server) handleShutdown(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusAccepted)
	if s.opt.OnShutdownRequested != nil {
		s.opt.OnShutdownRequested()
	}
}

func (s *Server) handleStatus(w http.ResponseWriter, r *http.Request) {
	sessions, err := s.st.ListSessions(r.Context(), false)
	if err != nil {
		writeJSON(w, 500, proto.APIError{Error: "internal error"})
		return
	}
	s.mu.Lock()
	n := len(s.conns)
	s.mu.Unlock()
	writeJSON(w, 200, proto.Status{Version: s.opt.Version, Proto: proto.Version, PID: os.Getpid(), StartedAt: s.started, Sessions: len(sessions), Agents: n})
}

func (s *Server) handleListSessions(w http.ResponseWriter, r *http.Request) {
	all := r.URL.Query().Get("all") == "1"
	sessions, err := s.st.ListSessions(r.Context(), all)
	if err != nil {
		writeJSON(w, 500, proto.APIError{Error: "internal error"})
		return
	}
	out := []proto.SessionInfo{}
	for _, sess := range sessions {
		info, err := s.sessionInfo(r.Context(), sess, all)
		if err != nil {
			writeJSON(w, 500, proto.APIError{Error: "internal error"})
			return
		}
		out = append(out, info)
	}
	writeJSON(w, 200, out)
}

func (s *Server) handleCreateSession(w http.ResponseWriter, r *http.Request) {
	var req proto.CreateSessionRequest
	if r.Body != nil {
		_ = json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&req)
	}
	if len(req.Name) > 64 {
		writeJSON(w, 400, proto.APIError{Error: "session name too long (max 64)"})
		return
	}
	sess, err := s.st.CreateSession(r.Context(), "shared", req.Name)
	if err != nil {
		writeJSON(w, 500, proto.APIError{Error: "internal error"})
		return
	}
	info, _ := s.sessionInfo(r.Context(), sess, false)
	writeJSON(w, 201, info)
}

func (s *Server) handleGetSession(w http.ResponseWriter, r *http.Request) {
	sess, err := s.st.GetSession(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, 404, proto.APIError{Error: "no such session"})
		return
	} else if err != nil {
		writeJSON(w, 500, proto.APIError{Error: "internal error"})
		return
	}
	info, err := s.sessionInfo(r.Context(), sess, r.URL.Query().Get("all") == "1")
	if err != nil {
		writeJSON(w, 500, proto.APIError{Error: "internal error"})
		return
	}
	writeJSON(w, 200, info)
}

func (s *Server) handleEndSession(w http.ResponseWriter, r *http.Request) {
	err := s.st.EndSession(r.Context(), r.PathValue("id"))
	if errors.Is(err, store.ErrNotFound) {
		writeJSON(w, 404, proto.APIError{Error: "no such session"})
		return
	} else if err != nil {
		writeJSON(w, 500, proto.APIError{Error: "internal error"})
		return
	}
	writeJSON(w, 200, map[string]string{"status": "ended"})
}

// stripControl removes control characters from a string that came from a peer.
func stripControl(s string) string {
	return strings.Map(func(r rune) rune {
		if r < 0x20 || r == 0x7f {
			return -1
		}
		return r
	}, s)
}

// enter registers one unit of in-flight work that Shutdown will wait for. It
// reports false once the server is closing.
func (s *Server) enter() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.closing {
		return false
	}
	s.wg.Add(1)
	return true
}

// spawn runs fn in a goroutine that Shutdown waits for (so nothing touches the
// store after it is closed). It reports false, without running fn, once the
// server is closing.
func (s *Server) spawn(fn func()) bool {
	if !s.enter() {
		return false
	}
	go func() {
		defer s.wg.Done()
		fn()
	}()
	return true
}
