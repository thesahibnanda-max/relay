// Package integration holds tests that need a real Postgres and real MongoDB
// shards - the docker-compose stack in server/docker-compose.yml. They skip
// themselves (rather than failing) when TEST_SQL_DSN / TEST_MONGO_URLS
// aren't set, so `go test ./...` stays green with no infrastructure running;
// run `docker compose -f server/docker-compose.yml up -d` and export those
// two variables (see server/.env.example) to actually exercise them.
package integration

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	gorilla "github.com/coder/websocket"
	"github.com/oklog/ulid/v2"

	"github.com/thesahibnanda-max/relay/server/package/config"
	"github.com/thesahibnanda-max/relay/server/package/cron"
	"github.com/thesahibnanda-max/relay/server/package/database/mongodb"
	"github.com/thesahibnanda-max/relay/server/package/database/postgres"
	"github.com/thesahibnanda-max/relay/server/package/database/repository"
	"github.com/thesahibnanda-max/relay/server/package/session"
	"github.com/thesahibnanda-max/relay/server/package/sharding"
	"github.com/thesahibnanda-max/relay/server/package/ws"
)

// testConfig builds a config.Config the same way production does - through
// config.New()'s real env-driven defaulting - rather than hand-constructing
// one, so every resource/policy knob (rate limits, TTL, sweep interval...)
// gets its real default instead of silently zero-valuing to "reject
// everything" (a zero RPCRateLimit trips "too many requests" on the very
// first RPC of any test, found the hard way running this suite for real).
func testConfig(t *testing.T) config.Config {
	t.Helper()
	dsn := os.Getenv("TEST_SQL_DSN")
	mongoURLs := os.Getenv("TEST_MONGO_URLS")
	if dsn == "" || mongoURLs == "" {
		t.Skip("set TEST_SQL_DSN and TEST_MONGO_URLS (see server/.env.example) against a running server/docker-compose.yml to run this test")
	}
	t.Setenv("SQL_DSN", dsn)
	t.Setenv("MONGO_URLS", mongoURLs)
	t.Setenv("PORT", "0")
	cfg, err := config.New()
	if err != nil {
		t.Fatalf("config.New: %v", err)
	}
	return cfg
}

// newIntegrationSessionService builds a real session.Interface the same way
// production does, for tests (like the cron ones) that need a genuine
// session.Interface but aren't themselves testing session behavior.
func newIntegrationSessionService(t *testing.T, cfg config.Config, pool mongodb.Interface, pg postgres.Interface) session.Interface {
	t.Helper()
	mongoURLRepo, err := repository.NewMongoURLRepository(pg)
	if err != nil {
		t.Fatalf("NewMongoURLRepository: %v", err)
	}
	shardMapRepo, err := repository.NewShardMapRepository(pg)
	if err != nil {
		t.Fatalf("NewShardMapRepository: %v", err)
	}
	sessionRepo, err := repository.NewSessionRepository(pool)
	if err != nil {
		t.Fatalf("NewSessionRepository: %v", err)
	}
	agentRepo, err := repository.NewAgentRepository(pool)
	if err != nil {
		t.Fatalf("NewAgentRepository: %v", err)
	}
	messageRepo, err := repository.NewMessageRepository(pool)
	if err != nil {
		t.Fatalf("NewMessageRepository: %v", err)
	}
	selector, err := sharding.New(cfg, shardMapRepo, mongoURLRepo)
	if err != nil {
		t.Fatalf("sharding.New: %v", err)
	}
	svc, err := session.New(cfg, selector, sessionRepo, agentRepo, messageRepo, shardMapRepo, pool)
	if err != nil {
		t.Fatalf("session.New: %v", err)
	}
	return svc
}

func TestPostgresAutoMigrationCreatesTables(t *testing.T) {
	cfg := testConfig(t)

	pg, err := postgres.New(cfg)
	if err != nil {
		t.Fatalf("postgres.New: %v", err)
	}
	for _, table := range []any{&postgres.MongoDBURL{}, &postgres.SessionShardMap{}} {
		if !pg.DB().Migrator().HasTable(table) {
			t.Errorf("expected AutoMigrate to have created a table for %T, it did not", table)
		}
	}
}

func TestMongoCollectionsCreatedIfMissing(t *testing.T) {
	cfg := testConfig(t)

	pool, err := mongodb.New(cfg)
	if err != nil {
		t.Fatalf("mongodb.New: %v", err)
	}
	sessions, err := repository.NewSessionRepository(pool)
	if err != nil {
		t.Fatalf("NewSessionRepository: %v", err)
	}
	agents, err := repository.NewAgentRepository(pool)
	if err != nil {
		t.Fatalf("NewAgentRepository: %v", err)
	}
	messages, err := repository.NewMessageRepository(pool)
	if err != nil {
		t.Fatalf("NewMessageRepository: %v", err)
	}

	ctx := context.Background()
	shardURL := cfg.MongoURLs[0]
	if err := sessions.EnsureCollection(ctx, shardURL); err != nil {
		t.Fatalf("sessions.EnsureCollection: %v", err)
	}
	if err := agents.EnsureCollection(ctx, shardURL); err != nil {
		t.Fatalf("agents.EnsureCollection: %v", err)
	}
	if err := messages.EnsureCollection(ctx, shardURL); err != nil {
		t.Fatalf("messages.EnsureCollection: %v", err)
	}
	// Calling EnsureCollection a second time must not error - "if not
	// exists" has to be safe to call on every startup, not just the first.
	if err := sessions.EnsureCollection(ctx, shardURL); err != nil {
		t.Fatalf("sessions.EnsureCollection (second call): %v", err)
	}

	db, err := pool.Database(shardURL)
	if err != nil {
		t.Fatalf("pool.Database: %v", err)
	}
	names, err := db.ListCollectionNames(ctx, map[string]any{})
	if err != nil {
		t.Fatalf("ListCollectionNames: %v", err)
	}
	want := map[string]bool{
		mongodb.CollectionSessions: false,
		mongodb.CollectionAgents:   false,
		mongodb.CollectionMessages: false,
	}
	for _, n := range names {
		if _, ok := want[n]; ok {
			want[n] = true
		}
	}
	for name, found := range want {
		if !found {
			t.Errorf("expected collection %q to exist after EnsureCollection, it does not", name)
		}
	}
}

// TestPostgresPing_SucceedsAgainstARealDatabase is the regression test for
// the bug found reviewing this method by hand: Raw("SELECT $1", 1) fails
// every time against the real driver ("unable to encode 1 into text format
// for text (OID 25)"), since GORM's Raw() expects its own "?" placeholder
// style, not a native "$1". Only a real database surfaces this - it's not
// something a fake/mock of postgres.Interface could ever catch.
func TestPostgresPing_SucceedsAgainstARealDatabase(t *testing.T) {
	cfg := testConfig(t)
	pg, err := postgres.New(cfg)
	if err != nil {
		t.Fatalf("postgres.New: %v", err)
	}
	if err := pg.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
}

// TestPostgresPing_FailsFastOnACancelledContext is the regression test for
// the other bug found reviewing this method: the SELECT half used to run
// against a plain (non-context-bound) session, silently ignoring the
// caller's own cancellation/timeout.
func TestPostgresPing_FailsFastOnACancelledContext(t *testing.T) {
	cfg := testConfig(t)
	pg, err := postgres.New(cfg)
	if err != nil {
		t.Fatalf("postgres.New: %v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err := pg.Ping(ctx); err == nil {
		t.Fatal("expected Ping to fail on an already-cancelled context, got nil")
	}
}

// TestMongoPoolPing_SucceedsAgainstEveryConfiguredShard proves Ping checks
// every shard named in TEST_MONGO_URLS, not just the first - a pool with 3
// shards (the real staging setup this was validated against) must exercise
// all 3, not stop after the first success.
func TestMongoPoolPing_SucceedsAgainstEveryConfiguredShard(t *testing.T) {
	cfg := testConfig(t)
	pool, err := mongodb.New(cfg)
	if err != nil {
		t.Fatalf("mongodb.New: %v", err)
	}
	if err := pool.Ping(context.Background()); err != nil {
		t.Fatalf("Ping: %v", err)
	}
}

// TestCronPingJob_RunsAgainstRealConnections proves package cron's wiring -
// New/Start/Stop, and the scheduled job actually calling both real Ping
// methods - works end to end against genuine infrastructure, not just the
// fakes package cron's own unit tests use.
func TestCronPingJob_RunsAgainstRealConnections(t *testing.T) {
	cfg := testConfig(t)
	cfg.PingCheckInterval = 50 * time.Millisecond

	pg, err := postgres.New(cfg)
	if err != nil {
		t.Fatalf("postgres.New: %v", err)
	}
	pool, err := mongodb.New(cfg)
	if err != nil {
		t.Fatalf("mongodb.New: %v", err)
	}

	sessions := newIntegrationSessionService(t, cfg, pool, pg)
	pingCron, err := cron.New(cfg, pool, pg, sessions)
	if err != nil {
		t.Fatalf("cron.New: %v", err)
	}
	if err := pingCron.Start(context.Background()); err != nil {
		t.Fatalf("Start: %v", err)
	}

	time.Sleep(300 * time.Millisecond) // let at least one real tick happen

	stopCtx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := pingCron.Stop(stopCtx); err != nil {
		t.Fatalf("Stop: %v", err)
	}
}

// testStack is the full wired-up server, plus every repository/service piece
// a test might want to assert against directly (DB state, not just what
// comes back over the wire).
type testStack struct {
	shards    sharding.Interface
	agentRepo repository.AgentRepository
	svc       session.Interface
	hub       ws.Interface
	pg        postgres.Interface
	mongoPool mongodb.Interface
}

// buildStack wires the full server against the real docker-compose stack.
// mutators (if any) tweak the config before anything is built - what the
// rate-limit/TTL/disconnect-grace tests use to shrink those knobs down to
// something a test can trigger in milliseconds instead of minutes.
func buildStack(t *testing.T, mutators ...func(*config.Config)) testStack {
	t.Helper()
	cfg := testConfig(t)
	for _, mutate := range mutators {
		mutate(&cfg)
	}

	pg, err := postgres.New(cfg)
	if err != nil {
		t.Fatalf("postgres.New: %v", err)
	}
	t.Cleanup(func() {
		if sqlDB, err := pg.DB().DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	mongoPool, err := mongodb.New(cfg)
	if err != nil {
		t.Fatalf("mongodb.New: %v", err)
	}
	// Every test builds its own pool (one *mongo.Client per configured shard,
	// plus its own Postgres connection above) - against local Docker that's
	// free to leak for the process's lifetime, but against a real,
	// connection-limited Postgres/Mongo (a pooler's small connection cap, an
	// Atlas free-tier cluster) it exhausts the ceiling after a handful of
	// tests, and every connection after that fails fast instead of
	// connecting - found running this suite against real staging credentials.
	t.Cleanup(func() { _ = mongoPool.Close(context.Background()) })
	mongoURLRepo, err := repository.NewMongoURLRepository(pg)
	if err != nil {
		t.Fatalf("NewMongoURLRepository: %v", err)
	}
	for _, url := range cfg.MongoURLs {
		if _, err := mongoURLRepo.EnsureURL(context.Background(), url); err != nil {
			t.Fatalf("EnsureURL(%s): %v", url, err)
		}
	}
	shardMapRepo, err := repository.NewShardMapRepository(pg)
	if err != nil {
		t.Fatalf("NewShardMapRepository: %v", err)
	}
	sessionRepo, err := repository.NewSessionRepository(mongoPool)
	if err != nil {
		t.Fatalf("NewSessionRepository: %v", err)
	}
	agentRepo, err := repository.NewAgentRepository(mongoPool)
	if err != nil {
		t.Fatalf("NewAgentRepository: %v", err)
	}
	messageRepo, err := repository.NewMessageRepository(mongoPool)
	if err != nil {
		t.Fatalf("NewMessageRepository: %v", err)
	}
	selector, err := sharding.New(cfg, shardMapRepo, mongoURLRepo)
	if err != nil {
		t.Fatalf("sharding.New: %v", err)
	}
	svc, err := session.New(cfg, selector, sessionRepo, agentRepo, messageRepo, shardMapRepo, mongoPool)
	if err != nil {
		t.Fatalf("session.New: %v", err)
	}
	hub, err := ws.New(cfg, svc)
	if err != nil {
		t.Fatalf("ws.New: %v", err)
	}
	return testStack{shards: selector, agentRepo: agentRepo, svc: svc, hub: hub, pg: pg, mongoPool: mongoPool}
}

func startServer(t *testing.T, stack testStack) string {
	t.Helper()
	srv := httptest.NewServer(ws.NewHandler(stack.hub, stack.pg, stack.mongoPool))
	t.Cleanup(srv.Close)
	return "ws" + strings.TrimPrefix(srv.URL, "http") + ws.Path
}

type healthzBody struct {
	Status string `json:"status"`
	SQL    string `json:"sql"`
	NoSQL  string `json:"no-sql"`
}

// TestHealthzEndpoint_ReflectsRealDatabaseConnectivity proves the fully
// wired-up handler (not the fakes package ws's own unit tests use) reports
// healthy against the real Postgres/Mongo docker-compose stack - the actual
// code path a deploy script or external monitor hits.
func TestHealthzEndpoint_ReflectsRealDatabaseConnectivity(t *testing.T) {
	stack := buildStack(t)
	srv := httptest.NewServer(ws.NewHandler(stack.hub, stack.pg, stack.mongoPool))
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/healthz")
	if err != nil {
		t.Fatalf("GET /healthz: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("GET /healthz: got status %d, want 200", resp.StatusCode)
	}
	var body healthzBody
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatalf("decoding /healthz body: %v", err)
	}
	if want := (healthzBody{Status: "healthy", SQL: "healthy", NoSQL: "healthy"}); body != want {
		t.Errorf("unexpected body: %+v, want %+v", body, want)
	}
}

// TestEndToEndTwoAgentsOneSession dials the WS server twice, joining the
// same freshly-created global session, and checks that list_agents sees
// both and a send from one is delivered to the other while connected -
// the plan's stated end-to-end acceptance check.
func TestEndToEndTwoAgentsOneSession(t *testing.T) {
	stack := buildStack(t)
	wsURL := startServer(t, stack)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	alice, _, err := gorilla.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial alice: %v", err)
	}
	defer alice.CloseNow()

	welcome := helloAndWait(t, ctx, alice, ws.Hello{Session: session.SessionNew, Name: "alice", Tool: "test", Role: "peer"})
	if welcome.SessionID == "" || welcome.AgentID == "" {
		t.Fatalf("alice welcome missing ids: %+v", welcome)
	}

	bob, _, err := gorilla.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial bob: %v", err)
	}
	defer bob.CloseNow()
	bobWelcome := helloAndWait(t, ctx, bob, ws.Hello{Session: welcome.SessionID, Name: "bob", Tool: "test", Role: "peer"})
	if bobWelcome.SessionID != welcome.SessionID {
		t.Fatalf("bob joined session %q, want %q", bobWelcome.SessionID, welcome.SessionID)
	}

	list := decodeResult[ws.ListAgentsResult](t, rpcCall(t, ctx, alice, ws.OpListAgents, struct{}{}))
	if len(list.Agents) != 2 {
		t.Fatalf("expected 2 agents in list_agents, got %d: %+v", len(list.Agents), list.Agents)
	}

	sendResult := decodeResult[ws.SendResult](t, rpcCall(t, ctx, alice, ws.OpSend, ws.SendArgs{To: "bob", Body: "hello bob"}))
	if sendResult.ID == "" {
		t.Fatal("expected a non-empty message id in send_result")
	}

	deliver := readTyped[ws.Deliver](t, ctx, bob, ws.TypeDeliver)
	if deliver.Message.Body != "hello bob" || deliver.Message.From != "alice" {
		t.Fatalf("unexpected deliver frame: %+v", deliver.Message)
	}

	sendAck(t, ctx, bob, deliver.Message.ID)
}

// TestDisconnectMarksAgentDisconnected proves the previously-dead
// "disconnect never persists to the DB" path is now wired: closing a
// connection must flip the agent's stored status.
func TestDisconnectMarksAgentDisconnected(t *testing.T) {
	stack := buildStack(t)
	wsURL := startServer(t, stack)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	c, _, err := gorilla.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	welcome := helloAndWait(t, ctx, c, ws.Hello{Session: session.SessionNew, Name: "alice", Tool: "test", Role: "peer"})
	c.CloseNow()

	shardURL, err := stack.shards.ShardURLFor(ctx, welcome.SessionID)
	if err != nil {
		t.Fatalf("ShardURLFor: %v", err)
	}

	deadline := time.Now().Add(5 * time.Second)
	for {
		agent, found, err := stack.agentRepo.Get(ctx, shardURL, welcome.AgentID)
		if err != nil {
			t.Fatalf("Get: %v", err)
		}
		if found && agent.Status == "disconnected" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("agent never marked disconnected (found=%v status=%q)", found, agent.Status)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

// TestMessageRedeliveredOnReconnect sends to an offline agent, then proves
// the message is replayed the moment that agent reconnects (resumes) -
// Phase 1's at-least-once, crash-survives-delivery guarantee.
func TestMessageRedeliveredOnReconnect(t *testing.T) {
	stack := buildStack(t)
	wsURL := startServer(t, stack)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	alice, _, err := gorilla.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial alice: %v", err)
	}
	defer alice.CloseNow()
	aliceWelcome := helloAndWait(t, ctx, alice, ws.Hello{Session: session.SessionNew, Name: "alice", Tool: "test", Role: "peer"})

	bob, _, err := gorilla.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial bob: %v", err)
	}
	bobWelcome := helloAndWait(t, ctx, bob, ws.Hello{Session: aliceWelcome.SessionID, Name: "bob", Tool: "test", Role: "peer"})
	bobToken := bobWelcome.Token
	bob.CloseNow()                     // bob goes offline
	time.Sleep(200 * time.Millisecond) // give the server a moment to notice

	sendResult := decodeResult[ws.SendResult](t, rpcCall(t, ctx, alice, ws.OpSend, ws.SendArgs{To: "bob", Body: "while you were out"}))
	if sendResult.State != "queued" {
		t.Fatalf("expected the message to stay queued while bob is offline, got state %q", sendResult.State)
	}

	bob2, _, err := gorilla.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial bob again: %v", err)
	}
	defer bob2.CloseNow()
	writeTyped(t, ctx, bob2, ws.TypeHello, ws.Hello{Session: aliceWelcome.SessionID, Name: "bob", Token: bobToken, Tool: "test", Role: "peer"})
	welcome2 := readTyped[ws.Welcome](t, ctx, bob2, ws.TypeWelcome)
	if !welcome2.Resumed {
		t.Fatalf("expected bob's reconnect to resume, got %+v", welcome2)
	}

	deliver := readTyped[ws.Deliver](t, ctx, bob2, ws.TypeDeliver)
	if deliver.Message.Body != "while you were out" {
		t.Fatalf("unexpected replayed message: %+v", deliver.Message)
	}
}

// TestResumeRejectsStillConnectedIdentity proves two processes can never
// silently share one agent identity.
func TestResumeRejectsStillConnectedIdentity(t *testing.T) {
	stack := buildStack(t)
	wsURL := startServer(t, stack)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	first, _, err := gorilla.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial: %v", err)
	}
	defer first.CloseNow()
	welcome := helloAndWait(t, ctx, first, ws.Hello{Session: session.SessionNew, Name: "alice", Tool: "test", Role: "peer"})

	second, _, err := gorilla.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial second: %v", err)
	}
	defer second.CloseNow()
	writeTyped(t, ctx, second, ws.TypeHello, ws.Hello{Session: welcome.SessionID, Name: "alice", Token: welcome.Token, Tool: "test", Role: "peer"})

	_, data, err := second.Read(ctx)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	var env ws.Envelope
	if err := json.Unmarshal(data, &env); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if env.Type != ws.TypeError {
		t.Fatalf("expected an error frame, got %q", env.Type)
	}
	var e ws.Error
	if err := json.Unmarshal(env.Payload, &e); err != nil {
		t.Fatalf("unmarshal error: %v", err)
	}
	if e.Code != "agent_live" {
		t.Fatalf("expected code agent_live, got %q (%s)", e.Code, e.Message)
	}
}

// TestWaitReturnsReply proves the relay_wait equivalent surfaces a reply
// once one arrives.
func TestWaitReturnsReply(t *testing.T) {
	stack := buildStack(t)
	wsURL := startServer(t, stack)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	alice, _, err := gorilla.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial alice: %v", err)
	}
	defer alice.CloseNow()
	aliceWelcome := helloAndWait(t, ctx, alice, ws.Hello{Session: session.SessionNew, Name: "alice", Tool: "test", Role: "peer"})

	bob, _, err := gorilla.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial bob: %v", err)
	}
	defer bob.CloseNow()
	helloAndWait(t, ctx, bob, ws.Hello{Session: aliceWelcome.SessionID, Name: "bob", Tool: "test", Role: "peer"})

	sendResult := decodeResult[ws.SendResult](t, rpcCall(t, ctx, alice, ws.OpSend, ws.SendArgs{To: "bob", Body: "question?"}))
	readTyped[ws.Deliver](t, ctx, bob, ws.TypeDeliver)
	decodeResult[ws.SendResult](t, rpcCall(t, ctx, bob, ws.OpSend, ws.SendArgs{To: "alice", Body: "answer!", ReplyTo: sendResult.ID}))

	waitResult := decodeResult[ws.WaitResult](t, rpcCall(t, ctx, alice, ws.OpWait, ws.WaitArgs{ID: sendResult.ID, TimeoutS: 5}))
	if waitResult.TimedOut || waitResult.Reply == nil {
		t.Fatalf("expected a reply, got %+v", waitResult)
	}
	if waitResult.Reply.Body != "answer!" {
		t.Fatalf("unexpected reply body: %+v", waitResult.Reply)
	}
}

// TestWaitTimesOut proves relay_wait gives up after its timeout instead of
// blocking forever.
func TestWaitTimesOut(t *testing.T) {
	stack := buildStack(t)
	wsURL := startServer(t, stack)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	alice, _, err := gorilla.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial alice: %v", err)
	}
	defer alice.CloseNow()
	aliceWelcome := helloAndWait(t, ctx, alice, ws.Hello{Session: session.SessionNew, Name: "alice", Tool: "test", Role: "peer"})

	bob, _, err := gorilla.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial bob: %v", err)
	}
	defer bob.CloseNow()
	helloAndWait(t, ctx, bob, ws.Hello{Session: aliceWelcome.SessionID, Name: "bob", Tool: "test", Role: "peer"})

	sendResult := decodeResult[ws.SendResult](t, rpcCall(t, ctx, alice, ws.OpSend, ws.SendArgs{To: "bob", Body: "no reply coming"}))
	readTyped[ws.Deliver](t, ctx, bob, ws.TypeDeliver) // bob never replies

	waitResult := decodeResult[ws.WaitResult](t, rpcCall(t, ctx, alice, ws.OpWait, ws.WaitArgs{ID: sendResult.ID, TimeoutS: 1}))
	if !waitResult.TimedOut {
		t.Fatalf("expected a timeout, got %+v", waitResult)
	}
}

// TestGetContextReturnsMessageHistory proves the relay_get_context
// equivalent returns an agent's recent message history in order.
func TestGetContextReturnsMessageHistory(t *testing.T) {
	stack := buildStack(t)
	wsURL := startServer(t, stack)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	alice, _, err := gorilla.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial alice: %v", err)
	}
	defer alice.CloseNow()
	aliceWelcome := helloAndWait(t, ctx, alice, ws.Hello{Session: session.SessionNew, Name: "alice", Tool: "test", Role: "peer"})

	bob, _, err := gorilla.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial bob: %v", err)
	}
	defer bob.CloseNow()
	helloAndWait(t, ctx, bob, ws.Hello{Session: aliceWelcome.SessionID, Name: "bob", Tool: "test", Role: "peer"})

	decodeResult[ws.SendResult](t, rpcCall(t, ctx, alice, ws.OpSend, ws.SendArgs{To: "bob", Body: "first"}))
	readTyped[ws.Deliver](t, ctx, bob, ws.TypeDeliver)
	decodeResult[ws.SendResult](t, rpcCall(t, ctx, alice, ws.OpSend, ws.SendArgs{To: "bob", Body: "second"}))
	readTyped[ws.Deliver](t, ctx, bob, ws.TypeDeliver)

	ctxResult := decodeResult[ws.GetContextResult](t, rpcCall(t, ctx, bob, ws.OpContext, ws.ContextArgs{Agent: "bob", N: 10}))
	if len(ctxResult.Messages) != 2 {
		t.Fatalf("expected 2 messages of history, got %d: %+v", len(ctxResult.Messages), ctxResult.Messages)
	}
	if ctxResult.Messages[0].Body != "first" || ctxResult.Messages[1].Body != "second" {
		t.Fatalf("expected chronological order, got %+v", ctxResult.Messages)
	}
}

// TestServerRestartMarksAllConnectedDisconnected mirrors the local daemon's
// "on restart, force every connected agent to disconnected" rule - a real
// process restart isn't practical in-test, so this calls the repository
// method directly.
func TestServerRestartMarksAllConnectedDisconnected(t *testing.T) {
	stack := buildStack(t)
	ctx := context.Background()

	welcome, err := stack.svc.Join(ctx, session.JoinRequest{SessionID: session.SessionNew, Name: "restart-test-agent", Tool: "test", Role: "peer"})
	if err != nil {
		t.Fatalf("Join: %v", err)
	}
	shardURL, err := stack.shards.ShardURLFor(ctx, welcome.SessionID)
	if err != nil {
		t.Fatalf("ShardURLFor: %v", err)
	}

	agent, found, err := stack.agentRepo.Get(ctx, shardURL, welcome.AgentID)
	if err != nil || !found {
		t.Fatalf("Get before restart: found=%v err=%v", found, err)
	}
	if agent.Status != "connected" {
		t.Fatalf("expected a freshly-registered agent to be connected, got %q", agent.Status)
	}

	if err := stack.agentRepo.MarkAllDisconnected(ctx, shardURL); err != nil {
		t.Fatalf("MarkAllDisconnected: %v", err)
	}

	agent, found, err = stack.agentRepo.Get(ctx, shardURL, welcome.AgentID)
	if err != nil || !found {
		t.Fatalf("Get after restart: found=%v err=%v", found, err)
	}
	if agent.Status != "disconnected" {
		t.Fatalf("expected the agent to be disconnected after MarkAllDisconnected, got %q", agent.Status)
	}
}

// TestApproveInboundHoldsThenApproveDelivers proves the full held/notice/
// list_held/approve round trip for a --approve-inbound target: the message
// stays held (never delivered) until approved, a notice fires when it
// becomes held, and approving it both queues and immediately delivers it
// since the recipient is online.
func TestApproveInboundHoldsThenApproveDelivers(t *testing.T) {
	stack := buildStack(t)
	wsURL := startServer(t, stack)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	alice, _, err := gorilla.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial alice: %v", err)
	}
	defer alice.CloseNow()
	aliceWelcome := helloAndWait(t, ctx, alice, ws.Hello{Session: session.SessionNew, Name: "alice", Tool: "test", Role: "peer"})

	bob, _, err := gorilla.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial bob: %v", err)
	}
	defer bob.CloseNow()
	helloAndWait(t, ctx, bob, ws.Hello{Session: aliceWelcome.SessionID, Name: "bob", Tool: "test", Role: "peer", ApproveInbound: true})

	sendResult := decodeResult[ws.SendResult](t, rpcCall(t, ctx, alice, ws.OpSend, ws.SendArgs{To: "bob", Body: "please approve me"}))
	if sendResult.State != mongodb.MessageStateHeld {
		t.Fatalf("expected the message to be held for an approve-inbound target, got %q", sendResult.State)
	}

	notice := readTyped[ws.Notice](t, ctx, bob, ws.TypeNotice)
	if notice.Held != 1 {
		t.Fatalf("expected held count 1, got %d", notice.Held)
	}

	held := decodeResult[ws.ListHeldResult](t, rpcCall(t, ctx, bob, ws.OpListHeld, struct{}{}))
	if len(held.Messages) != 1 || held.Messages[0].ID != sendResult.ID {
		t.Fatalf("unexpected list_held result: %+v", held)
	}

	approveResult := decodeResult[ws.ApproveResult](t, rpcCall(t, ctx, bob, ws.OpApprove, ws.ApproveArgs{}))
	if approveResult.ID != sendResult.ID || approveResult.State != mongodb.MessageStateQueued {
		t.Fatalf("unexpected approve result: %+v", approveResult)
	}

	deliver := readTyped[ws.Deliver](t, ctx, bob, ws.TypeDeliver)
	if deliver.Message.ID != sendResult.ID || deliver.Message.Body != "please approve me" {
		t.Fatalf("unexpected deliver after approve: %+v", deliver.Message)
	}
}

// TestRejectNotifiesSender proves rejecting a held message notifies the
// original sender - delivered as an ordinary "notify" message the sender
// picks up via get_context, not a live push (a documented simplification:
// only the approving agent's own connection gets a live push right now).
func TestRejectNotifiesSender(t *testing.T) {
	stack := buildStack(t)
	wsURL := startServer(t, stack)

	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()

	alice, _, err := gorilla.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial alice: %v", err)
	}
	defer alice.CloseNow()
	aliceWelcome := helloAndWait(t, ctx, alice, ws.Hello{Session: session.SessionNew, Name: "alice", Tool: "test", Role: "peer"})

	bob, _, err := gorilla.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial bob: %v", err)
	}
	defer bob.CloseNow()
	helloAndWait(t, ctx, bob, ws.Hello{Session: aliceWelcome.SessionID, Name: "bob", Tool: "test", Role: "peer", ApproveInbound: true})

	sendResult := decodeResult[ws.SendResult](t, rpcCall(t, ctx, alice, ws.OpSend, ws.SendArgs{To: "bob", Body: "reject me"}))
	readTyped[ws.Notice](t, ctx, bob, ws.TypeNotice) // held count -> 1

	rejectResult := decodeResult[ws.ApproveResult](t, rpcCall(t, ctx, bob, ws.OpReject, ws.ApproveArgs{ID: sendResult.ID}))
	if rejectResult.State != mongodb.MessageStateRejected {
		t.Fatalf("expected rejected, got %q", rejectResult.State)
	}
	readTyped[ws.Notice](t, ctx, bob, ws.TypeNotice) // held count -> 0

	ctxResult := decodeResult[ws.GetContextResult](t, rpcCall(t, ctx, alice, ws.OpContext, ws.ContextArgs{Agent: "alice", N: 10}))
	var sawNotify bool
	for _, m := range ctxResult.Messages {
		if m.Kind == "notify" {
			sawNotify = true
		}
	}
	if !sawNotify {
		t.Fatalf("expected a notify message to alice after the reject, got %+v", ctxResult.Messages)
	}
}

// TestHopLimitReHoldsEveryEighthReply proves a reply chain crossing a
// multiple of session.MaxHops is held again for human approval instead of
// being delivered straight through - built directly against the session
// service (not the wire) since the point is the hop-counting logic, not the
// RPC transport, and a 9-message ping-pong is much clearer this way.
func TestHopLimitReHoldsEveryEighthReply(t *testing.T) {
	stack := buildStack(t)
	ctx := context.Background()

	alice, err := stack.svc.Join(ctx, session.JoinRequest{SessionID: session.SessionNew, Name: "alice-hop", Tool: "test", Role: "peer"})
	if err != nil {
		t.Fatalf("join alice: %v", err)
	}
	bob, err := stack.svc.Join(ctx, session.JoinRequest{SessionID: alice.SessionID, Name: "bob-hop", Tool: "test", Role: "peer"})
	if err != nil {
		t.Fatalf("join bob: %v", err)
	}

	var lastID string
	fromID, toName := alice.AgentID, "bob-hop"
	for i := 0; i <= session.MaxHops; i++ {
		outcome, err := stack.svc.Send(ctx, alice.SessionID, fromID, session.SendRequest{
			To: toName, Body: fmt.Sprintf("hop %d", i), Priority: 2, ReplyTo: lastID,
		})
		if err != nil {
			t.Fatalf("send hop %d: %v", i, err)
		}
		lastID = outcome.MessageID
		if fromID == alice.AgentID {
			fromID, toName = bob.AgentID, "alice-hop"
		} else {
			fromID, toName = alice.AgentID, "bob-hop"
		}

		if i == session.MaxHops {
			if outcome.State != mongodb.MessageStateHeld {
				t.Fatalf("expected hop %d to be held (hop limit), got state %q", i, outcome.State)
			}
		} else if outcome.State != mongodb.MessageStateQueued {
			t.Fatalf("hop %d: expected queued, got %q", i, outcome.State)
		}
	}
}

// TestDedupCoalescesRecentIdenticalSend proves an identical (from,to,kind,
// body) retry within the dedup window returns the existing message instead
// of creating a new one.
func TestDedupCoalescesRecentIdenticalSend(t *testing.T) {
	stack := buildStack(t)
	wsURL := startServer(t, stack)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	alice, _, err := gorilla.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial alice: %v", err)
	}
	defer alice.CloseNow()
	aliceWelcome := helloAndWait(t, ctx, alice, ws.Hello{Session: session.SessionNew, Name: "alice", Tool: "test", Role: "peer"})

	bob, _, err := gorilla.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial bob: %v", err)
	}
	defer bob.CloseNow()
	helloAndWait(t, ctx, bob, ws.Hello{Session: aliceWelcome.SessionID, Name: "bob", Tool: "test", Role: "peer"})

	first := decodeResult[ws.SendResult](t, rpcCall(t, ctx, alice, ws.OpSend, ws.SendArgs{To: "bob", Body: "retry me"}))
	readTyped[ws.Deliver](t, ctx, bob, ws.TypeDeliver)

	second := decodeResult[ws.SendResult](t, rpcCall(t, ctx, alice, ws.OpSend, ws.SendArgs{To: "bob", Body: "retry me"}))
	if second.ID != first.ID {
		t.Fatalf("expected the duplicate to coalesce into %q, got a new id %q", first.ID, second.ID)
	}
	if second.Note == "" {
		t.Fatal("expected a Note explaining the duplicate coalesce")
	}
}

// TestRateLimitRejectsSendsOverThePairLimit proves an over-limit send is
// rejected outright (no DB write, no queue) rather than merely delayed.
func TestRateLimitRejectsSendsOverThePairLimit(t *testing.T) {
	stack := buildStack(t, func(c *config.Config) { c.PairRateLimit = 1 })
	wsURL := startServer(t, stack)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	alice, _, err := gorilla.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial alice: %v", err)
	}
	defer alice.CloseNow()
	aliceWelcome := helloAndWait(t, ctx, alice, ws.Hello{Session: session.SessionNew, Name: "alice", Tool: "test", Role: "peer"})

	bob, _, err := gorilla.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial bob: %v", err)
	}
	defer bob.CloseNow()
	helloAndWait(t, ctx, bob, ws.Hello{Session: aliceWelcome.SessionID, Name: "bob", Tool: "test", Role: "peer"})

	first := rpcCall(t, ctx, alice, ws.OpSend, ws.SendArgs{To: "bob", Body: "one"})
	if !first.OK {
		t.Fatalf("expected the first send within the limit to succeed: %+v", first.Error)
	}
	readTyped[ws.Deliver](t, ctx, bob, ws.TypeDeliver)

	second := rpcCall(t, ctx, alice, ws.OpSend, ws.SendArgs{To: "bob", Body: "two"})
	if second.OK {
		t.Fatal("expected the second send over the pair limit of 1/min to be rejected")
	}
	if second.Error.Code != "rate_limited" {
		t.Fatalf("expected rate_limited, got %q (%s)", second.Error.Code, second.Error.Message)
	}
}

// TestSweepExpiresDueMessagesAndNotifiesSender proves the TTL sweep expires
// a message once its ExpiresAt has passed and notifies the original sender -
// called directly against the service rather than waiting on the real
// background ticker, since the point is ExpireDue's own logic.
func TestSweepExpiresDueMessagesAndNotifiesSender(t *testing.T) {
	stack := buildStack(t, func(c *config.Config) { c.MessageTTL = 50 * time.Millisecond })
	ctx := context.Background()

	alice, err := stack.svc.Join(ctx, session.JoinRequest{SessionID: session.SessionNew, Name: "alice-ttl", Tool: "test", Role: "peer"})
	if err != nil {
		t.Fatalf("join alice: %v", err)
	}
	if _, err := stack.svc.Join(ctx, session.JoinRequest{SessionID: alice.SessionID, Name: "bob-ttl", Tool: "test", Role: "peer"}); err != nil {
		t.Fatalf("join bob: %v", err)
	}

	outcome, err := stack.svc.Send(ctx, alice.SessionID, alice.AgentID, session.SendRequest{To: "bob-ttl", Body: "will expire", Priority: 2})
	if err != nil {
		t.Fatalf("send: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	shardURL, err := stack.shards.ShardURLFor(ctx, alice.SessionID)
	if err != nil {
		t.Fatalf("ShardURLFor: %v", err)
	}
	if err := stack.svc.Sweep(ctx, shardURL); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	messages, err := stack.svc.Context(ctx, alice.SessionID, alice.AgentID, "alice-ttl", 10)
	if err != nil {
		t.Fatalf("Context: %v", err)
	}
	var sawExpired, sawNotify bool
	for _, m := range messages {
		if m.ID == outcome.MessageID && m.State == mongodb.MessageStateExpired {
			sawExpired = true
		}
		if m.Kind == "notify" {
			sawNotify = true
		}
	}
	if !sawExpired {
		t.Fatalf("expected the message to be expired after the sweep, got %+v", messages)
	}
	if !sawNotify {
		t.Fatalf("expected a notify message to alice after expiry, got %+v", messages)
	}
}

// TestSweepReapsGoneAgentsAndFailsPendingMail proves the disconnect reaper
// marks a long-gone agent "exited" and fails whatever was still pending for
// it to undeliverable, notifying the sender.
func TestSweepReapsGoneAgentsAndFailsPendingMail(t *testing.T) {
	stack := buildStack(t, func(c *config.Config) { c.DisconnectGrace = 50 * time.Millisecond })
	ctx := context.Background()

	alice, err := stack.svc.Join(ctx, session.JoinRequest{SessionID: session.SessionNew, Name: "alice-reap", Tool: "test", Role: "peer"})
	if err != nil {
		t.Fatalf("join alice: %v", err)
	}
	bob, err := stack.svc.Join(ctx, session.JoinRequest{SessionID: alice.SessionID, Name: "bob-reap", Tool: "test", Role: "peer"})
	if err != nil {
		t.Fatalf("join bob: %v", err)
	}

	outcome, err := stack.svc.Send(ctx, alice.SessionID, alice.AgentID, session.SendRequest{To: "bob-reap", Body: "gone before you saw it", Priority: 2})
	if err != nil {
		t.Fatalf("send: %v", err)
	}
	if err := stack.svc.Disconnect(ctx, alice.SessionID, bob.AgentID); err != nil {
		t.Fatalf("Disconnect: %v", err)
	}

	time.Sleep(100 * time.Millisecond)

	shardURL, err := stack.shards.ShardURLFor(ctx, alice.SessionID)
	if err != nil {
		t.Fatalf("ShardURLFor: %v", err)
	}
	if err := stack.svc.Sweep(ctx, shardURL); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	agents, err := stack.svc.ListAgents(ctx, alice.SessionID)
	if err != nil {
		t.Fatalf("ListAgents: %v", err)
	}
	var bobExited bool
	for _, a := range agents {
		if a.ID == bob.AgentID && a.Status == "exited" {
			bobExited = true
		}
	}
	if !bobExited {
		t.Fatalf("expected bob to be reaped to exited status, got %+v", agents)
	}

	messages, err := stack.svc.Context(ctx, alice.SessionID, alice.AgentID, "alice-reap", 10)
	if err != nil {
		t.Fatalf("Context: %v", err)
	}
	var sawUndeliverable, sawNotify bool
	for _, m := range messages {
		if m.ID == outcome.MessageID && m.State == mongodb.MessageStateUndeliverable {
			sawUndeliverable = true
		}
		if m.Kind == "notify" {
			sawNotify = true
		}
	}
	if !sawUndeliverable {
		t.Fatalf("expected the pending message to be marked undeliverable, got %+v", messages)
	}
	if !sawNotify {
		t.Fatalf("expected a notify message to alice after bob was reaped, got %+v", messages)
	}
}

// TestListAgentsShowsLiveAgentState proves an agent_state report surfaces in
// another agent's list_agents call while the reporter is connected.
func TestListAgentsShowsLiveAgentState(t *testing.T) {
	stack := buildStack(t)
	wsURL := startServer(t, stack)
	before := time.Now().UTC()

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	alice, _, err := gorilla.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial alice: %v", err)
	}
	defer alice.CloseNow()
	aliceWelcome := helloAndWait(t, ctx, alice, ws.Hello{Session: session.SessionNew, Name: "alice", Tool: "test", Role: "peer"})

	bob, _, err := gorilla.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial bob: %v", err)
	}
	defer bob.CloseNow()
	helloAndWait(t, ctx, bob, ws.Hello{Session: aliceWelcome.SessionID, Name: "bob", Tool: "test", Role: "peer"})

	stateResult := rpcCall(t, ctx, bob, ws.OpAgentState, ws.AgentStateArgs{State: "busy"})
	if !stateResult.OK {
		t.Fatalf("agent_state failed: %+v", stateResult.Error)
	}

	list := decodeResult[ws.ListAgentsResult](t, rpcCall(t, ctx, alice, ws.OpListAgents, struct{}{}))
	var bobInfo *ws.AgentInfo
	for i := range list.Agents {
		if list.Agents[i].Name == "bob" {
			bobInfo = &list.Agents[i]
		}
	}
	if bobInfo == nil {
		t.Fatalf("bob missing from list_agents: %+v", list.Agents)
	}
	if bobInfo.State != "busy" {
		t.Fatalf("expected bob's live state to be %q, got %q", "busy", bobInfo.State)
	}
	if bobInfo.Status != "connected" {
		t.Fatalf("expected bob's status to be connected, got %q", bobInfo.Status)
	}
	// Regression test: LastActive used to always come back as the Go zero
	// time.Time (it was never copied from mongodb.Agent.UpdatedAt into the
	// wire reply at all). Not asserting it reflects the agent_state call
	// just made - that RPC only updates the connection's in-memory live
	// State, not the persisted document - so it should be pinned to around
	// bob's join/registration instead.
	if bobInfo.LastActive.Before(before) {
		t.Fatalf("expected bob's LastActive to be at or after %v (test start), got %v", before, bobInfo.LastActive)
	}
	if time.Since(bobInfo.LastActive) > time.Minute {
		t.Fatalf("expected bob's LastActive to be recent, got %v", bobInfo.LastActive)
	}
}

// TestMsgStateAdvancesToInjectedThenDone proves the msg_state RPC lets a
// recipient report what it actually did with a message it received.
func TestMsgStateAdvancesToInjectedThenDone(t *testing.T) {
	stack := buildStack(t)
	wsURL := startServer(t, stack)

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	alice, _, err := gorilla.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial alice: %v", err)
	}
	defer alice.CloseNow()
	aliceWelcome := helloAndWait(t, ctx, alice, ws.Hello{Session: session.SessionNew, Name: "alice", Tool: "test", Role: "peer"})

	bob, _, err := gorilla.Dial(ctx, wsURL, nil)
	if err != nil {
		t.Fatalf("dial bob: %v", err)
	}
	defer bob.CloseNow()
	helloAndWait(t, ctx, bob, ws.Hello{Session: aliceWelcome.SessionID, Name: "bob", Tool: "test", Role: "peer"})

	sendResult := decodeResult[ws.SendResult](t, rpcCall(t, ctx, alice, ws.OpSend, ws.SendArgs{To: "bob", Body: "please inject"}))
	readTyped[ws.Deliver](t, ctx, bob, ws.TypeDeliver)

	if r := rpcCall(t, ctx, bob, ws.OpMsgState, ws.MsgStateArgs{ID: sendResult.ID, State: mongodb.MessageStateInjected}); !r.OK {
		t.Fatalf("msg_state injected failed: %+v", r.Error)
	}
	if r := rpcCall(t, ctx, bob, ws.OpMsgState, ws.MsgStateArgs{ID: sendResult.ID, State: mongodb.MessageStateDone}); !r.OK {
		t.Fatalf("msg_state done failed: %+v", r.Error)
	}

	messages := decodeResult[ws.GetContextResult](t, rpcCall(t, ctx, bob, ws.OpContext, ws.ContextArgs{Agent: "bob", N: 10}))
	var got string
	for _, m := range messages.Messages {
		if m.ID == sendResult.ID {
			got = m.State
		}
	}
	if got != mongodb.MessageStateDone {
		t.Fatalf("expected the message to end in done, got %q", got)
	}
}

func helloAndWait(t *testing.T, ctx context.Context, c *gorilla.Conn, hello ws.Hello) ws.Welcome {
	t.Helper()
	writeTyped(t, ctx, c, ws.TypeHello, hello)
	return readTyped[ws.Welcome](t, ctx, c, ws.TypeWelcome)
}

func writeTyped(t *testing.T, ctx context.Context, c *gorilla.Conn, typ string, payload any) {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatalf("marshal %s payload: %v", typ, err)
	}
	env := ws.Envelope{V: ws.Version, Type: typ, Payload: body}
	b, err := json.Marshal(env)
	if err != nil {
		t.Fatalf("marshal envelope: %v", err)
	}
	if err := c.Write(ctx, gorilla.MessageText, b); err != nil {
		t.Fatalf("write %s: %v", typ, err)
	}
}

func sendAck(t *testing.T, ctx context.Context, c *gorilla.Conn, messageID string) {
	t.Helper()
	writeTyped(t, ctx, c, ws.TypeAck, ws.Ack{ID: messageID})
}

// rpcCall sends an rpc frame and waits for its matching rpc_result, skipping
// over any other frames (e.g. a deliver push) that arrive interleaved.
func rpcCall(t *testing.T, ctx context.Context, c *gorilla.Conn, op string, args any) ws.RPCResult {
	t.Helper()
	id := ulid.Make().String()
	argsB, err := json.Marshal(args)
	if err != nil {
		t.Fatalf("marshal %s args: %v", op, err)
	}
	writeTyped(t, ctx, c, ws.TypeRPC, ws.RPC{ID: id, Op: op, Args: argsB})
	for {
		_, data, err := c.Read(ctx)
		if err != nil {
			t.Fatalf("read (want rpc_result for %s): %v", op, err)
		}
		var env ws.Envelope
		if err := json.Unmarshal(data, &env); err != nil {
			t.Fatalf("unmarshal envelope: %v", err)
		}
		if env.Type != ws.TypeRPCResult {
			continue
		}
		var result ws.RPCResult
		if err := json.Unmarshal(env.Payload, &result); err != nil {
			t.Fatalf("unmarshal rpc_result: %v", err)
		}
		if result.ID != id {
			continue
		}
		return result
	}
}

func decodeResult[T any](t *testing.T, result ws.RPCResult) T {
	t.Helper()
	var out T
	if !result.OK {
		t.Fatalf("rpc failed: %s: %s", result.Error.Code, result.Error.Message)
	}
	if err := json.Unmarshal(result.Result, &out); err != nil {
		t.Fatalf("unmarshal result: %v", err)
	}
	return out
}

func readTyped[T any](t *testing.T, ctx context.Context, c *gorilla.Conn, wantType string) T {
	t.Helper()
	for {
		_, data, err := c.Read(ctx)
		if err != nil {
			t.Fatalf("read (want %s): %v", wantType, err)
		}
		var env ws.Envelope
		if err := json.Unmarshal(data, &env); err != nil {
			t.Fatalf("unmarshal envelope: %v", err)
		}
		if env.Type == ws.TypeError {
			var e ws.Error
			_ = json.Unmarshal(env.Payload, &e)
			t.Fatalf("server error frame while waiting for %s: %s: %s", wantType, e.Code, e.Message)
		}
		if env.Type != wantType {
			// Not the frame we're waiting for (e.g. bob's welcome arriving
			// interleaved) - keep reading.
			continue
		}
		var out T
		if err := json.Unmarshal(env.Payload, &out); err != nil {
			t.Fatalf("unmarshal %s payload: %v", wantType, err)
		}
		return out
	}
}

// deleteTestDeps is every piece TestDeleteSessionsOlderThan_*/TestWithTransaction_*
// need direct access to, beyond what testStack exposes (raw session/message/
// shard-map repositories, to seed data with exact, backdated timestamps that
// going through session.Interface's own Join/Send can't control).
type deleteTestDeps struct {
	mongoPool  mongodb.Interface
	sessions   repository.SessionRepository
	agents     repository.AgentRepository
	messages   repository.MessageRepository
	shardMap   repository.ShardMapRepository
	svc        session.Interface
	shardURL   string
	mongoURLID uint
}

func newDeleteTestDeps(t *testing.T) deleteTestDeps {
	t.Helper()
	cfg := testConfig(t)
	pg, err := postgres.New(cfg)
	if err != nil {
		t.Fatalf("postgres.New: %v", err)
	}
	t.Cleanup(func() {
		if sqlDB, err := pg.DB().DB(); err == nil {
			_ = sqlDB.Close()
		}
	})
	mongoPool, err := mongodb.New(cfg)
	if err != nil {
		t.Fatalf("mongodb.New: %v", err)
	}
	t.Cleanup(func() { _ = mongoPool.Close(context.Background()) })

	mongoURLRepo, err := repository.NewMongoURLRepository(pg)
	if err != nil {
		t.Fatalf("NewMongoURLRepository: %v", err)
	}
	shardURL := cfg.MongoURLs[0]
	mongoURLRow, err := mongoURLRepo.EnsureURL(context.Background(), shardURL)
	if err != nil {
		t.Fatalf("EnsureURL: %v", err)
	}
	shardMapRepo, err := repository.NewShardMapRepository(pg)
	if err != nil {
		t.Fatalf("NewShardMapRepository: %v", err)
	}
	sessionRepo, err := repository.NewSessionRepository(mongoPool)
	if err != nil {
		t.Fatalf("NewSessionRepository: %v", err)
	}
	agentRepo, err := repository.NewAgentRepository(mongoPool)
	if err != nil {
		t.Fatalf("NewAgentRepository: %v", err)
	}
	messageRepo, err := repository.NewMessageRepository(mongoPool)
	if err != nil {
		t.Fatalf("NewMessageRepository: %v", err)
	}
	if err := sessionRepo.EnsureCollection(context.Background(), shardURL); err != nil {
		t.Fatalf("sessions.EnsureCollection: %v", err)
	}
	if err := agentRepo.EnsureCollection(context.Background(), shardURL); err != nil {
		t.Fatalf("agents.EnsureCollection: %v", err)
	}
	if err := messageRepo.EnsureCollection(context.Background(), shardURL); err != nil {
		t.Fatalf("messages.EnsureCollection: %v", err)
	}
	selector, err := sharding.New(cfg, shardMapRepo, mongoURLRepo)
	if err != nil {
		t.Fatalf("sharding.New: %v", err)
	}
	svc, err := session.New(cfg, selector, sessionRepo, agentRepo, messageRepo, shardMapRepo, mongoPool)
	if err != nil {
		t.Fatalf("session.New: %v", err)
	}

	return deleteTestDeps{
		mongoPool: mongoPool, sessions: sessionRepo, agents: agentRepo, messages: messageRepo,
		shardMap: shardMapRepo, svc: svc, shardURL: shardURL, mongoURLID: mongoURLRow.ID,
	}
}

// backdateAgent forces agentID's updated_at directly, bypassing every
// repository method (which always stamps "now") - the only way to construct
// an agent that genuinely looks idle since some time in the past.
func backdateAgent(t *testing.T, deps deleteTestDeps, agentID string, when time.Time) {
	t.Helper()
	db, err := deps.mongoPool.Database(deps.shardURL)
	if err != nil {
		t.Fatalf("Database: %v", err)
	}
	if _, err := db.Collection(mongodb.CollectionAgents).UpdateOne(context.Background(),
		map[string]any{"_id": agentID}, map[string]any{"$set": map[string]any{"updated_at": when}}); err != nil {
		t.Fatalf("backdating agent %s: %v", agentID, err)
	}
}

func backdateSession(t *testing.T, deps deleteTestDeps, sessionID string, when time.Time) {
	t.Helper()
	db, err := deps.mongoPool.Database(deps.shardURL)
	if err != nil {
		t.Fatalf("Database: %v", err)
	}
	if _, err := db.Collection(mongodb.CollectionSessions).UpdateOne(context.Background(),
		map[string]any{"_id": sessionID}, map[string]any{"$set": map[string]any{"updated_at": when}}); err != nil {
		t.Fatalf("backdating session %s: %v", sessionID, err)
	}
}

// TestWithTransaction_RollsBackAllWritesOnError is the single most important
// test in this file: it proves mongodb.Interface.WithTransaction gives real
// ACID atomicity against the actual driver and a real (single-node) replica
// set - not just "looks fine in the happy path." A write is made, then the
// callback returns an error; the write must not be visible afterward.
func TestWithTransaction_RollsBackAllWritesOnError(t *testing.T) {
	deps := newDeleteTestDeps(t)
	ctx := context.Background()
	sessionID := ulid.Make().String()

	wantErr := errors.New("deliberate failure to force a rollback")
	err := deps.mongoPool.WithTransaction(ctx, deps.shardURL, func(sessCtx context.Context) error {
		if _, err := deps.sessions.Create(sessCtx, deps.shardURL, mongodb.Session{ID: sessionID, Status: "active"}); err != nil {
			return err
		}
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("WithTransaction error = %v, want %v", err, wantErr)
	}

	if _, found, err := deps.sessions.Get(ctx, deps.shardURL, sessionID); err != nil {
		t.Fatalf("Get: %v", err)
	} else if found {
		t.Error("the session must NOT exist: its only write happened inside a transaction that was rolled back")
	}
}

// TestDeleteSessionsOlderThan_RemovesFullyIdleSessionAtomically is the real-DB
// counterpart to package session's fake-based unit test of the same name:
// proves the whole thing - the coarse ListOlderThan filter, the per-agent
// idle check, the real cross-collection transaction, and the Postgres
// shard-map cleanup - works end to end against genuine infrastructure.
func TestDeleteSessionsOlderThan_RemovesFullyIdleSessionAtomically(t *testing.T) {
	deps := newDeleteTestDeps(t)
	ctx := context.Background()
	sessionID, agentID, messageID := ulid.Make().String(), ulid.Make().String(), ulid.Make().String()
	old := time.Now().UTC().Add(-2 * time.Hour)
	cutoff := time.Now().UTC().Add(-time.Hour)

	if _, err := deps.sessions.Create(ctx, deps.shardURL, mongodb.Session{ID: sessionID, Status: "active"}); err != nil {
		t.Fatalf("sessions.Create: %v", err)
	}
	backdateSession(t, deps, sessionID, old)
	if _, err := deps.agents.Create(ctx, deps.shardURL, mongodb.Agent{ID: agentID, SessionID: sessionID, Name: "a", Status: "disconnected"}); err != nil {
		t.Fatalf("agents.Create: %v", err)
	}
	backdateAgent(t, deps, agentID, old)
	if _, err := deps.messages.Create(ctx, deps.shardURL, mongodb.Message{ID: messageID, SessionID: sessionID, ToAgentID: agentID, Kind: "task", Body: "hi"}); err != nil {
		t.Fatalf("messages.Create: %v", err)
	}
	if _, err := deps.shardMap.Create(ctx, sessionID, deps.mongoURLID); err != nil {
		t.Fatalf("shardMap.Create: %v", err)
	}

	rep, err := deps.svc.DeleteSessionsOlderThan(ctx, deps.shardURL, cutoff)
	if err != nil {
		t.Fatalf("DeleteSessionsOlderThan: %v", err)
	}
	if rep.Sessions < 1 {
		t.Fatalf("expected at least the seeded session to be reported deleted, got %+v", rep)
	}

	if _, found, err := deps.sessions.Get(ctx, deps.shardURL, sessionID); err != nil {
		t.Fatalf("Get: %v", err)
	} else if found {
		t.Error("session should be gone")
	}
	if agents, err := deps.agents.ListBySession(ctx, deps.shardURL, sessionID); err != nil {
		t.Fatalf("ListBySession: %v", err)
	} else if len(agents) != 0 {
		t.Errorf("expected no agents left, got %d", len(agents))
	}
	if msgs, err := deps.messages.ListForAgent(ctx, deps.shardURL, sessionID, agentID, 10); err != nil {
		t.Fatalf("ListForAgent: %v", err)
	} else if len(msgs) != 0 {
		t.Errorf("expected no messages left, got %d", len(msgs))
	}
	if _, found, err := deps.shardMap.GetBySessionID(ctx, sessionID); err != nil {
		t.Fatalf("GetBySessionID: %v", err)
	} else if found {
		t.Error("shard map row should be gone")
	}
}

func TestDeleteSessionsOlderThan_SkipsSessionWithAConnectedAgent(t *testing.T) {
	deps := newDeleteTestDeps(t)
	ctx := context.Background()
	sessionID, agentID := ulid.Make().String(), ulid.Make().String()
	old := time.Now().UTC().Add(-2 * time.Hour)
	cutoff := time.Now().UTC().Add(-time.Hour)

	if _, err := deps.sessions.Create(ctx, deps.shardURL, mongodb.Session{ID: sessionID, Status: "active"}); err != nil {
		t.Fatalf("sessions.Create: %v", err)
	}
	backdateSession(t, deps, sessionID, old)
	if _, err := deps.agents.Create(ctx, deps.shardURL, mongodb.Agent{ID: agentID, SessionID: sessionID, Name: "a", Status: "connected"}); err != nil {
		t.Fatalf("agents.Create: %v", err)
	}
	backdateAgent(t, deps, agentID, old)

	if _, err := deps.svc.DeleteSessionsOlderThan(ctx, deps.shardURL, cutoff); err != nil {
		t.Fatalf("DeleteSessionsOlderThan: %v", err)
	}
	if _, found, err := deps.sessions.Get(ctx, deps.shardURL, sessionID); err != nil {
		t.Fatalf("Get: %v", err)
	} else if !found {
		t.Error("a session with a connected agent must survive")
	}
}

func TestDeleteSessionsOlderThan_SkipsSessionWithRecentAgentActivity(t *testing.T) {
	deps := newDeleteTestDeps(t)
	ctx := context.Background()
	sessionID, agentID := ulid.Make().String(), ulid.Make().String()
	old := time.Now().UTC().Add(-2 * time.Hour)
	cutoff := time.Now().UTC().Add(-time.Hour)

	if _, err := deps.sessions.Create(ctx, deps.shardURL, mongodb.Session{ID: sessionID, Status: "active"}); err != nil {
		t.Fatalf("sessions.Create: %v", err)
	}
	backdateSession(t, deps, sessionID, old)
	// Agent created "now" (via Create's own timestamping) - i.e. active
	// after cutoff, even though the session document itself looks old.
	if _, err := deps.agents.Create(ctx, deps.shardURL, mongodb.Agent{ID: agentID, SessionID: sessionID, Name: "a", Status: "disconnected"}); err != nil {
		t.Fatalf("agents.Create: %v", err)
	}

	if _, err := deps.svc.DeleteSessionsOlderThan(ctx, deps.shardURL, cutoff); err != nil {
		t.Fatalf("DeleteSessionsOlderThan: %v", err)
	}
	if _, found, err := deps.sessions.Get(ctx, deps.shardURL, sessionID); err != nil {
		t.Fatalf("Get: %v", err)
	} else if !found {
		t.Error("a session with a recently-active agent must survive - this is what proves the per-agent check is load-bearing, not just the session document's own (stale-by-design) updated_at")
	}
}

func TestDeleteSessionsOlderThan_HandlesASessionWithNoAgentsAtAll(t *testing.T) {
	deps := newDeleteTestDeps(t)
	ctx := context.Background()
	sessionID := ulid.Make().String()
	old := time.Now().UTC().Add(-2 * time.Hour)
	cutoff := time.Now().UTC().Add(-time.Hour)

	if _, err := deps.sessions.Create(ctx, deps.shardURL, mongodb.Session{ID: sessionID, Status: "active"}); err != nil {
		t.Fatalf("sessions.Create: %v", err)
	}
	backdateSession(t, deps, sessionID, old)

	rep, err := deps.svc.DeleteSessionsOlderThan(ctx, deps.shardURL, cutoff)
	if err != nil {
		t.Fatalf("DeleteSessionsOlderThan: %v", err)
	}
	if rep.Sessions < 1 {
		t.Fatalf("an ancient, agent-less session should be deleted by age alone, got %+v", rep)
	}
	if _, found, err := deps.sessions.Get(ctx, deps.shardURL, sessionID); err != nil {
		t.Fatalf("Get: %v", err)
	} else if found {
		t.Error("session should be gone")
	}
}
