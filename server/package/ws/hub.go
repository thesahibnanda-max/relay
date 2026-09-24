package ws

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/coder/websocket"

	"github.com/thesahibnanda-max/relay/server/package/session"
)

// Timeouts mirror the local daemon's own defaults for the same events -
// values proven in production there, not re-derived from scratch.
const (
	helloTimeout = 5 * time.Second
	pingEvery    = 15 * time.Second
	pongTimeout  = 10 * time.Second
	writeTimeout = 10 * time.Second
	readLimit    = 4 << 20 // 4 MiB
)

// Interface accepts one WebSocket connection and drives it end to end.
type Interface interface {
	Accept(w http.ResponseWriter, r *http.Request)
}

type hub struct {
	session session.Interface
	reg     *registry
}

func New(sessionService session.Interface) (Interface, error) {
	if sessionService == nil {
		return nil, errors.New("ws: session service is nil")
	}
	return hub{session: sessionService, reg: newRegistry()}, nil
}

func (h hub) Accept(w http.ResponseWriter, r *http.Request) {
	// InsecureSkipVerify accepts a connection regardless of Origin - this is
	// what "allow all origins" means for a WebSocket upgrade, which plain
	// CORS response headers don't govern the same way they govern a normal
	// HTTP request (see http.go for the REST-endpoint CORS middleware).
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}
	defer c.CloseNow()
	c.SetReadLimit(readLimit)

	ctx := r.Context()
	result, ok := h.handshake(ctx, c)
	if !ok {
		return
	}

	h.reg.set(result.AgentID, c)
	defer h.reg.remove(result.AgentID)

	pingCtx, stopPing := context.WithCancel(ctx)
	defer stopPing()
	go h.pingLoop(pingCtx, c)

	h.serve(ctx, c, result)
	c.Close(websocket.StatusNormalClosure, "bye")
}

// handshake reads the one Hello frame a connection must open with, joins
// the session it names, and replies with a Welcome. ok is false if the
// connection should be torn down (a protocol violation or a failed join,
// either way an Error frame has already been sent).
func (h hub) handshake(ctx context.Context, c *websocket.Conn) (session.JoinResult, bool) {
	hctx, cancel := context.WithTimeout(ctx, helloTimeout)
	env, err := readEnvelope(hctx, c)
	cancel()
	if err != nil {
		return session.JoinResult{}, false
	}
	if env.Type != TypeHello {
		reject(ctx, c, "bad_request", "expected hello")
		return session.JoinResult{}, false
	}
	var hello Hello
	if err := json.Unmarshal(env.Payload, &hello); err != nil {
		reject(ctx, c, "bad_request", "malformed hello")
		return session.JoinResult{}, false
	}

	result, err := h.session.Join(ctx, session.JoinRequest{
		SessionID: hello.Session, Name: hello.Name, Token: hello.Token, Tool: hello.Tool, Role: hello.Role,
	})
	if err != nil {
		reject(ctx, c, "join_failed", err.Error())
		return session.JoinResult{}, false
	}

	if err := writeTyped(ctx, c, TypeWelcome, Welcome{
		SessionID: result.SessionID, AgentID: result.AgentID, Name: result.Name,
		Token: result.Token, Resumed: result.Resumed,
	}); err != nil {
		return session.JoinResult{}, false
	}
	return result, true
}

// serve handles every frame after the handshake until the connection closes.
func (h hub) serve(ctx context.Context, c *websocket.Conn, me session.JoinResult) {
	for {
		env, err := readEnvelope(ctx, c)
		if err != nil {
			return
		}
		switch env.Type {
		case TypeSend:
			h.handleSend(ctx, c, me, env.Payload)
		case TypeListAgents:
			h.handleListAgents(ctx, c, me)
		default:
			_ = writeTyped(ctx, c, TypeError, Error{Code: "bad_request", Message: "unknown frame type"})
		}
	}
}

func (h hub) handleSend(ctx context.Context, c *websocket.Conn, me session.JoinResult, payload json.RawMessage) {
	var req Send
	if err := json.Unmarshal(payload, &req); err != nil {
		_ = writeTyped(ctx, c, TypeError, Error{Code: "bad_request", Message: "malformed send"})
		return
	}
	messageID, targetID, err := h.session.Send(ctx, me.SessionID, me.AgentID, req.To, req.Body)
	if err != nil {
		_ = writeTyped(ctx, c, TypeError, Error{Code: "send_failed", Message: err.Error()})
		return
	}
	_ = writeTyped(ctx, c, TypeSendResult, SendResult{ID: messageID})
	if target, online := h.reg.get(targetID); online {
		_ = writeTyped(ctx, target, TypeDeliver, Deliver{ID: messageID, From: me.Name, Body: req.Body})
	}
}

func (h hub) handleListAgents(ctx context.Context, c *websocket.Conn, me session.JoinResult) {
	agents, err := h.session.ListAgents(ctx, me.SessionID)
	if err != nil {
		_ = writeTyped(ctx, c, TypeError, Error{Code: "list_failed", Message: err.Error()})
		return
	}
	out := make([]AgentInfo, len(agents))
	for i, a := range agents {
		out[i] = AgentInfo{ID: a.ID, Name: a.Name, Tool: a.Tool, Role: a.Role, Status: a.Status}
	}
	_ = writeTyped(ctx, c, TypeAgentsList, ListAgentsResult{Agents: out})
}

// pingLoop is the server-initiated liveness check: if a ping goes
// unanswered within pongTimeout, the connection is torn down.
func (h hub) pingLoop(ctx context.Context, c *websocket.Conn) {
	ticker := time.NewTicker(pingEvery)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			pctx, cancel := context.WithTimeout(ctx, pongTimeout)
			err := c.Ping(pctx)
			cancel()
			if err != nil {
				c.CloseNow()
				return
			}
		}
	}
}

func readEnvelope(ctx context.Context, c *websocket.Conn) (Envelope, error) {
	_, data, err := c.Read(ctx)
	if err != nil {
		return Envelope{}, err
	}
	var env Envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return Envelope{}, err
	}
	return env, nil
}

func writeTyped(ctx context.Context, c *websocket.Conn, typ string, payload any) error {
	env, err := marshal(typ, payload)
	if err != nil {
		return err
	}
	b, err := json.Marshal(env)
	if err != nil {
		return err
	}
	wctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	return c.Write(wctx, websocket.MessageText, b)
}

// reject sends a structured error frame, then closes with PolicyViolation -
// the same two-step convention (readable error before the close frame) the
// local daemon already uses.
func reject(ctx context.Context, c *websocket.Conn, code, message string) {
	_ = writeTyped(ctx, c, TypeError, Error{Code: code, Message: message})
	c.Close(websocket.StatusPolicyViolation, code)
}
