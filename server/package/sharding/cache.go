package sharding

import (
	"sync"
	"time"
)

// shardURLCache is an L1, in-memory cache of sessionID -> shard URL, sitting
// in front of the two Postgres round trips ShardURLFor would otherwise make
// on every call for an already-assigned session (GetBySessionID, then
// GetByID). Safe with no invalidation logic because the mapping it caches is
// genuinely immutable for a session's whole life (see ShardURLFor's and
// SessionShardMap's own doc comments) - the TTL is a defensive ceiling, not
// a correctness requirement.
//
// Mirrors ws/ratelimit.go's keyedLimiter shape: a lazily-populated map
// behind one plain sync.Mutex (never sync.RWMutex, matching every other
// shared-mutable-state type in this module), reached only through a
// pointer since a mutex must never be copied after first use.
type shardURLCache struct {
	mu      sync.Mutex
	ttl     time.Duration
	entries map[string]cacheEntry
}

type cacheEntry struct {
	url      string
	cachedAt time.Time
}

func newShardURLCache(ttl time.Duration) *shardURLCache {
	return &shardURLCache{ttl: ttl, entries: map[string]cacheEntry{}}
}

// get reports a cached URL for sessionID, if any and not yet past ttl. A
// ttl of zero (or negative) means every entry is always stale, so this
// always misses - a config-only way to disable the cache entirely, with no
// special-casing needed here.
func (c *shardURLCache) get(sessionID string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	entry, ok := c.entries[sessionID]
	if !ok {
		return "", false
	}
	if time.Since(entry.cachedAt) >= c.ttl {
		delete(c.entries, sessionID) // stale - drop it so the map doesn't grow unbounded with expired entries
		return "", false
	}
	return entry.url, true
}

func (c *shardURLCache) set(sessionID, url string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.entries[sessionID] = cacheEntry{url: url, cachedAt: time.Now()}
}
