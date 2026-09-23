// Package link is the agent's connection to the daemon. Events are numbered,
// buffered and sent in order; the daemon acknowledges what it has stored. On
// any disconnect the client reconnects (restarting the daemon if needed),
// resumes as the same agent, and resends whatever was not acknowledged.
//
// Send never blocks: the terminal path must not wait on the network.
package link

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math/rand/v2"
	"strconv"
	"sync"
	"time"

	"github.com/coder/websocket"

	"github.com/thesahibnanda-max/relay/internal/proto"
	"github.com/thesahibnanda-max/relay/internal/relayhome"
)

const (
	defaultMaxBuffered = 50000
	maxBatchEvents     = 256
	maxBatchBytes      = 512 << 10
	flushOnClose       = 3 * time.Second
)

type Options struct {
	Paths relayhome.Paths
	Hello proto.Hello // Session/Name/Tool/Role/...; Proto and Token are managed here
	Log   *slog.Logger

	// EnsureDaemon, if set, is called before each reconnect attempt so a
	// stopped daemon gets restarted. It should be cheap when one is running.
	EnsureDaemon func(ctx context.Context) error

	// OnDeliver is called (from the connection's reader, so it must not
	// block) for each message the daemon pushes. Delivery is at-least-once:
	// the same message can arrive again after a reconnect.
	OnDeliver func(proto.MessageView)
	// OnNotice is called when the daemon reports how many messages are held
	// for a human to approve on this agent. Must not block.
	OnNotice func(held int)

	MaxBuffered int           // events kept while disconnected (default 50000)
	BackoffMin  time.Duration // default 100ms
	BackoffMax  time.Duration // default 3s
}

type Client struct {
	opt Options
	log *slog.Logger

	mu       sync.Mutex
	cond     *sync.Cond
	buf      []proto.Event // unacked, ascending seq
	next     uint64        // next seq to assign
	acked    uint64
	sent     uint64 // highest seq written on the current connection
	ident    proto.Welcome
	token    string
	online   bool
	fatal    error
	closing  bool
	exitCode int
	dropped  uint64

	cur     *websocket.Conn // the live connection, nil while offline
	calls   map[string]chan proto.Result
	callSeq uint64

	ctx    context.Context
	cancel context.CancelFunc
	done   chan struct{}
}

// Connect performs the first connection and returns once the daemon has
// admitted the agent. Errors from the daemon come back as *proto.Error.
func Connect(ctx context.Context, opt Options) (*Client, error) {
	if opt.Log == nil {
		opt.Log = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	if opt.MaxBuffered == 0 {
		opt.MaxBuffered = defaultMaxBuffered
	}
	if opt.BackoffMin == 0 {
		opt.BackoffMin = 100 * time.Millisecond
	}
	if opt.BackoffMax == 0 {
		opt.BackoffMax = 3 * time.Second
	}
	c := &Client{opt: opt, log: opt.Log, next: 1, done: make(chan struct{}), calls: map[string]chan proto.Result{}}
	c.cond = sync.NewCond(&c.mu)
	c.ctx, c.cancel = context.WithCancel(context.Background())
	// Seed the token this Client should keep presenting on its own future
	// reconnects. adopt() only overwrites it when a Welcome carries one, and
	// a resume's Welcome never does (Token is only set on first
	// registration) - without this, a Client whose very first Hello was
	// itself a resume would forget the token the moment it landed, and its
	// next automatic reconnect would go out empty, read by the daemon as a
	// brand new registration colliding on the still-occupied name.
	c.token = opt.Hello.Token

	hctx, hcancel := context.WithTimeout(ctx, 10*time.Second)
	defer hcancel()
	ws, w, err := c.dialAndHello(hctx, opt.Hello)
	if err != nil {
		c.cancel()
		return nil, err
	}
	c.adopt(w)
	go c.run(ws)
	return c, nil
}

func (c *Client) adopt(w proto.Welcome) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if w.Token != "" {
		c.token = w.Token
	}
	c.ident = w
	c.ident.Token = c.token
	c.trimLocked(w.AckedSeq)
	if c.next <= w.AckedSeq {
		c.next = w.AckedSeq + 1 // a new process resuming an agent continues its numbering
	}
	c.sent = w.AckedSeq
	c.online = true
}

// Identity returns the session/agent the daemon assigned (including the
// resume token, which callers may persist).
func (c *Client) Identity() proto.Welcome {
	if c == nil {
		return proto.Welcome{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.ident
}

func (c *Client) Online() bool {
	if c == nil {
		return false
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.online
}

// Err returns the reason the client gave up syncing, if it did.
func (c *Client) Err() error {
	if c == nil {
		return nil
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.fatal
}

// Dropped is how many events were discarded because the buffer overflowed
// while the daemon was unreachable.
func (c *Client) Dropped() uint64 {
	if c == nil {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.dropped
}

// Send queues an event (its Seq is assigned here). It never blocks. If the
// buffer is full, the oldest raw events are discarded first: structured events
// (start, resize, exit...) are small and precious.
func (c *Client) Send(ev proto.Event) {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.fatal != nil || c.closing {
		return
	}
	ev.Seq = c.next
	c.next++
	c.buf = append(c.buf, ev)
	if len(c.buf) > c.opt.MaxBuffered {
		c.evictLocked()
	}
	c.cond.Broadcast()
}

func (c *Client) evictLocked() {
	over := len(c.buf) - c.opt.MaxBuffered
	kept := c.buf[:0]
	for _, e := range c.buf {
		if over > 0 && e.IsRaw() {
			over--
			c.dropped++
			continue
		}
		kept = append(kept, e)
	}
	c.buf = kept
}

func (c *Client) trimLocked(acked uint64) {
	if acked > c.acked {
		c.acked = acked
	}
	i := 0
	for i < len(c.buf) && c.buf[i].Seq <= c.acked {
		i++
	}
	c.buf = append(c.buf[:0], c.buf[i:]...)
}

// Close flushes what it can (a few seconds at most), tells the daemon the
// tool exited, and stops. Events not acknowledged by then remain only in the
// local event log.
func (c *Client) Close(exitCode int) {
	if c == nil {
		return
	}
	c.mu.Lock()
	if c.closing {
		c.mu.Unlock()
		return
	}
	c.closing, c.exitCode = true, exitCode
	c.cond.Broadcast()
	c.mu.Unlock()

	select {
	case <-c.done:
	case <-time.After(flushOnClose):
		c.cancel()
		<-c.done
	}
}

func (c *Client) dialAndHello(ctx context.Context, h proto.Hello) (*websocket.Conn, proto.Welcome, error) {
	h.Proto = proto.Version
	ws, err := proto.DialAgent(ctx, c.opt.Paths.SocketPath())
	if err != nil {
		return nil, proto.Welcome{}, err
	}
	b, _ := proto.Marshal(proto.TypeHello, h)
	if err := ws.Write(ctx, websocket.MessageText, b); err != nil {
		ws.CloseNow()
		return nil, proto.Welcome{}, err
	}
	_, data, err := ws.Read(ctx)
	if err != nil {
		ws.CloseNow()
		return nil, proto.Welcome{}, err
	}
	env, err := proto.Unmarshal(data)
	if err != nil {
		ws.CloseNow()
		return nil, proto.Welcome{}, err
	}
	switch env.Type {
	case proto.TypeWelcome:
		var w proto.Welcome
		if err := json.Unmarshal(env.Payload, &w); err != nil {
			ws.CloseNow()
			return nil, proto.Welcome{}, err
		}
		return ws, w, nil
	case proto.TypeError:
		var pe proto.Error
		_ = json.Unmarshal(env.Payload, &pe)
		ws.CloseNow()
		return nil, proto.Welcome{}, &pe
	}
	ws.CloseNow()
	return nil, proto.Welcome{}, errors.New("unexpected reply to hello: " + env.Type)
}

// run owns the connection lifecycle until Close or a fatal error.
func (c *Client) run(ws *websocket.Conn) {
	defer close(c.done)
	backoff := c.opt.BackoffMin
	for {
		graceful := c.serve(ws) // returns when the connection ends
		ws = nil
		c.mu.Lock()
		c.online = false
		stop := graceful || c.closing && len(c.buf) == 0 || c.fatal != nil
		c.mu.Unlock()
		if stop || c.ctx.Err() != nil {
			return
		}

		// reconnect with backoff, resuming as the same agent
		for ws == nil {
			if !c.sleep(jitter(backoff)) {
				return
			}
			if backoff *= 2; backoff > c.opt.BackoffMax {
				backoff = c.opt.BackoffMax
			}
			if c.opt.EnsureDaemon != nil {
				if err := c.opt.EnsureDaemon(c.ctx); err != nil {
					c.log.Debug("ensure daemon", "err", err)
					continue
				}
			}
			c.mu.Lock()
			h := c.opt.Hello
			h.Session, h.Name, h.Token = c.ident.Session.ID, c.ident.Agent.Name, c.token
			c.mu.Unlock()
			dctx, dcancel := context.WithTimeout(c.ctx, 5*time.Second)
			var w proto.Welcome
			var err error
			ws, w, err = c.dialAndHello(dctx, h)
			dcancel()
			if err != nil {
				var pe *proto.Error
				if errors.As(err, &pe) && definitive(pe.Code) {
					// The daemon will never accept us back (session ended,
					// token rejected...). Stop trying; local logging continues.
					c.mu.Lock()
					c.fatal = err
					c.buf = nil
					c.mu.Unlock()
					c.log.Warn("daemon refused resume; syncing stopped", "err", err)
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

// serve pumps events over one connection. It returns true if the agent said
// bye (shutdown complete), false if the connection failed.
func (c *Client) serve(ws *websocket.Conn) (graceful bool) {
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
			ch <- proto.Result{ID: id, Error: &proto.Error{Code: proto.CodeOffline, Message: "lost the connection to the relay daemon"}}
			delete(c.calls, id)
		}
		c.mu.Unlock()
	}()

	// reader: acks and errors
	readErr := make(chan error, 1)
	go func() {
		for {
			_, data, err := ws.Read(ctx)
			if err != nil {
				readErr <- err
				return
			}
			env, err := proto.Unmarshal(data)
			if err != nil {
				continue
			}
			switch env.Type {
			case proto.TypeAck:
				var a proto.Ack
				if json.Unmarshal(env.Payload, &a) == nil {
					c.mu.Lock()
					c.trimLocked(a.Seq)
					c.cond.Broadcast()
					c.mu.Unlock()
				}
			case proto.TypeDeliver:
				var d proto.Deliver
				if json.Unmarshal(env.Payload, &d) == nil && c.opt.OnDeliver != nil {
					c.opt.OnDeliver(d.Message)
				}
			case proto.TypeNotice:
				var n proto.Notice
				if json.Unmarshal(env.Payload, &n) == nil && c.opt.OnNotice != nil {
					c.opt.OnNotice(n.Held)
				}
			case proto.TypeResult:
				var r proto.Result
				if json.Unmarshal(env.Payload, &r) == nil {
					c.mu.Lock()
					if ch, ok := c.calls[r.ID]; ok {
						ch <- r
						delete(c.calls, r.ID)
					}
					c.mu.Unlock()
				}
			case proto.TypeError:
				var pe proto.Error
				_ = json.Unmarshal(env.Payload, &pe)
				readErr <- &pe
				return
			}
		}
	}()
	// wake the writer when the connection dies
	go func() {
		select {
		case <-readErr:
		case <-ctx.Done():
		}
		cancel()
		c.mu.Lock()
		c.cond.Broadcast()
		c.mu.Unlock()
	}()

	for {
		c.mu.Lock()
		for ctx.Err() == nil && !c.hasUnsentLocked() && !(c.closing && len(c.buf) == 0) {
			c.cond.Wait()
		}
		if ctx.Err() != nil {
			c.mu.Unlock()
			return false
		}
		if c.closing && len(c.buf) == 0 { // everything acked: say bye
			code := c.exitCode
			c.mu.Unlock()
			b, _ := proto.Marshal(proto.TypeBye, proto.Bye{ExitCode: code})
			wctx, wcancel := context.WithTimeout(ctx, 2*time.Second)
			err := ws.Write(wctx, websocket.MessageText, b)
			wcancel()
			if err == nil {
				ws.Close(websocket.StatusNormalClosure, "bye")
			}
			return true
		}
		batch := c.nextBatchLocked()
		c.mu.Unlock()

		b, err := proto.Marshal(proto.TypeEvents, proto.Events{Events: batch})
		if err != nil {
			continue
		}
		wctx, wcancel := context.WithTimeout(ctx, 10*time.Second)
		err = ws.Write(wctx, websocket.MessageText, b)
		wcancel()
		if err != nil {
			return false
		}
		c.mu.Lock()
		if last := batch[len(batch)-1].Seq; last > c.sent {
			c.sent = last
		}
		c.mu.Unlock()
	}
}

func (c *Client) hasUnsentLocked() bool {
	return len(c.buf) > 0 && c.buf[len(c.buf)-1].Seq > c.sent
}

func (c *Client) nextBatchLocked() []proto.Event {
	var batch []proto.Event
	size := 0
	for _, e := range c.buf {
		if e.Seq <= c.sent {
			continue
		}
		if len(batch) >= maxBatchEvents || (len(batch) > 0 && size+len(e.B)*2 > maxBatchBytes) {
			break
		}
		batch = append(batch, e)
		size += len(e.B)*2 + 64
	}
	return batch
}

// Call performs a request/response operation on the daemon and decodes the
// result into out (which may be nil). Daemon-side failures come back as
// *proto.Error. If the connection is down it waits for the reconnect until ctx
// ends (at most 10 s when ctx has no deadline).
func (c *Client) Call(ctx context.Context, op string, args, out any) error {
	if c == nil {
		return &proto.Error{Code: proto.CodeOffline, Message: "not connected to a relay session"}
	}
	if _, ok := ctx.Deadline(); !ok {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 10*time.Second)
		defer cancel()
	}
	raw, err := json.Marshal(args)
	if err != nil {
		return err
	}
	internalRetries := 0
	for {
		c.mu.Lock()
		ws, fatal := c.cur, c.fatal
		var ch chan proto.Result
		var id string
		if ws != nil {
			c.callSeq++
			id = "c" + strconv.FormatUint(c.callSeq, 10)
			ch = make(chan proto.Result, 1)
			c.calls[id] = ch
		}
		c.mu.Unlock()
		if fatal != nil {
			return &proto.Error{Code: proto.CodeOffline, Message: "the relay daemon refused this agent: " + fatal.Error()}
		}
		if ws == nil {
			select {
			case <-ctx.Done():
				return &proto.Error{Code: proto.CodeOffline, Message: "the relay daemon is unreachable"}
			case <-time.After(50 * time.Millisecond):
				continue
			}
		}

		b, _ := proto.Marshal(proto.TypeRPC, proto.RPC{ID: id, Op: op, Args: raw})
		if err := ws.Write(ctx, websocket.MessageText, b); err != nil {
			c.mu.Lock()
			delete(c.calls, id)
			c.mu.Unlock()
			select {
			case <-ctx.Done():
				return &proto.Error{Code: proto.CodeOffline, Message: "the relay daemon is unreachable"}
			case <-time.After(50 * time.Millisecond):
				continue
			}
		}
		select {
		case r := <-ch:
			if r.Error != nil && r.Error.Code == proto.CodeOffline && ctx.Err() == nil {
				continue // the connection dropped mid-request: try again on the new one
			}
			if r.Error != nil && r.Error.Code == proto.CodeInternal && internalRetries < 3 && ctx.Err() == nil {
				internalRetries++ // a hiccup inside the daemon (e.g. it is restarting); the operations are safe to repeat
				select {
				case <-ctx.Done():
				case <-time.After(time.Duration(internalRetries) * 100 * time.Millisecond):
				}
				continue
			}
			if r.Error != nil {
				return r.Error
			}
			if out != nil && len(r.Result) > 0 {
				return json.Unmarshal(r.Result, out)
			}
			return nil
		case <-ctx.Done():
			c.mu.Lock()
			delete(c.calls, id)
			c.mu.Unlock()
			return &proto.Error{Code: proto.CodeOffline, Message: "timed out waiting for the relay daemon"}
		}
	}
}

// definitive reports whether a refusal from the daemon will never change, so
// retrying is pointless. Anything else (an internal error while the daemon
// restarts, an agent-still-live race with our own old connection) is
// transient: giving up on those would silently cut an agent off for good.
func definitive(code string) bool {
	switch code {
	case proto.CodeBadToken, proto.CodeSessionEnded, proto.CodeSessionNotFound, proto.CodeProtoMismatch,
		proto.CodeBadName, proto.CodeNameTaken, proto.CodeBadRequest, proto.CodeSessionFull:
		return true
	}
	return false
}
