package config

import (
	"context"
	"errors"
	"time"

	"github.com/sethvargo/go-envconfig"
)

type Config struct {
	PORT          int      `env:"PORT,default=5555"`
	PostgreSQLDSN string   `env:"SQL_DSN,required"`
	MongoURLs     []string `env:"MONGO_URLS,required"` // comma-separated list of MongoDB connection strings, one per shard

	// Resource/policy knobs, all configurable since a self-hosted server can
	// run on anything from a laptop to a free-tier VM - defaults match the
	// local daemon's own proven numbers.
	PairRateLimit   int           `env:"PAIR_RATE_LIMIT,default=20"`   // msgs/min, sender->target pair
	SenderRateLimit int           `env:"SENDER_RATE_LIMIT,default=60"` // msgs/min, per sender overall
	RPCRateLimit    int           `env:"RPC_RATE_LIMIT,default=200"`   // RPCs per RPCRateWindow, per connection
	RPCRateWindow   time.Duration `env:"RPC_RATE_WINDOW,default=10s"`
	MaxInFlightRPCs int           `env:"MAX_INFLIGHT_RPCS,default=16"` // concurrent in-flight RPCs per connection
	DedupWindow     time.Duration `env:"DEDUP_WINDOW,default=30s"`
	MessageTTL      time.Duration `env:"MESSAGE_TTL,default=1h"`
	SweepEvery      time.Duration `env:"SWEEP_EVERY,default=30s"`
	DisconnectGrace time.Duration `env:"DISCONNECT_GRACE,default=15m"`
}

// New reads Config from the environment. It is the root of the dependency
// graph, so unlike every other constructor in this module it takes no
// dependencies to check for nil.
func New() (Config, error) {
	var c Config
	if err := envconfig.Process(context.Background(), &c); err != nil {
		return c, err
	}
	if len(c.MongoURLs) == 0 {
		return c, errors.New("config: MONGO_URLS must name at least one MongoDB shard")
	}
	return c, nil
}
