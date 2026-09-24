package ws

import (
	"context"
	"encoding/json"
	"sync"

	"github.com/coder/websocket"
)

// conn wraps one agent's live WebSocket connection with its own write mutex.
// Once replay-on-connect and the async "wait" RPC exist, more than one
// goroutine can want to write to the same connection at once (a deliver push
// triggered by another agent's Send, a replay burst at connect time,
// pingLoop's Ping, an eventual "wait" rpc_result) - writes to a single
// *websocket.Conn must be serialized, which is what this mutex does. Reads
// are unaffected: only the connection's own owning goroutine (handshake,
// then the serve loop) ever reads from it.
type conn struct {
	ws *websocket.Conn
	mu sync.Mutex
}

func newConn(ws *websocket.Conn) *conn {
	return &conn{ws: ws}
}

// writeTyped marshals payload into typ's envelope and writes it, holding the
// connection's write lock for the duration.
func (c *conn) writeTyped(ctx context.Context, typ string, payload any) error {
	env, err := marshal(typ, payload)
	if err != nil {
		return err
	}
	b, err := json.Marshal(env)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	wctx, cancel := context.WithTimeout(ctx, writeTimeout)
	defer cancel()
	return c.ws.Write(wctx, websocket.MessageText, b)
}

// registry tracks every currently-connected agent's live conn, keyed by
// agent id, so Send can push a Deliver frame immediately when the target is
// online. It is always reached through a pointer (see hub's registry
// field): a sync.Mutex must never be copied after first use, and the rest
// of this module's types are deliberately value types, so this is the one
// piece of shared mutable state that has to live behind a pointer - not a
// stylistic choice, a correctness requirement.
type registry struct {
	mu    sync.Mutex
	conns map[string]*conn
}

func newRegistry() *registry {
	return &registry{conns: map[string]*conn{}}
}

func (r *registry) set(agentID string, c *conn) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.conns[agentID] = c
}

func (r *registry) remove(agentID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.conns, agentID)
}

func (r *registry) get(agentID string) (*conn, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.conns[agentID]
	return c, ok
}
