// Package postgres is the control plane: it never stores session/agent/
// message content, only the shard map (which MongoDB URL a given session's
// data lives on). Tables are created automatically on first connect - see
// New - so there is no separate migration step to run by hand.
package postgres

import (
	gormpostgres "gorm.io/driver/postgres"
	"gorm.io/gorm"

	"github.com/thesahibnanda-max/relay/server/package/config"
)

// Interface is the control-plane database handle every Postgres-backed
// repository in this module depends on.
type Interface interface {
	// DB returns the underlying *gorm.DB. Exposed directly (rather than
	// wrapped further) because gorm.DB is itself the idiomatic query
	// builder repositories are expected to use - wrapping it again would
	// just be a redundant layer, not real decoupling.
	DB() *gorm.DB
}

type impl struct {
	db *gorm.DB
}

// New opens the Postgres connection named by cfg and creates the
// mongo_db_urls/session_shard_map tables if they don't already exist.
func New(cfg config.Config) (Interface, error) {
	db, err := gorm.Open(gormpostgres.Open(cfg.PostgreSQLDSN), &gorm.Config{})
	if err != nil {
		return nil, err
	}
	if err := db.AutoMigrate(&MongoDBURL{}, &SessionShardMap{}); err != nil {
		return nil, err
	}
	return impl{db: db}, nil
}

func (i impl) DB() *gorm.DB { return i.db }
