# Deploying the relay global-session server

This is the centralized server `--session=NEW` (a global session) talks to. It's a single
static Go binary plus a Postgres instance (the control-plane shard map) and one or more
MongoDB instances (the actual session/agent/message data, one shard per URL in `MONGO_URLS`).
Nothing here assumes a big machine - the defaults below match what the local daemon already
proves in production, and every resource knob is an env var so a small VM (an Oracle Cloud
free-tier instance, for example) can be tuned without a rebuild.

## Environment variables

| Variable | Default | Meaning |
|---|---|---|
| `PORT` | `5555` | TCP port the server listens on. |
| `SQL_DSN` | *(required)* | Postgres DSN (gorm/`pgx` style), e.g. `host=localhost user=relay password=relay dbname=relay port=5432 sslmode=disable`. |
| `MONGO_URLS` | *(required)* | Comma-separated MongoDB connection strings, one per shard. A single shard is fine to start; add more later and restart - new shards are picked up automatically. |
| `PAIR_RATE_LIMIT` | `20` | Messages/minute a sender may send to one specific recipient. |
| `SENDER_RATE_LIMIT` | `60` | Messages/minute a sender may send in total, across all recipients. |
| `RPC_RATE_LIMIT` | `200` | RPCs a single connection may issue per `RPC_RATE_WINDOW`. |
| `RPC_RATE_WINDOW` | `10s` | The window `RPC_RATE_LIMIT` is measured over. |
| `MAX_INFLIGHT_RPCS` | `16` | Concurrent in-flight RPCs a single connection may have outstanding. |
| `DEDUP_WINDOW` | `30s` | An identical (from, to, kind, body) retry within this window coalesces into the existing message instead of creating a new one. |
| `MESSAGE_TTL` | `1h` | How long an undelivered message lives before the sweep expires it. |
| `SWEEP_EVERY` | `30s` | How often the TTL-expiry + disconnect-reaper maintenance pass runs, per shard. This never deletes anything - it only transitions message/agent states. |
| `DISCONNECT_GRACE` | `15m` | How long a disconnected agent may stay offline before the sweep reaps it as gone for good (`exited`) and fails its pending mail to `undeliverable`. |
| `SESSION_MAX_AGE` | `24h` | How long an idle global session (no connected agent, no activity) is kept before it's **permanently deleted** - the session, every one of its agents, and every one of its messages, removed together in one transaction. This is a single server-wide default; a per-session override is planned (see the project's issue tracker) but not yet implemented. |
| `SESSION_CLEANUP_INTERVAL` | `30m` | How often the idle-session deletion pass runs, per shard. Deliberately its own, much less frequent schedule than `SWEEP_EVERY`: sweeping is cheap and latency-sensitive, permanent deletion isn't something that needs to run every few seconds. |
| `POSTGRES_CACHE_TTL` | `10m` | How long the in-memory L1 cache of session-to-shard lookups stays valid before re-querying Postgres. The mapping never actually changes once assigned, so this is a defensive ceiling, not a correctness knob - useful to shorten on a fast local Postgres, or set to `0` to disable the cache entirely. |
| `PING_CHECK_INTERVAL` | `5m30s` | How often a background job checks every configured Postgres and MongoDB connection is actually alive (not just cached as healthy) - catches an outage even during a quiet period with no real traffic to surface one on its own. Failures are logged, not otherwise acted on. |

On a very small VM (1 shared vCPU, ~1GB RAM), lowering `RPC_RATE_LIMIT`/`MAX_INFLIGHT_RPCS`
and raising `SWEEP_EVERY` (e.g. to `2m`) trades a little latency for less CPU/DB load; none
of this needs a code change.

## Running with Docker

```
docker build -t relay-server -f server/Dockerfile server
docker run -d --name relay-server -p 5555:5555 \
  -e SQL_DSN="host=<postgres-host> user=relay password=<pw> dbname=relay port=5432 sslmode=disable" \
  -e MONGO_URLS="mongodb://<mongo-host>:27017" \
  relay-server
```

`server/docker-compose.yml` starts a local Postgres + two MongoDB instances for development
and integration testing - not meant to be the production database story by itself, but the
env vars above work identically against it (see `server/.env.example`).

## Running as a systemd service (no Docker)

Build the binary once (`cd server && CGO_ENABLED=0 go build -o /usr/local/bin/relay-server .`),
then:

```ini
# /etc/systemd/system/relay-server.service
[Unit]
Description=relay global-session server
After=network-online.target
Wants=network-online.target

[Service]
ExecStart=/usr/local/bin/relay-server
Restart=on-failure
RestartSec=2
Environment=PORT=5555
Environment=SQL_DSN=host=localhost user=relay password=relay dbname=relay port=5432 sslmode=disable
Environment=MONGO_URLS=mongodb://localhost:27017
# Tune for a small VM:
Environment=RPC_RATE_LIMIT=100
Environment=MAX_INFLIGHT_RPCS=8
Environment=SWEEP_EVERY=2m
DynamicUser=yes

[Install]
WantedBy=multi-user.target
```

```
sudo systemctl daemon-reload
sudo systemctl enable --now relay-server
```

## TLS via a reverse proxy

The server itself only speaks plain `ws://` - it has no TLS story of its own, deliberately
(that's a solved problem a reverse proxy handles better). [Caddy](https://caddyserver.com/)
gets automatic HTTPS from a two-line Caddyfile, which is the right amount of ops for a
free-tier VM:

```
# /etc/caddy/Caddyfile
relay.example.com {
    reverse_proxy localhost:5555
}
```

Once a proxy terminates TLS in front of it, clients must dial `wss://` instead of `ws://`. The
officially distributed `relay` binary already has this server's address and TLS baked in at
release build time (via the `RELAY_BASE_URL` GitHub secret + GoReleaser ldflags - see
`internal/cli/builtinserver.go`), so nothing needs to be set for it. Only someone running a
`relay` CLI built from source (`make build`/`go build .`) against a self-hosted server needs
`RELAY_GLOBAL_TLS=1` in the environment plus `--server=relay.example.com:443` (or whatever
port Caddy listens on) - the source build has no server baked in and is free to point at any
server, including this one.

## Data retention

Global sessions aren't kept forever. Two independent, periodic background jobs (both scheduled
on the same supervised `cron` service as the DB liveness check - see `server/package/cron`, so
a panic or a hang in one can never silently take the others down with it) run per configured
shard:

* **Sweep** (`SWEEP_EVERY`, default `30s`): expires overdue messages and reaps agents that have
  been disconnected longer than `DISCONNECT_GRACE`. State transitions only - nothing is deleted.
* **Idle-session cleanup** (`SESSION_CLEANUP_INTERVAL`, default `30m`): permanently deletes any
  session with no connected agent and no activity for `SESSION_MAX_AGE` (default `24h`) - the
  session document, every one of its agents, and every one of its messages, removed together in
  one real database transaction (all or nothing, never a partial delete).

Both are single server-wide settings today; there is no way to give one particular session a
longer or shorter lifetime yet (tracked as a future enhancement in the project's issue tracker).

## Out of scope

No horizontal scaling or multi-instance story: `relay-server` keeps its live-connection
registry and per-agent live tool state in a single process's memory, exactly as documented
in the Phase 1 plan. Running two instances behind a load balancer would split agents across
processes that can't see each other's connections - don't do that. A single small instance
per deployment is the supported shape.
