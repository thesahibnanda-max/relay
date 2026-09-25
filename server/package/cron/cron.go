package cron

import (
	"context"
	"errors"
	"fmt"
	"log/slog"

	gocron "github.com/robfig/cron/v3"

	"github.com/thesahibnanda-max/relay/server/package/config"
	"github.com/thesahibnanda-max/relay/server/package/database/mongodb"
	"github.com/thesahibnanda-max/relay/server/package/database/postgres"
)

type Interface interface {
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
}

type impl struct {
	goCron             *gocron.Cron
	mongodbPool        mongodb.Interface
	postgresConnection postgres.Interface
}

// New schedules a periodic liveness check of every configured Postgres and
// Mongo connection, every cfg.PingCheckInterval (default 5m30s) - detects an
// outage even during a quiet period with no real traffic to surface one
// on its own. Independent of package sharding's PostgresCacheTTL, which is
// about avoiding redundant reads, not about detecting failures proactively.
func New(cfg config.Config, mongodbPool mongodb.Interface, postgresConnection postgres.Interface) (Interface, error) {
	if mongodbPool == nil {
		return nil, fmt.Errorf("mongodbPool is nil")
	}
	if postgresConnection == nil {
		return nil, fmt.Errorf("postgresConnection is nil")
	}

	i := impl{
		goCron:             gocron.New(gocron.WithChain(gocron.Recover(gocron.DefaultLogger), gocron.DelayIfStillRunning(gocron.DefaultLogger))),
		mongodbPool:        mongodbPool,
		postgresConnection: postgresConnection,
	}

	if _, err := i.goCron.AddFunc(fmt.Sprintf("@every %s", cfg.PingCheckInterval), func() {
		if err := i.pingAll(context.Background()); err != nil {
			slog.Error("periodic DB ping failed", slog.Any("error", err))
		}
	}); err != nil {
		return nil, err
	}

	return i, nil
}

// pingAll is the scheduled job's own body, split out so it's directly
// unit-testable without waiting on the real scheduler.
func (i impl) pingAll(ctx context.Context) error {
	err1 := i.mongodbPool.Ping(ctx)
	err2 := i.postgresConnection.Ping(ctx)
	return errors.Join(err1, err2)
}

// Start begins running the periodic ping job in the background and returns
// immediately - gocron.Cron.Start is itself non-blocking, so ctx is unused
// here (nothing to wait on yet); it's part of the signature for symmetry
// with Stop and to match this module's other lifecycle-shaped constructors.
func (i impl) Start(ctx context.Context) error {
	i.goCron.Start()
	return nil
}

// Stop asks the scheduler to stop accepting new runs, then waits for
// whichever ping job is currently in flight (if any) to finish - bounded by
// ctx, so a caller's own shutdown timeout (e.g. an Fx OnStop deadline) is
// still honored even if a ping is hung.
func (i impl) Stop(ctx context.Context) error {
	select {
	case <-i.goCron.Stop().Done():
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}
