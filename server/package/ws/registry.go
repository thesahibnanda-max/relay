package ws

import (
	"sync"

	"github.com/coder/websocket"
)

// registry tracks every currently-connected agent's live socket, keyed by
// agent id, so Send can push a Deliver frame immediately when the target is
// online. It is always reached through a pointer (see hub's registry
// field): a sync.Mutex must never be copied after first use, and the rest
// of this module's types are deliberately value types, so this is the one
// piece of shared mutable state that has to live behind a pointer - not a
// stylistic choice, a correctness requirement.
type registry struct {
	mu    sync.Mutex
	conns map[string]*websocket.Conn
}

func newRegistry() *registry {
	return &registry{conns: map[string]*websocket.Conn{}}
}

func (r *registry) set(agentID string, c *websocket.Conn) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.conns[agentID] = c
}

func (r *registry) remove(agentID string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	delete(r.conns, agentID)
}

func (r *registry) get(agentID string) (*websocket.Conn, bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	c, ok := r.conns[agentID]
	return c, ok
}
