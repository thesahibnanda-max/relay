// Package sharding decides, once per global session, which configured
// MongoDB shard that session's data lives on - see the project plan's
// "per-session, round-robin" decision.
package sharding

import (
	"context"
	"errors"
	"fmt"

	"github.com/thesahibnanda-max/relay/server/package/config"
	"github.com/thesahibnanda-max/relay/server/package/database/repository"
)

// Interface resolves a session to the MongoDB connection string its data
// lives (or should be created) on.
type Interface interface {
	// ShardURLFor returns the MongoDB URL sessionID's data lives on,
	// assigning one round-robin if this is the first time sessionID has
	// been seen. Idempotent: calling it again for the same session always
	// returns the same URL.
	ShardURLFor(ctx context.Context, sessionID string) (string, error)
}

type selector struct {
	shardMap  repository.ShardMapRepository
	mongoURLs repository.MongoURLRepository
	cache     *shardURLCache
}

func New(cfg config.Config, shardMap repository.ShardMapRepository, mongoURLs repository.MongoURLRepository) (Interface, error) {
	if shardMap == nil {
		return nil, errors.New("sharding: shardMap repository is nil")
	}
	if mongoURLs == nil {
		return nil, errors.New("sharding: mongoURLs repository is nil")
	}
	return selector{shardMap: shardMap, mongoURLs: mongoURLs, cache: newShardURLCache(cfg.PostgresCacheTTL)}, nil
}

func (s selector) ShardURLFor(ctx context.Context, sessionID string) (string, error) {
	if url, ok := s.cache.get(sessionID); ok {
		return url, nil
	}

	if existing, found, err := s.shardMap.GetBySessionID(ctx, sessionID); err != nil {
		return "", err
	} else if found {
		url, err := s.mongoURLs.GetByID(ctx, existing.MongoURLID)
		if err != nil {
			return "", err
		}
		s.cache.set(sessionID, url.MongoURL)
		return url.MongoURL, nil
	}

	// Deliberately NOT cached: ListOrderedByID/Count are only read on this
	// brand-new-session path (at most once per session's whole lifetime,
	// never repeated), and caching Count specifically would let concurrent
	// new-session creations within the same TTL window all compute the same
	// stale round-robin index - a real correctness regression the cache
	// would introduce for zero benefit, since this path is never hot.
	urls, err := s.mongoURLs.ListOrderedByID(ctx)
	if err != nil {
		return "", err
	}
	if len(urls) == 0 {
		return "", errors.New("sharding: no MongoDB shards are configured")
	}
	count, err := s.shardMap.Count(ctx)
	if err != nil {
		return "", err
	}
	// A pure function of "how many sessions have been assigned so far" -
	// restart-safe, no separate counter to keep in sync.
	chosen := urls[int(count)%len(urls)]
	if _, err := s.shardMap.Create(ctx, sessionID, chosen.ID); err != nil {
		return "", fmt.Errorf("sharding: recording assignment for session %s: %w", sessionID, err)
	}
	s.cache.set(sessionID, chosen.MongoURL)
	return chosen.MongoURL, nil
}
