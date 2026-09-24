package ws

import "sync"

// liveStates tracks each connected agent's self-reported tool state
// (idle/busy/dialog/...) - deliberately in-memory only, never persisted to
// Mongo, mirroring the local daemon's own unpersisted liveState map exactly:
// the value is instantaneous, not historical, and stale the moment it's
// written, so losing it on a server restart is correct, not a gap.
type liveStates struct {
	mu sync.Mutex
	m  map[string]string
}

func newLiveStates() *liveStates {
	return &liveStates{m: map[string]string{}}
}

func (l *liveStates) set(agentID, state string) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.m[agentID] = state
}

// get returns "unknown" if agentID has never reported a state - including
// right after a server restart, matching the local daemon's own behavior
// before an agent's first agent_state report.
func (l *liveStates) get(agentID string) string {
	l.mu.Lock()
	defer l.mu.Unlock()
	if s, ok := l.m[agentID]; ok {
		return s
	}
	return "unknown"
}
