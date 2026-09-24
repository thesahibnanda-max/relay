package globallink

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/thesahibnanda-max/relay/internal/proto"
)

// fakeServer is a minimal in-process stand-in for server/'s WS hub - just
// enough wire-protocol surface (hello/welcome/rpc/rpc_result/deliver/ack) to
// exercise Client end to end. This package can't import server/ (a separate
// Go module) to reuse its real handler, so this is a deliberate, small
// duplication of just the pieces under test.
type fakeServer struct {
	mu          sync.Mutex
	acksSeen    []string
	agentSeq    int
	tokenSeq    int
	pushOnHello []messageView // pushed as deliver frames right after Welcome
}

func (f *fakeServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc(wsPath, f.serve)
	return mux
}

func (f *fakeServer) serve(w http.ResponseWriter, r *http.Request) {
	c, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
	if err != nil {
		return
	}
	defer c.CloseNow()
	ctx := r.Context()

	_, data, err := c.Read(ctx)
	if err != nil {
		return
	}
	var env envelope
	if json.Unmarshal(data, &env) != nil || env.Type != typeHello {
		return
	}
	var h helloFrame
	_ = json.Unmarshal(env.Payload, &h)

	f.mu.Lock()
	f.agentSeq++
	agentID := "agent-" + strconv.Itoa(f.agentSeq)
	sessionID := "01TESTSESSIONULID0000000A"
	if h.Session != "" && h.Session != "NEW" {
		sessionID = h.Session
	}
	resumed := h.Token != ""
	token := h.Token
	if !resumed {
		f.tokenSeq++
		token = "token-" + strconv.Itoa(f.tokenSeq)
	}
	push := f.pushOnHello
	f.mu.Unlock()

	welcome := welcomeFrame{SessionID: sessionID, AgentID: agentID, Name: h.Name, Resumed: resumed}
	if !resumed {
		welcome.Token = token
	}
	b, _ := marshalEnvelope(typeWelcome, welcome)
	if c.Write(ctx, websocket.MessageText, b) != nil {
		return
	}

	for _, m := range push {
		db, _ := marshalEnvelope(typeDeliver, deliverFrame{Message: m})
		if c.Write(ctx, websocket.MessageText, db) != nil {
			return
		}
	}

	for {
		_, data, err := c.Read(ctx)
		if err != nil {
			return
		}
		var env envelope
		if json.Unmarshal(data, &env) != nil {
			continue
		}
		switch env.Type {
		case typeAck:
			var a ackFrame
			if json.Unmarshal(env.Payload, &a) == nil {
				f.mu.Lock()
				f.acksSeen = append(f.acksSeen, a.ID)
				f.mu.Unlock()
			}
		case typeRPC:
			var rpc rpcFrame
			if json.Unmarshal(env.Payload, &rpc) != nil {
				continue
			}
			result := f.handleRPC(rpc)
			b, _ := marshalEnvelope(typeRPCResult, result)
			if c.Write(ctx, websocket.MessageText, b) != nil {
				return
			}
		}
	}
}

func (f *fakeServer) handleRPC(rpc rpcFrame) rpcResultFrame {
	switch rpc.Op {
	case opSend:
		res, _ := json.Marshal(sendResult{ID: "msg-1", State: "queued"})
		return rpcResultFrame{ID: rpc.ID, OK: true, Result: res}
	case opListAgents:
		res, _ := json.Marshal(listAgentsResult{Agents: []agentInfo{
			{ID: "agent-2", Name: "bob", Tool: "claude", Role: "peer", Status: "connected"},
		}})
		return rpcResultFrame{ID: rpc.ID, OK: true, Result: res}
	case opContext:
		var a contextArgs
		_ = json.Unmarshal(rpc.Args, &a)
		res, _ := json.Marshal(getContextResult{Agent: a.Agent, Messages: []messageView{
			{ID: "msg-1", From: "alice", To: a.Agent, Kind: "task", Body: "hi", CreatedAt: time.Unix(0, 0).UTC()},
		}})
		return rpcResultFrame{ID: rpc.ID, OK: true, Result: res}
	case opWait:
		var a waitArgs
		_ = json.Unmarshal(rpc.Args, &a)
		res, _ := json.Marshal(waitResult{ID: a.ID, State: "acknowledged"})
		return rpcResultFrame{ID: rpc.ID, OK: true, Result: res}
	default:
		return rpcResultFrame{ID: rpc.ID, OK: false, Error: &wireError{Code: "bad_request", Message: "unknown op"}}
	}
}

func startFakeServer(t *testing.T, fs *fakeServer) string {
	t.Helper()
	srv := httptest.NewServer(fs.handler())
	t.Cleanup(srv.Close)
	return strings.TrimPrefix(srv.URL, "http://")
}

func connectTestClient(t *testing.T, hostPort string, opt Options) *Client {
	t.Helper()
	opt.HostPort = hostPort
	if opt.Hello.Session == "" {
		opt.Hello.Session = "NEW"
	}
	if opt.Hello.Name == "" {
		opt.Hello.Name = "alice"
	}
	if opt.Hello.Tool == "" {
		opt.Hello.Tool = "claude"
	}
	if opt.Hello.Role == "" {
		opt.Hello.Role = "peer"
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	c, err := Connect(ctx, opt)
	if err != nil {
		t.Fatalf("Connect: %v", err)
	}
	t.Cleanup(func() { c.Close(0) })
	return c
}

func TestConnect_HandshakeAndIdentity(t *testing.T) {
	hostPort := startFakeServer(t, &fakeServer{})
	c := connectTestClient(t, hostPort, Options{})

	id := c.Identity()
	if id.Agent.Name != "alice" || id.Session.Kind != "shared" || id.Token == "" || id.Resumed {
		t.Fatalf("unexpected identity: %+v", id)
	}
	wantSession := "01TESTSESSIONULID0000000A@" + hostPort
	if id.Session.ID != wantSession {
		t.Errorf("Session.ID = %q, want %q (the full shareable token, so the CLI's 'others join with' banner just works)", id.Session.ID, wantSession)
	}
}

func TestCall_RoundTripsThroughEachSupportedOp(t *testing.T) {
	hostPort := startFakeServer(t, &fakeServer{})
	c := connectTestClient(t, hostPort, Options{})
	ctx := context.Background()

	var sendResult proto.SendResult
	if err := c.Call(ctx, proto.OpSend, proto.SendArgs{To: "bob", Body: "hi"}, &sendResult); err != nil {
		t.Fatalf("Call(send): %v", err)
	}
	if sendResult.ID != "msg-1" || sendResult.State != "queued" || len(sendResult.To) != 1 || sendResult.To[0] != "bob" {
		t.Errorf("unexpected send result: %+v", sendResult)
	}

	var listResult proto.ListAgentsResult
	if err := c.Call(ctx, proto.OpListAgents, struct{}{}, &listResult); err != nil {
		t.Fatalf("Call(list_agents): %v", err)
	}
	if len(listResult.Agents) != 1 || listResult.Agents[0].Name != "bob" {
		t.Errorf("unexpected list_agents result: %+v", listResult)
	}

	var ctxResult proto.ContextResult
	if err := c.Call(ctx, proto.OpContext, proto.ContextArgs{Agent: "alice", N: 10}, &ctxResult); err != nil {
		t.Fatalf("Call(get_context): %v", err)
	}
	if len(ctxResult.Turns) != 1 || !strings.Contains(ctxResult.Turns[0].Text, "hi") {
		t.Errorf("unexpected context result: %+v", ctxResult)
	}

	var waitResult proto.WaitResult
	if err := c.Call(ctx, proto.OpWait, proto.WaitArgs{ID: "msg-1", TimeoutS: 1}, &waitResult); err != nil {
		t.Fatalf("Call(wait): %v", err)
	}
	if waitResult.State != "acknowledged" || waitResult.TimedOut {
		t.Errorf("unexpected wait result: %+v", waitResult)
	}
}

// TestCall_UnsupportedOpReturnsBadRequest covers approve/reject/agent_state/
// msg_state - not supported by a Phase 1 server, deliberately, per the
// project plan's Phase 2 table.
func TestCall_UnsupportedOpReturnsBadRequest(t *testing.T) {
	hostPort := startFakeServer(t, &fakeServer{})
	c := connectTestClient(t, hostPort, Options{})

	err := c.Call(context.Background(), proto.OpApprove, proto.ApproveArgs{}, nil)
	var pe *proto.Error
	if !errors.As(err, &pe) || pe.Code != proto.CodeBadRequest {
		t.Fatalf("expected a CodeBadRequest *proto.Error, got %v", err)
	}
}

// TestDeliver_CallsOnDeliverAndAcksEveryTime proves the transport layer
// never assumes at-most-once delivery: the same message id can legitimately
// arrive twice (a real reconnect-triggered replay), OnDeliver fires both
// times (dedup is internal/bus's job, not this package's), and an ack is
// sent back both times too - idempotent by design.
func TestDeliver_CallsOnDeliverAndAcksEveryTime(t *testing.T) {
	fs := &fakeServer{pushOnHello: []messageView{
		{ID: "dup-1", From: "bob", To: "alice", Kind: "task", Body: "hello"},
		{ID: "dup-1", From: "bob", To: "alice", Kind: "task", Body: "hello"},
	}}
	hostPort := startFakeServer(t, fs)

	var mu sync.Mutex
	var delivered []string
	done := make(chan struct{})
	connectTestClient(t, hostPort, Options{
		OnDeliver: func(m proto.MessageView) {
			mu.Lock()
			delivered = append(delivered, m.ID)
			n := len(delivered)
			mu.Unlock()
			if n == 2 {
				close(done)
			}
		},
	})

	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("did not receive both deliver frames")
	}

	deadline := time.Now().Add(2 * time.Second)
	for {
		fs.mu.Lock()
		n := len(fs.acksSeen)
		fs.mu.Unlock()
		if n >= 2 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("expected 2 acks, got %d", n)
		}
		time.Sleep(20 * time.Millisecond)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(delivered) != 2 || delivered[0] != "dup-1" || delivered[1] != "dup-1" {
		t.Fatalf("unexpected deliveries: %v", delivered)
	}
}
