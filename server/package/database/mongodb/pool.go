// Package mongodb is the data plane: N independent MongoDB shards, each
// holding a disjoint subset of sessions' full data. Which shard a given
// session lives on is decided once, at creation time, and recorded in
// Postgres - see package sharding.
package mongodb

import (
	"context"
	"errors"
	"fmt"

	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/thesahibnanda-max/relay/server/package/config"
)

// databaseName is the single logical database used within every configured
// MongoDB shard - each shard is a whole cluster/URL, not a database name, so
// one fixed name here is enough.
const databaseName = "relay"

// Interface is a pool of already-connected MongoDB clients, one per
// configured shard URL.
type Interface interface {
	// Database returns the shard's database handle for mongoURL, which must
	// be one of the URLs this pool was constructed with.
	Database(mongoURL string) (*mongo.Database, error)
	// Close disconnects every pooled client. The long-lived server process
	// never calls this (its one pool lives for the whole process), but any
	// short-lived caller that constructs its own pool - a test, a one-off
	// script - must, or it leaks a live connection per configured shard.
	Close(ctx context.Context) error
}

type impl struct {
	clients map[string]*mongo.Client
}

// New connects to every shard named in cfg.MongoURLs up front, so a
// misconfigured shard fails fast at startup rather than on first use.
func New(cfg config.Config) (Interface, error) {
	ctx := context.Background()
	clients := make(map[string]*mongo.Client, len(cfg.MongoURLs))
	for _, url := range cfg.MongoURLs {
		client, err := mongo.Connect(options.Client().ApplyURI(url))
		if err != nil {
			return nil, fmt.Errorf("mongo: connecting to %s: %w", url, err)
		}
		if err := client.Ping(ctx, nil); err != nil {
			return nil, fmt.Errorf("mongo: pinging %s: %w", url, err)
		}
		clients[url] = client
	}
	return impl{clients: clients}, nil
}

func (i impl) Database(mongoURL string) (*mongo.Database, error) {
	client, ok := i.clients[mongoURL]
	if !ok {
		return nil, fmt.Errorf("mongo: no client configured for url %q", mongoURL)
	}
	return client.Database(databaseName), nil
}

func (i impl) Close(ctx context.Context) error {
	var errs []error
	for _, client := range i.clients {
		if err := client.Disconnect(ctx); err != nil {
			errs = append(errs, err)
		}
	}
	return errors.Join(errs...)
}
