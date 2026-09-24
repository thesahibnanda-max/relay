package app

import (
	"context"
	"fmt"
	"net"
	"net/http"
	"time"

	"go.uber.org/fx"

	"github.com/thesahibnanda-max/relay/server/package/config"
	"github.com/thesahibnanda-max/relay/server/package/database/repository"
	"github.com/thesahibnanda-max/relay/server/package/session"
	"github.com/thesahibnanda-max/relay/server/package/ws"
)

// Serve is the fx.Invoke target that actually starts the server: on OnStart
// it makes sure every shard named in cfg.MongoURLs has a mongo_db_urls row
// (so newly-added shards are picked up on every restart with no manual
// step), force-marks every agent still "connected" on every shard as
// disconnected (a previous process definitely owned any connection that was
// live before this restart, mirroring the local daemon's own rule), then
// begins listening; OnStop shuts the HTTP server down cleanly. *http.Server
// is the standard library's own type - an unavoidable pointer, like
// *gorm.DB and *mongo.Client elsewhere in this module.
func Serve(lc fx.Lifecycle, cfg config.Config, mongoURLs repository.MongoURLRepository, agents repository.AgentRepository, sessions session.Interface, handler ws.Interface) {
	srv := &http.Server{
		Addr:    fmt.Sprintf(":%d", cfg.PORT),
		Handler: ws.NewHandler(handler),
	}
	sweepStop := make(chan struct{})

	lc.Append(fx.Hook{
		OnStart: func(ctx context.Context) error {
			for _, url := range cfg.MongoURLs {
				if _, err := mongoURLs.EnsureURL(ctx, url); err != nil {
					return fmt.Errorf("app: registering shard %s: %w", url, err)
				}
				if err := agents.MarkAllDisconnected(ctx, url); err != nil {
					return fmt.Errorf("app: marking shard %s's agents disconnected: %w", url, err)
				}
			}
			ln, err := net.Listen("tcp", srv.Addr)
			if err != nil {
				return fmt.Errorf("app: listening on %s: %w", srv.Addr, err)
			}
			// srv.Serve always returns non-nil, including http.ErrServerClosed
			// on a graceful OnStop shutdown - nothing actionable to do with
			// it here in this skeleton.
			go func() { _ = srv.Serve(ln) }()
			go runSweeps(sweepStop, cfg, sessions)
			return nil
		},
		OnStop: func(ctx context.Context) error {
			close(sweepStop)
			return srv.Shutdown(ctx)
		},
	})
}

// runSweeps runs one TTL-expiry + disconnect-reaper pass (session.Sweep) per
// configured shard on cfg.SweepEvery, until stop is closed - the periodic
// maintenance pass that earns the expired/undeliverable terminal states.
func runSweeps(stop <-chan struct{}, cfg config.Config, sessions session.Interface) {
	ticker := time.NewTicker(cfg.SweepEvery)
	defer ticker.Stop()
	for {
		select {
		case <-stop:
			return
		case <-ticker.C:
			for _, url := range cfg.MongoURLs {
				sctx, cancel := context.WithTimeout(context.Background(), cfg.SweepEvery)
				_ = sessions.Sweep(sctx, url)
				cancel()
			}
		}
	}
}
