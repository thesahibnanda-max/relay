package cli

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/thesahibnanda-max/relay/internal/globalid"
	"github.com/thesahibnanda-max/relay/internal/relayhome"
)

// The wire* types below mirror server/package/ws/protocol.go's JSON shape
// just enough to drive runApproveGlobal end to end. This package can't
// import server/ (a separate Go module) or internal/globallink's unexported
// types, so this is a small, deliberate duplication - the same approach
// internal/globallink's own client_test.go takes for its fakeServer.

type wireEnvelope struct {
	V       int             `json:"v"`
	Type    string          `json:"type"`
	Payload json.RawMessage `json:"payload"`
}

type wireHello struct {
	Session string `json:"session"`
	Name    string `json:"name"`
	Token   string `json:"token,omitempty"`
	Tool    string `json:"tool"`
	Role    string `json:"role"`
}

type wireWelcome struct {
	SessionID string `json:"session_id"`
	AgentID   string `json:"agent_id"`
	Name      string `json:"name"`
	Resumed   bool   `json:"resumed"`
}

type wireRPC struct {
	ID   string          `json:"id"`
	Op   string          `json:"op"`
	Args json.RawMessage `json:"args,omitempty"`
}

type wireRPCResult struct {
	ID     string          `json:"id"`
	OK     bool            `json:"ok"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *wireErr        `json:"error,omitempty"`
}

type wireErr struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type wireMessageView struct {
	ID        string    `json:"id"`
	From      string    `json:"from"`
	To        string    `json:"to"`
	Kind      string    `json:"kind"`
	Priority  int       `json:"priority"`
	Body      string    `json:"body"`
	State     string    `json:"state,omitempty"`
	Detail    string    `json:"detail,omitempty"`
	CreatedAt time.Time `json:"created_at"`
}

type wireListHeldResult struct {
	Messages []wireMessageView `json:"messages"`
}

type wireApproveResult struct {
	ID    string `json:"id"`
	State string `json:"state"`
}

func marshalWire(typ string, payload any) []byte {
	b, _ := json.Marshal(payload)
	env, _ := json.Marshal(wireEnvelope{V: 1, Type: typ, Payload: b})
	return env
}

// fakeGlobalServer is a minimal stand-in for server/'s WS hub: enough to
// answer a hello and then list_held/approve/reject. It records every
// approve/reject it was asked to perform, in order.
type fakeGlobalServer struct {
	held []wireMessageView
	ops  []string // "approve:<id>" / "reject:<id>"
}

func (f *fakeGlobalServer) handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/agent", f.serve)
	return mux
}

func (f *fakeGlobalServer) serve(w http.ResponseWriter, r *http.Request) {
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
	var env wireEnvelope
	if json.Unmarshal(data, &env) != nil || env.Type != "hello" {
		return
	}
	var h wireHello
	_ = json.Unmarshal(env.Payload, &h)

	welcome := wireWelcome{SessionID: h.Session, AgentID: "agent-1", Name: h.Name, Resumed: h.Token != ""}
	if c.Write(ctx, websocket.MessageText, marshalWire("welcome", welcome)) != nil {
		return
	}

	for {
		_, data, err := c.Read(ctx)
		if err != nil {
			return
		}
		var env wireEnvelope
		if json.Unmarshal(data, &env) != nil || env.Type != "rpc" {
			continue
		}
		var rpc wireRPC
		if json.Unmarshal(env.Payload, &rpc) != nil {
			continue
		}
		var result wireRPCResult
		switch rpc.Op {
		case "list_held":
			res, _ := json.Marshal(wireListHeldResult{Messages: f.held})
			result = wireRPCResult{ID: rpc.ID, OK: true, Result: res}
		case "approve", "reject":
			var a struct {
				ID string `json:"id,omitempty"`
			}
			_ = json.Unmarshal(rpc.Args, &a)
			id := a.ID
			if id == "" && len(f.held) > 0 {
				id = f.held[0].ID
			}
			f.ops = append(f.ops, rpc.Op+":"+id)
			state := "queued"
			if rpc.Op == "reject" {
				state = "rejected"
			}
			for i, m := range f.held {
				if m.ID == id {
					f.held = append(f.held[:i], f.held[i+1:]...)
					break
				}
			}
			res, _ := json.Marshal(wireApproveResult{ID: id, State: state})
			result = wireRPCResult{ID: rpc.ID, OK: true, Result: res}
		default:
			result = wireRPCResult{ID: rpc.ID, OK: false, Error: &wireErr{Code: "bad_request", Message: "unknown op"}}
		}
		if c.Write(ctx, websocket.MessageText, marshalWire("rpc_result", result)) != nil {
			return
		}
	}
}

// TestRunApproveGlobal_AcceptAllDecidesEveryHeldMessage proves relay approve
// works end to end for a global session: it resumes as the named agent
// using its saved identity (never touching a local daemon's admin API),
// lists what's held, and approves it.
func TestRunApproveGlobal_AcceptAllDecidesEveryHeldMessage(t *testing.T) {
	fs := &fakeGlobalServer{held: []wireMessageView{
		{ID: "held-1", From: "alice", To: "bob", Kind: "task", Body: "please approve", State: "held", Detail: "awaiting approval", CreatedAt: time.Now()},
	}}
	srv := httptest.NewServer(fs.handler())
	defer srv.Close()
	hostPort := strings.TrimPrefix(srv.URL, "http://")

	t.Setenv("RELAY_HOME", t.TempDir())
	paths, err := relayhome.Resolve()
	if err != nil {
		t.Fatalf("relayhome.Resolve: %v", err)
	}
	if err := paths.Ensure(); err != nil {
		t.Fatalf("paths.Ensure: %v", err)
	}

	token := globalid.Token{ULID: "01TESTSESSIONULID0000000A", HostPort: hostPort}
	saveIdentityWithPolicy(paths, token.FileSafe(), "bob", "saved-token", "claude", true, false, false)

	p := Parsed{
		Kind: KindApprove, Sub: "accept", Words: []string{"all"},
		SessionKind: SessionKindGlobalJoin, GlobalToken: token, Session: token.String(), Name: "bob",
	}
	var out, errw strings.Builder
	if code := runApprove(p, nil, &out, &errw); code != 0 {
		t.Fatalf("runApprove: code=%d out=%q err=%q", code, out.String(), errw.String())
	}
	if len(fs.ops) != 1 || fs.ops[0] != "approve:held-1" {
		t.Fatalf("expected exactly one approve of held-1, got %v", fs.ops)
	}
	if !strings.Contains(out.String(), "approved held-1") {
		t.Errorf("unexpected output: %q", out.String())
	}
}

// TestRunApproveGlobal_ListsHeldMessages proves the no-subcommand path
// prints a table of what's held, without deciding anything.
func TestRunApproveGlobal_ListsHeldMessages(t *testing.T) {
	fs := &fakeGlobalServer{held: []wireMessageView{
		{ID: "held-1", From: "alice", To: "bob", Kind: "task", Body: "please approve", State: "held", Detail: "awaiting approval", CreatedAt: time.Now()},
	}}
	srv := httptest.NewServer(fs.handler())
	defer srv.Close()
	hostPort := strings.TrimPrefix(srv.URL, "http://")

	t.Setenv("RELAY_HOME", t.TempDir())
	paths, err := relayhome.Resolve()
	if err != nil {
		t.Fatalf("relayhome.Resolve: %v", err)
	}
	if err := paths.Ensure(); err != nil {
		t.Fatalf("paths.Ensure: %v", err)
	}

	token := globalid.Token{ULID: "01TESTSESSIONULID0000000A", HostPort: hostPort}
	saveIdentityWithPolicy(paths, token.FileSafe(), "bob", "saved-token", "claude", true, false, false)

	p := Parsed{Kind: KindApprove, SessionKind: SessionKindGlobalJoin, GlobalToken: token, Session: token.String(), Name: "bob"}
	var out, errw strings.Builder
	if code := runApprove(p, nil, &out, &errw); code != 0 {
		t.Fatalf("runApprove: code=%d out=%q err=%q", code, out.String(), errw.String())
	}
	if len(fs.ops) != 0 {
		t.Fatalf("ls must not decide anything, got ops=%v", fs.ops)
	}
	if !strings.Contains(out.String(), "held-1") || !strings.Contains(out.String(), "please approve") {
		t.Errorf("expected the held message in the listing, got %q", out.String())
	}
}

// TestRunApproveGlobal_NoSavedIdentityFailsClearly proves acting on an agent
// that never connected fails with a clear error rather than a confusing dial
// failure.
func TestRunApproveGlobal_NoSavedIdentityFailsClearly(t *testing.T) {
	t.Setenv("RELAY_HOME", t.TempDir())
	paths, err := relayhome.Resolve()
	if err != nil {
		t.Fatalf("relayhome.Resolve: %v", err)
	}
	if err := paths.Ensure(); err != nil {
		t.Fatalf("paths.Ensure: %v", err)
	}

	token := globalid.Token{ULID: "01TESTSESSIONULID0000000A", HostPort: "127.0.0.1:1"}
	p := Parsed{Kind: KindApprove, SessionKind: SessionKindGlobalJoin, GlobalToken: token, Session: token.String(), Name: "nobody"}
	var out, errw strings.Builder
	if code := runApprove(p, nil, &out, &errw); code != 1 {
		t.Fatalf("expected failure, got code=%d out=%q err=%q", code, out.String(), errw.String())
	}
	if !strings.Contains(errw.String(), "no saved identity") {
		t.Errorf("expected a clear no-saved-identity error, got %q", errw.String())
	}
}
