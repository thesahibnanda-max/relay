// Package globallink is an agent's connection to a global (multi-machine)
// session server, over a real WebSocket - the sibling of internal/link,
// which connects to the local daemon over a unix socket instead. Both
// satisfy internal/collab.Link identically, so Session and HandleCtl never
// know or care which one they're talking to; internal/cli's connect() is
// the only place that decides.
//
// Unlike internal/link, this client never buffers or replays raw terminal
// events: the global server has no equivalent of internal/proto's Events/Ack
// frames, since Phase 1 never sends it any (Send is a deliberate no-op) -
// see the project plan for why relay_get_context means something different
// (recent message history, not a transcript) for a global session.
package globallink

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"math/rand/v2"
	"os"
	"strconv"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/thesahibnanda-max/relay/internal/globalid"
	"github.com/thesahibnanda-max/relay/internal/proto"
)

// wsPath matches server/package/ws.Path.
const wsPath = "/v1/agent"

// tlsEnvVar, if set to any non-empty value, switches the dial scheme from
// ws:// to wss:// - for the rare case of a TLS-terminating reverse proxy in
// front of the server. Phase 1 has no other TLS story (see the project
// plan's risks section).
const tlsEnvVar = "RELAY_GLOBAL_TLS"

type Options struct {
	HostPort string // "host:port" to dial
	// Hello carries Session/Name/Token/Tool/Role - the same proto.Hello an
	// internal/link connection uses, so internal/cli builds one Hello value
	// the same way for both paths. Every other field (PID, Cwd,
	// ApproveInbound, CanInterrupt, CanBroadcast, Proto, Client) is
	// local-daemon-only and ignored here.
	Hello proto.Hello

	// OnDeliver is called (from the connection's reader, so it must not
	// block) for each message the server pushes. Delivery is at-least-once:
	// the same message can arrive again after a reconnect, but
	// internal/bus's own dedup-by-id makes that safe.
	OnDeliver func(proto.MessageView)
	// OnNotice is never called by a Phase 1 server (no --approve-inbound
	// yet) - kept so Phase 2 needs no client-side signature change.
	OnNotice func(held int)

	BackoffMin time.Duration // default 100ms
	BackoffMax time.Duration // default 3s
}

type Client struct {
	opt Options

	mu          sync.Mutex
	ident       proto.Welcome
	sessionULID string // bare ULID, for reconnect Hellos - never the full host-embedded token
	token       string
	online      bool
	fatal       error
	closing     bool

	cur     *websocket.Conn // the live connection, nil while offline
	calls   map[string]chan rpcResultFrame
	callSeq uint64

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
}

// Connect performs the first connection and returns once the server has
// admitted the agent. Errors from the server come back as *proto.Error, the
// same type internal/link.Connect uses, so callers like
// internal/cli's friendly() work identically for both.
func Connect(ctx context.Context, opt Options) (*Client, error) {
	if opt.BackoffMin == 0 {
		opt.BackoffMin = 100 * time.Millisecond
	}
	if opt.BackoffMax == 0 {
		opt.BackoffMax = 3 * time.Second
	}
	c := &Client{opt: opt, done: make(chan struct{}), calls: map[string]chan rpcResultFrame{}}
	c.ctx, c.cancel = context.WithCancel(context.Background())
	// Seed the token this Client should keep presenting on its own future
	// reconnects - a resume's Welcome never carries one (only first
	// registration does), the same reasoning internal/link.Connect documents.
	c.token = opt.Hello.Token

	hctx, hcancel := context.WithTimeout(ctx, 10*time.Second)
	defer hcancel()
	ws, w, err := c.dialAndHello(hctx, helloFrame{
		Session: opt.Hello.Session, Name: opt.Hello.Name, Token: opt.Hello.Token,
		Tool: opt.Hello.Tool, Role: opt.Hello.Role,
	})
	if err != nil {
		c.cancel()
		return nil, err
	}
	c.adopt(w)
	go c.run(ws)
	return c, nil
}

func (c *Client) adopt(w welcomeFrame) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if w.Token != "" {
		c.token = w.Token
	}
	c.sessionULID = w.SessionID
	c.ident = proto.Welcome{
		Session: proto.SessionRef{
			ID:   globalid.Token{ULID: w.SessionID, HostPort: c.opt.HostPort}.String(),
			Kind: "shared",
		},
		Agent:   proto.AgentRef{ID: w.AgentID, Name: w.Name, Tool: c.opt.Hello.Tool, Role: c.opt.Hello.Role},
		Token:   c.token,
		Resumed: w.Resumed,
	}
	c.online = true
}

// Identity returns the session/agent the server assigned, with Session.ID
// set to the full shareable token ("<ulid>@host:port") so the existing
// "others join with" banner prints something joinable with no extra changes.
func (c *Client) Identity() proto.Welcome {
	if c == nil {
		return proto.Welcome{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ident
}

// Send is a deliberate no-op: the global server never receives raw terminal
// bytes from any agent. See the package doc for why.
func (c *Client) Send(ev proto.Event) {}

// Close disconnects. There is no local-daemon-style Bye/exit-code frame in
// the Phase 1 wire protocol - the server only cares that the connection
// closed, which triggers its own Disconnect bookkeeping.
func (c *Client) Close(exitCode int) {
	if c == nil {
		return
	}
	c.mu.Lock()
	if c.closing {
		c.mu.Unlock()
		return
	}
	c.closing = true
	c.mu.Unlock()
	c.cancel()
	<-c.done
}

func (c *Client) dialURL() string {
	scheme := "ws"
	if os.Getenv(tlsEnvVar) != "" {
		scheme = "wss"
	}
	return scheme + "://" + c.opt.HostPort + wsPath
}

func (c *Client) dialAndHello(ctx context.Context, h helloFrame) (*websocket.Conn, welcomeFrame, error) {
	ws, _, err := websocket.Dial(ctx, c.dialURL(), nil)
	if err != nil {
		return nil, welcomeFrame{}, err
	}
	ws.SetReadLimit(4 << 20)
	b, err := marshalEnvelope(typeHello, h)
	if err != nil {
		ws.CloseNow()
		return nil, welcomeFrame{}, err
	}
	if err := ws.Write(ctx, websocket.MessageText, b); err != nil {
		ws.CloseNow()
		return nil, welcomeFrame{}, err
	}
	_, data, err := ws.Read(ctx)
	if err != nil {
		ws.CloseNow()
		return nil, welcomeFrame{}, err
	}
	var env envelope
	if err := json.Unmarshal(data, &env); err != nil {
		ws.CloseNow()
		return nil, welcomeFrame{}, err
	}
	switch env.Type {
	case typeWelcome:
		var w welcomeFrame
		if err := json.Unmarshal(env.Payload, &w); err != nil {
			ws.CloseNow()
			return nil, welcomeFrame{}, err
		}
		return ws, w, nil
	case typeError:
		var e wireError
		_ = json.Unmarshal(env.Payload, &e)
		ws.CloseNow()
		return nil, welcomeFrame{}, &proto.Error{Code: e.Code, Message: e.Message}
	}
	ws.CloseNow()
	return nil, welcomeFrame{}, fmt.Errorf("unexpected reply to hello: %s", env.Type)
}

// run owns the connection lifecycle until Close or a fatal (definitive)
// refusal - mirrors internal/link.Client.run's reconnect-with-backoff shape.
func (c *Client) run(ws *websocket.Conn) {
	defer close(c.done)
	backoff := c.opt.BackoffMin
	for {
		c.serve(ws)
		ws = nil
		c.mu.Lock()
		c.online = false
		stop := c.closing || c.fatal != nil
		c.mu.Unlock()
		if stop || c.ctx.Err() != nil {
			return
		}

		for ws == nil {
			if !c.sleep(jitter(backoff)) {
				return
			}
			if backoff *= 2; backoff > c.opt.BackoffMax {
				backoff = c.opt.BackoffMax
			}
			c.mu.Lock()
			h := helloFrame{Session: c.sessionULID, Name: c.opt.Hello.Name, Token: c.token, Tool: c.opt.Hello.Tool, Role: c.opt.Hello.Role}
			c.mu.Unlock()
			dctx, dcancel := context.WithTimeout(c.ctx, 5*time.Second)
			var w welcomeFrame
			var err error
			ws, w, err = c.dialAndHello(dctx, h)
			dcancel()
			if err != nil {
				var pe *proto.Error
				if errors.As(err, &pe) && definitive(pe.Code) {
					// The server will never accept us back (session ended,
					// token rejected...). Stop trying.
					c.mu.Lock()
					c.fatal = err
					c.mu.Unlock()
					return
				}
				ws = nil
				continue
			}
			c.adopt(w)
			backoff = c.opt.BackoffMin
		}
	}
}

func (c *Client) sleep(d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-c.ctx.Done():
		return false
	case <-t.C:
		return true
	}
}

func jitter(d time.Duration) time.Duration {
	return d/2 + time.Duration(rand.Int64N(int64(d/2)+1))
}

// definitive reports whether a refusal from the server will never change, so
// retrying is pointless - mirrors internal/link's own definitive().
func definitive(code string) bool {
	switch code {
	case proto.CodeBadToken, proto.CodeSessionEnded, proto.CodeSessionNotFound, proto.CodeNameTaken, proto.CodeBadRequest:
		return true
	}
	return false
}

// serve pumps frames over one connection until it drops.
func (c *Client) serve(ws *websocket.Conn) {
	ctx, cancel := context.WithCancel(c.ctx)
	defer cancel()
	defer ws.CloseNow()
	c.mu.Lock()
	c.cur = ws
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		if c.cur == ws {
			c.cur = nil
		}
		for id, ch := range c.calls { // requests in flight die with the connection
			ch <- rpcResultFrame{ID: id, Error: &wireError{Code: proto.CodeOffline, Message: "lost the connection to the relay server"}}
			delete(c.calls, id)
		}
		c.mu.Unlock()
	}()

	for {
		_, data, err := ws.Read(ctx)
		if err != nil {
			return
		}
		var env envelope
		if err := json.Unmarshal(data, &env); err != nil {
			continue
		}
		switch env.Type {
		case typeDeliver:
			var d deliverFrame
			if json.Unmarshal(env.Payload, &d) == nil {
				if c.opt.OnDeliver != nil {
					c.opt.OnDeliver(toProtoMessageView(d.Message))
				}
				c.sendAck(ctx, d.Message.ID)
			}
		case typeRPCResult:
			var r rpcResultFrame
			if json.Unmarshal(env.Payload, &r) == nil {
				c.mu.Lock()
				if ch, ok := c.calls[r.ID]; ok {
					ch <- r
					delete(c.calls, r.ID)
				}
				c.mu.Unlock()
			}
		case typeError:
			return
		}
	}
}

func (c *Client) sendAck(ctx context.Context, messageID string) {
	c.mu.Lock()
	ws := c.cur
	c.mu.Unlock()
	if ws == nil {
		return
	}
	b, err := marshalEnvelope(typeAck, ackFrame{ID: messageID})
	if err != nil {
		return
	}
	wctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	_ = ws.Write(wctx, websocket.MessageText, b)
}

func toProtoMessageView(m messageView) proto.MessageView {
	return proto.MessageView{
		ID: m.ID, From: m.From, To: m.To, Kind: m.Kind, Priority: m.Priority,
		ReplyTo: m.ReplyTo, Body: m.Body, Hops: m.Hops, State: m.State, CreatedAt: m.CreatedAt,
	}
}

// Call performs a request/response operation against the global session
// server, translating internal/proto's request/result types (the same ones
// internal/link.Client accepts) into and out of this package's own wire
// shapes - Session and HandleCtl never see the difference.
func (c *Client) Call(ctx context.Context, op string, args, out any) error {
	if c == nil {
		return &proto.Error{Code: proto.CodeOffline, Message: "not connected to a relay session"}
	}
	switch op {
	case proto.OpSend:
		return c.callSend(ctx, args, out)
	case proto.OpListAgents:
		return c.callListAgents(ctx, out)
	case proto.OpContext:
		return c.callContext(ctx, args, out)
	case proto.OpWait:
		return c.callWait(ctx, args, out)
	default:
		// approve/reject/agent_state/msg_state: not supported by a Phase 1
		// global session server yet - see the project plan's Phase 2 table.
		// CodeBadRequest (not Offline/Internal) makes callers like
		// collab's sessionEnv.Report silently swallow this instead of
		// treating it as a retryable failure.
		return &proto.Error{Code: proto.CodeBadRequest, Message: "relay: " + op + " is not supported for global sessions yet"}
	}
}

func (c *Client) callSend(ctx context.Context, args, out any) error {
	a, ok := args.(proto.SendArgs)
	if !ok {
		return fmt.Errorf("globallink: unexpected args type %T for send", args)
	}
	raw, err := c.doRPC(ctx, opSend, sendArgs{To: a.To, Body: a.Body, Kind: a.Kind, Priority: a.Priority, ReplyTo: a.ReplyTo})
	if err != nil {
		return err
	}
	var r sendResult
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &r); err != nil {
			return err
		}
	}
	if out != nil {
		res, ok := out.(*proto.SendResult)
		if !ok {
			return fmt.Errorf("globallink: unexpected out type %T for send", out)
		}
		*res = proto.SendResult{ID: r.ID, To: []string{a.To}, State: r.State}
	}
	return nil
}

func (c *Client) callListAgents(ctx context.Context, out any) error {
	raw, err := c.doRPC(ctx, opListAgents, struct{}{})
	if err != nil {
		return err
	}
	var r listAgentsResult
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &r); err != nil {
			return err
		}
	}
	if out != nil {
		res, ok := out.(*proto.ListAgentsResult)
		if !ok {
			return fmt.Errorf("globallink: unexpected out type %T for list_agents", out)
		}
		id := c.Identity()
		peers := make([]proto.PeerInfo, len(r.Agents))
		for i, a := range r.Agents {
			peers[i] = proto.PeerInfo{Name: a.Name, Role: a.Role, Tool: a.Tool, Status: a.Status, Self: a.Name == id.Agent.Name}
		}
		*res = proto.ListAgentsResult{Session: id.Session.ID, Agents: peers}
	}
	return nil
}

func (c *Client) callContext(ctx context.Context, args, out any) error {
	a, ok := args.(proto.ContextArgs)
	if !ok {
		return fmt.Errorf("globallink: unexpected args type %T for get_context", args)
	}
	raw, err := c.doRPC(ctx, opContext, contextArgs{Agent: a.Agent, N: a.N})
	if err != nil {
		return err
	}
	var r getContextResult
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &r); err != nil {
			return err
		}
	}
	if out != nil {
		res, ok := out.(*proto.ContextResult)
		if !ok {
			return fmt.Errorf("globallink: unexpected out type %T for get_context", out)
		}
		turns := make([]proto.TurnView, len(r.Messages))
		for i, m := range r.Messages {
			turns[i] = proto.TurnView{TS: m.CreatedAt, Role: "message", Text: fmt.Sprintf("%s -> %s: %s", m.From, m.To, m.Body)}
		}
		*res = proto.ContextResult{Agent: r.Agent, Mode: "tail", Turns: turns}
	}
	return nil
}

func (c *Client) callWait(ctx context.Context, args, out any) error {
	a, ok := args.(proto.WaitArgs)
	if !ok {
		return fmt.Errorf("globallink: unexpected args type %T for wait", args)
	}
	raw, err := c.doRPC(ctx, opWait, waitArgs{ID: a.ID, TimeoutS: a.TimeoutS})
	if err != nil {
		return err
	}
	var r waitResult
	if len(raw) > 0 {
		if err := json.Unmarshal(raw, &r); err != nil {
			return err
		}
	}
	if out != nil {
		res, ok := out.(*proto.WaitResult)
		if !ok {
			return fmt.Errorf("globallink: unexpected out type %T for wait", out)
		}
		wr := proto.WaitResult{ID: r.ID, State: r.State, TimedOut: r.TimedOut}
		if r.Reply != nil {
			reply := toProtoMessageView(*r.Reply)
			wr.Reply = &reply
		}
		*res = wr
	}
	return nil
}

// doRPC sends one rpc frame and waits for its matching rpc_result, waiting
// out any reconnect if the connection is currently down - mirrors
// internal/link.Client.Call's own request/response mechanics.
func (c *Client) doRPC(ctx context.Context, op string, args any) (json.RawMessage, error) {
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
	}
	raw, err := json.Marshal(args)
	if err != nil {
		return nil, err
	}
	for {
		c.mu.Lock()
		ws, fatal := c.cur, c.fatal
		var ch chan rpcResultFrame
		var id string
		if ws != nil {
			c.callSeq++
			id = strconv.FormatUint(c.callSeq, 10)
			ch = make(chan rpcResultFrame, 1)
			c.calls[id] = ch
		}
		c.mu.Unlock()
		if fatal != nil {
			return nil, &proto.Error{Code: proto.CodeOffline, Message: "the relay server refused this agent: " + fatal.Error()}
		}
		if ws == nil {
			select {
			case <-ctx.Done():
				return nil, &proto.Error{Code: proto.CodeOffline, Message: "the relay server is unreachable"}
			case <-time.After(50 * time.Millisecond):
				continue
			}
		}

		b, err := marshalEnvelope(typeRPC, rpcFrame{ID: id, Op: op, Args: raw})
		if err != nil {
			c.mu.Lock()
			delete(c.calls, id)
			c.mu.Unlock()
			return nil, err
		}
		if err := ws.Write(ctx, websocket.MessageText, b); err != nil {
			c.mu.Lock()
			delete(c.calls, id)
			c.mu.Unlock()
			select {
			case <-ctx.Done():
				return nil, &proto.Error{Code: proto.CodeOffline, Message: "the relay server is unreachable"}
			case <-time.After(50 * time.Millisecond):
				continue
			}
		}
		select {
		case r := <-ch:
			if r.Error != nil && r.Error.Code == proto.CodeOffline && ctx.Err() == nil {
				continue // the connection dropped mid-request: try again on the new one
			}
			if r.Error != nil {
				return nil, &proto.Error{Code: r.Error.Code, Message: r.Error.Message}
			}
			return r.Result, nil
		case <-ctx.Done():
			c.mu.Lock()
			delete(c.calls, id)
			c.mu.Unlock()
			return nil, &proto.Error{Code: proto.CodeOffline, Message: "timed out waiting for the relay server"}
		}
	}
}
