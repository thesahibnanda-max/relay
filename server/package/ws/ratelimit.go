package ws

import (
	"sync"
	"time"
)

// rateLimiter is a small sliding-window counter: allow reports whether one
// more event may happen right now, recording it if so. Cheap enough for a
// free-tier VM - a per-instance slice of timestamps pruned on read, no
// background goroutine.
type rateLimiter struct {
	mu     sync.Mutex
	limit  int
	window time.Duration
	hits   []time.Time
}

func newRateLimiter(limit int, window time.Duration) *rateLimiter {
	return &rateLimiter{limit: limit, window: window}
}

func (r *rateLimiter) allow() bool {
	r.mu.Lock()
	defer r.mu.Unlock()
	cutoff := time.Now().Add(-r.window)
	kept := r.hits[:0]
	for _, t := range r.hits {
		if t.After(cutoff) {
			kept = append(kept, t)
		}
	}
	r.hits = kept
	if len(r.hits) >= r.limit {
		return false
	}
	r.hits = append(r.hits, time.Now())
	return true
}

// keyedLimiter is one rateLimiter per key (e.g. a sender id, or a
// sender|target pair), created lazily on first use. Entries are never
// evicted - acceptable at the scale a single small VM serves, matching the
// local daemon's own in-memory rate-limit maps.
type keyedLimiter struct {
	mu     sync.Mutex
	limit  int
	window time.Duration
	byKey  map[string]*rateLimiter
}

func newKeyedLimiter(limit int, window time.Duration) *keyedLimiter {
	return &keyedLimiter{limit: limit, window: window, byKey: map[string]*rateLimiter{}}
}

func (k *keyedLimiter) allow(key string) bool {
	k.mu.Lock()
	rl, ok := k.byKey[key]
	if !ok {
		rl = newRateLimiter(k.limit, k.window)
		k.byKey[key] = rl
	}
	k.mu.Unlock()
	return rl.allow()
}
