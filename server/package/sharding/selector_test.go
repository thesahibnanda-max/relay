package sharding

import (
	"context"
	"testing"

	"github.com/thesahibnanda-max/relay/server/package/database/postgres"
	"github.com/thesahibnanda-max/relay/server/package/database/repository"
)

// fakeMongoURLRepo and fakeShardMapRepo are in-memory stand-ins for the two
// repository interfaces selector depends on, so the round-robin algorithm
// can be unit tested with no real Postgres involved.
type fakeMongoURLRepo struct {
	urls []postgres.MongoDBURL
}

func (f *fakeMongoURLRepo) ListOrderedByID(ctx context.Context) ([]postgres.MongoDBURL, error) {
	return f.urls, nil
}

func (f *fakeMongoURLRepo) EnsureURL(ctx context.Context, url string) (postgres.MongoDBURL, error) {
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
	for _, u := range f.urls {
		if u.ID == id {
			return u, nil
		}
	}
	return postgres.MongoDBURL{}, errNotFound
}

type fakeShardMapRepo struct {
	rows []postgres.SessionShardMap
}

func (f *fakeShardMapRepo) Count(ctx context.Context) (int64, error) {
	return int64(len(f.rows)), nil
}

func (f *fakeShardMapRepo) GetBySessionID(ctx context.Context, sessionID string) (postgres.SessionShardMap, bool, error) {
	for _, r := range f.rows {
		if r.SessionID == sessionID {
			return r, true, nil
		}
	}
	return postgres.SessionShardMap{}, false, nil
}

func (f *fakeShardMapRepo) Create(ctx context.Context, sessionID string, mongoURLID uint) (postgres.SessionShardMap, error) {
	row := postgres.SessionShardMap{ID: uint(len(f.rows) + 1), SessionID: sessionID, MongoURLID: mongoURLID}
	f.rows = append(f.rows, row)
	return row, nil
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
	sel, err := New(shardMap, mongoURLs)
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
	sel, err := New(shardMap, mongoURLs)
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
	sel, err := New(&fakeShardMapRepo{}, &fakeMongoURLRepo{})
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
