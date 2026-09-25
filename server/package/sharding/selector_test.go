package sharding

import (
	"context"
	"sync"
	"testing"
	"time"

	"github.com/thesahibnanda-max/relay/server/package/config"
	"github.com/thesahibnanda-max/relay/server/package/database/postgres"
	"github.com/thesahibnanda-max/relay/server/package/database/repository"
)

// testConfig is the default config new() tests build a selector with -
// caching on, at a duration long enough that no test's real elapsed time
// could ever cross it by accident.
func testConfig() config.Config {
	return config.Config{PostgresCacheTTL: time.Hour}
}

// fakeMongoURLRepo and fakeShardMapRepo are in-memory stand-ins for the two
// repository interfaces selector depends on, so the round-robin algorithm
// (and the cache sitting in front of them) can be unit tested with no real
// Postgres involved. Call counts are tracked (mutex-guarded: the cache's own
// race test drives these concurrently) so a test can assert the cache
// actually avoided a repository round trip, not just that the answer was
// right.
type fakeMongoURLRepo struct {
	mu             sync.Mutex
	urls           []postgres.MongoDBURL
	getByIDCalls   int
	listOrderCalls int
}

func (f *fakeMongoURLRepo) ListOrderedByID(ctx context.Context) ([]postgres.MongoDBURL, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.listOrderCalls++
	return f.urls, nil
}

func (f *fakeMongoURLRepo) EnsureURL(ctx context.Context, url string) (postgres.MongoDBURL, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, u := range f.urls {
		if u.MongoURL == url {
			return u, nil
		}
	}
	row := postgres.MongoDBURL{ID: uint(len(f.urls) + 1), MongoURL: url}
	f.urls = append(f.urls, row)
	return row, nil
}

func (f *fakeMongoURLRepo) GetByID(ctx context.Context, id uint) (postgres.MongoDBURL, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getByIDCalls++
	for _, u := range f.urls {
		if u.ID == id {
			return u, nil
		}
	}
	return postgres.MongoDBURL{}, errNotFound
}

func (f *fakeMongoURLRepo) calls() (getByID, listOrdered int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.getByIDCalls, f.listOrderCalls
}

type fakeShardMapRepo struct {
	mu               sync.Mutex
	rows             []postgres.SessionShardMap
	getBySessionCall int
}

func (f *fakeShardMapRepo) Count(ctx context.Context) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return int64(len(f.rows)), nil
}

func (f *fakeShardMapRepo) GetBySessionID(ctx context.Context, sessionID string) (postgres.SessionShardMap, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.getBySessionCall++
	for _, r := range f.rows {
		if r.SessionID == sessionID {
			return r, true, nil
		}
	}
	return postgres.SessionShardMap{}, false, nil
}

func (f *fakeShardMapRepo) Create(ctx context.Context, sessionID string, mongoURLID uint) (postgres.SessionShardMap, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	row := postgres.SessionShardMap{ID: uint(len(f.rows) + 1), SessionID: sessionID, MongoURLID: mongoURLID}
	f.rows = append(f.rows, row)
	return row, nil
}

func (f *fakeShardMapRepo) calls() (getBySessionID int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.getBySessionCall
}

var errNotFound = &notFoundErr{}

type notFoundErr struct{}

func (*notFoundErr) Error() string { return "not found" }

var (
	_ repository.MongoURLRepository = (*fakeMongoURLRepo)(nil)
	_ repository.ShardMapRepository = (*fakeShardMapRepo)(nil)
)

func TestShardURLFor_RoundRobinsAcrossNewSessions(t *testing.T) {
	mongoURLs := &fakeMongoURLRepo{urls: []postgres.MongoDBURL{
		{ID: 1, MongoURL: "mongodb://shard-a"},
		{ID: 2, MongoURL: "mongodb://shard-b"},
		{ID: 3, MongoURL: "mongodb://shard-c"},
	}}
	shardMap := &fakeShardMapRepo{}
	sel, err := New(testConfig(), shardMap, mongoURLs)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	want := []string{
		"mongodb://shard-a",
		"mongodb://shard-b",
		"mongodb://shard-c",
		"mongodb://shard-a", // wraps back around
	}
	for i, w := range want {
		got, err := sel.ShardURLFor(context.Background(), sessionIDFor(i))
		if err != nil {
			t.Fatalf("ShardURLFor(%d): %v", i, err)
		}
		if got != w {
			t.Errorf("session %d: got shard %q, want %q", i, got, w)
		}
	}
}

func TestShardURLFor_IsIdempotentForAnAlreadyMappedSession(t *testing.T) {
	mongoURLs := &fakeMongoURLRepo{urls: []postgres.MongoDBURL{
		{ID: 1, MongoURL: "mongodb://shard-a"},
		{ID: 2, MongoURL: "mongodb://shard-b"},
	}}
	shardMap := &fakeShardMapRepo{}
	sel, err := New(testConfig(), shardMap, mongoURLs)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx := context.Background()
	first, err := sel.ShardURLFor(ctx, "session-1")
	if err != nil {
		t.Fatalf("first ShardURLFor: %v", err)
	}

	// Assign a second, unrelated session in between - if resolution weren't
	// idempotent per-session this would otherwise shift session-1's answer.
	if _, err := sel.ShardURLFor(ctx, "session-2"); err != nil {
		t.Fatalf("ShardURLFor(session-2): %v", err)
	}

	for i := 0; i < 3; i++ {
		got, err := sel.ShardURLFor(ctx, "session-1")
		if err != nil {
			t.Fatalf("repeat ShardURLFor(session-1): %v", err)
		}
		if got != first {
			t.Errorf("repeat lookup %d: got %q, want stable %q", i, got, first)
		}
	}
}

func TestShardURLFor_ErrorsWithNoShardsConfigured(t *testing.T) {
	sel, err := New(testConfig(), &fakeShardMapRepo{}, &fakeMongoURLRepo{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	if _, err := sel.ShardURLFor(context.Background(), "session-1"); err == nil {
		t.Fatal("expected an error when no shards are configured, got nil")
	}
}

func sessionIDFor(i int) string {
	return "session-" + string(rune('a'+i))
}

// TestShardURLFor_CachesAnAlreadyMappedSession is the actual bug the L1
// cache exists to fix: a second lookup for the same already-mapped session
// must not cost another Postgres round trip.
func TestShardURLFor_CachesAnAlreadyMappedSession(t *testing.T) {
	mongoURLs := &fakeMongoURLRepo{urls: []postgres.MongoDBURL{{ID: 1, MongoURL: "mongodb://shard-a"}}}
	shardMap := &fakeShardMapRepo{}
	sel, err := New(testConfig(), shardMap, mongoURLs)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()

	first, err := sel.ShardURLFor(ctx, "session-1")
	if err != nil {
		t.Fatalf("first ShardURLFor: %v", err)
	}
	getByID, listOrdered := mongoURLs.calls()
	getBySession := shardMap.calls()

	for i := 0; i < 5; i++ {
		got, err := sel.ShardURLFor(ctx, "session-1")
		if err != nil {
			t.Fatalf("cached ShardURLFor: %v", err)
		}
		if got != first {
			t.Errorf("cached lookup %d: got %q, want %q", i, got, first)
		}
	}

	if gotGetByID, gotListOrdered := mongoURLs.calls(); gotGetByID != getByID || gotListOrdered != listOrdered {
		t.Errorf("cached lookups made extra mongoURLs calls: GetByID %d->%d, ListOrderedByID %d->%d",
			getByID, gotGetByID, listOrdered, gotListOrdered)
	}
	if got := shardMap.calls(); got != getBySession {
		t.Errorf("cached lookups made extra GetBySessionID calls: %d -> %d", getBySession, got)
	}
}

// TestShardURLFor_NewSessionWarmsTheCache proves the round-robin-assignment
// path also populates the cache, so the very next lookup for that session
// (which always follows Join in practice) is already a hit.
func TestShardURLFor_NewSessionWarmsTheCache(t *testing.T) {
	mongoURLs := &fakeMongoURLRepo{urls: []postgres.MongoDBURL{{ID: 1, MongoURL: "mongodb://shard-a"}}}
	shardMap := &fakeShardMapRepo{}
	sel, err := New(testConfig(), shardMap, mongoURLs)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()

	if _, err := sel.ShardURLFor(ctx, "brand-new"); err != nil {
		t.Fatalf("first ShardURLFor: %v", err)
	}
	getBySession := shardMap.calls()

	if _, err := sel.ShardURLFor(ctx, "brand-new"); err != nil {
		t.Fatalf("second ShardURLFor: %v", err)
	}
	if got := shardMap.calls(); got != getBySession {
		t.Errorf("second lookup for a just-created session made an extra GetBySessionID call: %d -> %d", getBySession, got)
	}
}

// TestShardURLFor_DifferentSessionsDoNotCollideInTheCache guards against the
// most obvious way a keyed cache could be wrong: two sessions must never
// resolve to each other's cached URL.
func TestShardURLFor_DifferentSessionsDoNotCollideInTheCache(t *testing.T) {
	mongoURLs := &fakeMongoURLRepo{urls: []postgres.MongoDBURL{
		{ID: 1, MongoURL: "mongodb://shard-a"},
		{ID: 2, MongoURL: "mongodb://shard-b"},
	}}
	shardMap := &fakeShardMapRepo{}
	sel, err := New(testConfig(), shardMap, mongoURLs)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()

	a, err := sel.ShardURLFor(ctx, "session-a")
	if err != nil {
		t.Fatalf("ShardURLFor(session-a): %v", err)
	}
	b, err := sel.ShardURLFor(ctx, "session-b")
	if err != nil {
		t.Fatalf("ShardURLFor(session-b): %v", err)
	}
	if a == b {
		t.Fatalf("session-a and session-b resolved to the same shard (%q) - test is meaningless, fix the fixture", a)
	}

	if got, err := sel.ShardURLFor(ctx, "session-a"); err != nil || got != a {
		t.Errorf("cached session-a = %q, %v; want %q, nil", got, err, a)
	}
	if got, err := sel.ShardURLFor(ctx, "session-b"); err != nil || got != b {
		t.Errorf("cached session-b = %q, %v; want %q, nil", got, err, b)
	}
}

// TestShardURLFor_TTLExpiryReQueriesTheRepository proves this is a real
// cache-with-fallback, not "cache forever": once an entry is older than the
// configured TTL, the next lookup must hit the repository again.
func TestShardURLFor_TTLExpiryReQueriesTheRepository(t *testing.T) {
	mongoURLs := &fakeMongoURLRepo{urls: []postgres.MongoDBURL{{ID: 1, MongoURL: "mongodb://shard-a"}}}
	shardMap := &fakeShardMapRepo{}
	sel, err := New(config.Config{PostgresCacheTTL: 50 * time.Millisecond}, shardMap, mongoURLs)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()

	if _, err := sel.ShardURLFor(ctx, "session-1"); err != nil {
		t.Fatalf("first ShardURLFor: %v", err)
	}
	getByID, _ := mongoURLs.calls()

	time.Sleep(100 * time.Millisecond)

	if _, err := sel.ShardURLFor(ctx, "session-1"); err != nil {
		t.Fatalf("post-expiry ShardURLFor: %v", err)
	}
	if gotGetByID, _ := mongoURLs.calls(); gotGetByID != getByID+1 {
		t.Errorf("expected exactly one more GetByID call after TTL expiry, got %d -> %d", getByID, gotGetByID)
	}
}

// TestShardURLFor_ZeroTTLDisablesTheCache proves POSTGRES_CACHE_TTL=0 (or
// negative) is a real, documented way to turn caching off entirely - every
// lookup must re-query the repository.
func TestShardURLFor_ZeroTTLDisablesTheCache(t *testing.T) {
	mongoURLs := &fakeMongoURLRepo{urls: []postgres.MongoDBURL{{ID: 1, MongoURL: "mongodb://shard-a"}}}
	shardMap := &fakeShardMapRepo{}
	sel, err := New(config.Config{PostgresCacheTTL: 0}, shardMap, mongoURLs)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()

	// The very first lookup for a brand-new session takes the "assign a
	// shard" path, which resolves the URL straight from ListOrderedByID and
	// never calls GetByID - GetByID is only used for an *already-mapped*
	// session. Do that first lookup once, outside the assertion, so what's
	// being tested is "every lookup after the mapping exists re-queries",
	// not conflating the two different code paths' call counts.
	if _, err := sel.ShardURLFor(ctx, "session-1"); err != nil {
		t.Fatalf("warm-up ShardURLFor: %v", err)
	}
	baseline, _ := mongoURLs.calls()

	for i := 0; i < 3; i++ {
		if _, err := sel.ShardURLFor(ctx, "session-1"); err != nil {
			t.Fatalf("ShardURLFor(%d): %v", i, err)
		}
	}
	if got, _ := mongoURLs.calls(); got != baseline+3 {
		t.Errorf("expected every lookup to re-query with the cache disabled, got %d GetByID calls, want %d", got, baseline+3)
	}
}

// TestShardURLFor_ConcurrentLookupsForTheSameSessionAreRaceFree is the one
// new piece of shared mutable state in this package (the cache) proving
// safe under concurrent access - run with -race.
func TestShardURLFor_ConcurrentLookupsForTheSameSessionAreRaceFree(t *testing.T) {
	mongoURLs := &fakeMongoURLRepo{urls: []postgres.MongoDBURL{{ID: 1, MongoURL: "mongodb://shard-a"}}}
	shardMap := &fakeShardMapRepo{}
	sel, err := New(testConfig(), shardMap, mongoURLs)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	ctx := context.Background()

	var wg sync.WaitGroup
	for i := 0; i < 20; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := sel.ShardURLFor(ctx, "session-1"); err != nil {
				t.Errorf("ShardURLFor: %v", err)
			}
		}()
	}
	wg.Wait()
}
