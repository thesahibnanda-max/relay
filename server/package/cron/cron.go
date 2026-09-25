package cron

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	gocron "github.com/robfig/cron/v3"

	"github.com/thesahibnanda-max/relay/server/package/config"
	"github.com/thesahibnanda-max/relay/server/package/database/mongodb"
	"github.com/thesahibnanda-max/relay/server/package/database/postgres"
	"github.com/thesahibnanda-max/relay/server/package/session"
)

type Interface interface {
	Start(ctx context.Context) error
	Stop(ctx context.Context) error
}

type impl struct {
	goCron             *gocron.Cron
	mongodbPool        mongodb.Interface
	postgresConnection postgres.Interface
	sessions           session.Interface
	mongoURLs          []string
	sessionMaxAge      time.Duration
}

// New schedules every one of this server's periodic background jobs on one
// supervised scheduler, so none of them can die silently: gocron.Recover
// catches and logs a panic in any job instead of letting it kill the
// scheduler (or the process), and gocron.DelayIfStillRunning skips a tick
// for a job still running from the previous one instead of piling runs up.
// A bare `go func(){ for { ...; time.Sleep(...) } }()` goroutine (this
// package's very first approach, and how session sweeping briefly lived
// directly in app.Serve) has neither property - a panic or a silent hang in
// it is invisible until someone notices the symptom, not the cause.
//
//   - The DB ping job runs every cfg.PingCheckInterval (default 5m30s) -
//     detects an outage even during a quiet period with no real traffic to
//     surface one on its own. Independent of package sharding's
//     PostgresCacheTTL, which is about avoiding redundant reads, not about
//     detecting failures proactively.
//   - The sweep job runs every cfg.SweepEvery (default 30s), once per
//     configured shard: session.Interface.Sweep - TTL-expiry and
//     disconnect-reaping, state transitions only, nothing deleted. Cheap and
//     latency-sensitive (a message's expiry/an agent's reap should be timely),
//     so it runs often.
//   - The idle-session cleanup job runs on its own, much less frequent
//     schedule - cfg.SessionCleanupInterval (default 30m) - since it does
//     real, permanent deletion work (DeleteSessionsOlderThan, removing every
//     session, agent and message idle since cfg.SessionMaxAge, default 24h):
//     there is no correctness reason to run something that destructive on a
//     30-second cadence, only extra load.
func New(cfg config.Config, mongodbPool mongodb.Interface, postgresConnection postgres.Interface, sessions session.Interface) (Interface, error) {
	if mongodbPool == nil {
		return nil, fmt.Errorf("mongodbPool is nil")
	}
	if postgresConnection == nil {
		return nil, fmt.Errorf("postgresConnection is nil")
	}
	if sessions == nil {
		return nil, fmt.Errorf("sessions is nil")
	}

	i := impl{
		goCron:             gocron.New(gocron.WithChain(gocron.Recover(gocron.DefaultLogger), gocron.DelayIfStillRunning(gocron.DefaultLogger))),
		mongodbPool:        mongodbPool,
		postgresConnection: postgresConnection,
		sessions:           sessions,
		mongoURLs:          cfg.MongoURLs,
		sessionMaxAge:      cfg.SessionMaxAge,
	}

	if _, err := i.goCron.AddFunc(fmt.Sprintf("@every %s", cfg.PingCheckInterval), func() {
		if err := i.pingAll(context.Background()); err != nil {
			slog.Error("periodic DB ping failed", slog.Any("error", err))
		}
	}); err != nil {
		return nil, err
	}

	if _, err := i.goCron.AddFunc(fmt.Sprintf("@every %s", cfg.SweepEvery), func() {
		i.sweepAll(context.Background())
	}); err != nil {
		return nil, err
	}

	if _, err := i.goCron.AddFunc(fmt.Sprintf("@every %s", cfg.SessionCleanupInterval), func() {
		i.cleanupOldSessions(context.Background())
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

// sweepAll is the scheduled sweep job's own body, split out so it's directly
// unit-testable without waiting on the real scheduler. Errors are logged per
// shard rather than aborting the rest - one bad shard must never stop every
// other shard's sweep from running.
func (i impl) sweepAll(ctx context.Context) {
	for _, url := range i.mongoURLs {
		if err := i.sessions.Sweep(ctx, url); err != nil {
			slog.Error("periodic session sweep failed", slog.String("shard", url), slog.Any("error", err))
		}
	}
}

// cleanupOldSessions is the scheduled idle-session cleanup job's own body,
// split out so it's directly unit-testable without waiting on the real
// scheduler. Errors are logged per shard rather than aborting the rest - one
// bad shard must never stop every other shard's cleanup from running.
func (i impl) cleanupOldSessions(ctx context.Context) {
	cutoff := time.Now().UTC().Add(-i.sessionMaxAge)
	for _, url := range i.mongoURLs {
		if _, err := i.sessions.DeleteSessionsOlderThan(ctx, url, cutoff); err != nil {
			slog.Error("periodic idle-session cleanup failed", slog.String("shard", url), slog.Any("error", err))
		}
	}
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
