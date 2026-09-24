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
	"github.com/oklog/ulid/v2"

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

// testStack is the full wired-up server, plus every repository/service piece
// a test might want to assert against directly (DB state, not just what
// comes back over the wire).
type testStack struct {
	shards    sharding.Interface
	agentRepo repository.AgentRepository
	svc       session.Interface
	hub       ws.Interface
}

func buildStack(t *testing.T) testStack {
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
	svc, err := session.New(cfg, selector, sessionRepo, agentRepo, messageRepo)
	if err != nil {
		t.Fatalf("session.New: %v", err)
	}
	hub, err := ws.New(svc)
	if err != nil {
		t.Fatalf("ws.New: %v", err)
	}
	return testStack{shards: selector, agentRepo: agentRepo, svc: svc, hub: hub}
}

func startServer(t *testing.T, hub ws.Interface) string {
	t.Helper()
	srv := httptest.NewServer(ws.NewHandler(hub))
	t.Cleanup(srv.Close)
	return "ws" + strings.TrimPrefix(srv.URL, "http") + ws.Path
}

// TestEndToEndTwoAgentsOneSession dials the WS server twice, joining the
// same freshly-created global session, and checks that list_agents sees
// both and a send from one is delivered to the other while connected -
// the plan's stated end-to-end acceptance check.
func TestEndToEndTwoAgentsOneSession(t *testing.T) {
	stack := buildStack(t)
	wsURL := startServer(t, stack.hub)

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
	wsURL := startServer(t, stack.hub)

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
	wsURL := startServer(t, stack.hub)

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
	wsURL := startServer(t, stack.hub)

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
	wsURL := startServer(t, stack.hub)

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
	wsURL := startServer(t, stack.hub)

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
	wsURL := startServer(t, stack.hub)

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
