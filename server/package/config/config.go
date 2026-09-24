package config

import (
	"context"
	"errors"

	"github.com/sethvargo/go-envconfig"
)

type Config struct {
	PORT          int      `env:"PORT,default=5555"`
	PostgreSQLDSN string   `env:"SQL_DSN,required"`
	MongoURLs     []string `env:"MONGO_URLS,required"` // comma-separated list of MongoDB connection strings, one per shard
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
