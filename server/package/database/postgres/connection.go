// Package postgres is the control plane: it never stores session/agent/
// message content, only the shard map (which MongoDB URL a given session's
// data lives on). Tables are created automatically on first connect - see
// New - so there is no separate migration step to run by hand.
package postgres

import (
	"context"
	"errors"

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

	// Ping reports whether this shard's control-plane database can actually
	// serve a query, not just that a connection to it exists - see the
	// implementation's own comment for why both checks are needed.
	Ping(ctx context.Context) error
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

// Ping checks both that a connection can be established (sqlDB.PingContext)
// and that a real query actually executes (SELECT 1) - deliberately both,
// not just one: this DSN goes through Supabase's session pooler, which can
// report a healthy TCP-level connection while query execution is broken
// (a stale pooled backend, an exhausted pool slot, etc.), so a bare ping
// alone isn't a reliable health signal for a pooler-fronted database. "1" is
// a constant, not real data, so it's a literal in the SQL rather than a
// bound parameter - GORM's Raw() expects its own "?" placeholder style, and
// a native "$1" placeholder silently breaks its argument encoding instead
// (confirmed against the real database: it fails every time with "unable to
// encode 1 into text format for text (OID 25)"). Both checks run against
// the same ctx-bound session (ctxDB), so the whole call - not just the ping
// half - honors the caller's timeout/cancellation.
func (i impl) Ping(ctx context.Context) error {
	ctxDB := i.db.WithContext(ctx)
	sqlDB, err := ctxDB.DB()
	if err != nil {
		return err
	}
	var one int
	return errors.Join(sqlDB.PingContext(ctx), ctxDB.Raw("SELECT 1").Scan(&one).Error)
}
