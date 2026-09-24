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
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	gorilla "github.com/coder/websocket"

	"github.com/thesahibnanda-max/relay/server/package/config"
	"github.com/thesahibnanda-max/relay/server/package/database/mongodb"
	"github.com/thesahibnanda-max/relay/server/package/database/postgres"
	"github.com/thesahibnanda-max/relay/server/package/database/repository"
	"github.com/thesahibnanda-max/relay/server/package/session"
	"github.com/thesahibnanda-max/relay/server/package/sharding"
	"github.com/thesahibnanda-max/relay/server/package/ws"
)

func testConfig(t *testing.T) config.Config {
	t.Helper()
	dsn := os.Getenv("TEST_SQL_DSN")
	mongoURLs := os.Getenv("TEST_MONGO_URLS")
	if dsn == "" || mongoURLs == "" {
		t.Skip("set TEST_SQL_DSN and TEST_MONGO_URLS (see server/.env.example) against a running server/docker-compose.yml to run this test")
	}
	return config.Config{PORT: 0, PostgreSQLDSN: dsn, MongoURLs: strings.Split(mongoURLs, ",")}
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

func buildHub(t *testing.T) ws.Interface {
	t.Helper()
	cfg := testConfig(t)

	pg, err := postgres.New(cfg)
	if err != nil {
		t.Fatalf("postgres.New: %v", err)
	}
	mongoPool, err := mongodb.New(cfg)
	if err != nil {
		t.Fatalf("mongodb.New: %v", err)
	}
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
	selector, err := sharding.New(shardMapRepo, mongoURLRepo)
	if err != nil {
		t.Fatalf("sharding.New: %v", err)
	}
	svc, err := session.New(selector, sessionRepo, agentRepo, messageRepo)
	if err != nil {
		t.Fatalf("session.New: %v", err)
	}
	hub, err := ws.New(svc)
	if err != nil {
		t.Fatalf("ws.New: %v", err)
	}
	return hub
}

// TestEndToEndTwoAgentsOneSession dials the WS server twice, joining the
// same freshly-created global session, and checks that list_agents sees
// both and a send from one is delivered to the other while connected -
// exactly the plan's stated end-to-end acceptance check.
func TestEndToEndTwoAgentsOneSession(t *testing.T) {
	hub := buildHub(t)
	srv := httptest.NewServer(ws.NewHandler(hub))
	defer srv.Close()
	wsURL := "ws" + strings.TrimPrefix(srv.URL, "http") + ws.Path

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

	writeTyped(t, ctx, alice, ws.TypeListAgents, struct{}{})
	list := readTyped[ws.ListAgentsResult](t, ctx, alice, ws.TypeAgentsList)
	if len(list.Agents) != 2 {
		t.Fatalf("expected 2 agents in list_agents, got %d: %+v", len(list.Agents), list.Agents)
	}

	writeTyped(t, ctx, alice, ws.TypeSend, ws.Send{To: "bob", Body: "hello bob"})
	sendResult := readTyped[ws.SendResult](t, ctx, alice, ws.TypeSendResult)
	if sendResult.ID == "" {
		t.Fatal("expected a non-empty message id in send_result")
	}

	deliver := readTyped[ws.Deliver](t, ctx, bob, ws.TypeDeliver)
	if deliver.Body != "hello bob" || deliver.From != "alice" {
		t.Fatalf("unexpected deliver frame: %+v", deliver)
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
