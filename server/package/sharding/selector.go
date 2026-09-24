// Package sharding decides, once per global session, which configured
// MongoDB shard that session's data lives on - see the project plan's
// "per-session, round-robin" decision.
package sharding

import (
	"context"
	"errors"
	"fmt"

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
}

func New(shardMap repository.ShardMapRepository, mongoURLs repository.MongoURLRepository) (Interface, error) {
	if shardMap == nil {
		return nil, errors.New("sharding: shardMap repository is nil")
	}
	if mongoURLs == nil {
		return nil, errors.New("sharding: mongoURLs repository is nil")
	}
	return selector{shardMap: shardMap, mongoURLs: mongoURLs}, nil
}

func (s selector) ShardURLFor(ctx context.Context, sessionID string) (string, error) {
	if existing, found, err := s.shardMap.GetBySessionID(ctx, sessionID); err != nil {
		return "", err
	} else if found {
		url, err := s.mongoURLs.GetByID(ctx, existing.MongoURLID)
		if err != nil {
			return "", err
		}
		return url.MongoURL, nil
	}

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
	return chosen.MongoURL, nil
}
