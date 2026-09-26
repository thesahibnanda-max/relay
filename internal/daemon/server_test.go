package daemon

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"

	"github.com/thesahibnanda-max/relay/internal/ids"
	"github.com/thesahibnanda-max/relay/internal/proto"
	"github.com/thesahibnanda-max/relay/internal/relayhome"
)

var bg = context.Background()

type env struct {
	t      *testing.T
	srv    *Server
	paths  relayhome.Paths
	socket string
	http   *http.Client
}

// startServer runs a daemon in-process on a short-path unix socket.
func startServer(t *testing.T, mutate ...func(*Options)) *env {
	t.Helper()
	root, err := os.MkdirTemp("", "rl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	paths := relayhome.Paths{Root: root}
	var logw io.Writer = io.Discard
	if os.Getenv("RELAY_TEST_LOG") != "" {
		logw = os.Stderr
	}
	opt := Options{Paths: paths, Version: "test", Log: slog.New(slog.NewTextHandler(logw, nil))}
	for _, m := range mutate {
		m(&opt)
	}
	srv, err := New(opt)
	if err != nil {
		t.Fatal(err)
	}
	ln, err := net.Listen("unix", paths.SocketPath())
	if err != nil {
		t.Fatal(err)
	}
	go srv.Serve(ln)
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(bg, 3*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	})
	return &env{t: t, srv: srv, paths: paths, socket: paths.SocketPath(), http: proto.HTTPClient(paths.SocketPath())}
}

// client is a bare-bones agent used to exercise the server directly.
type client struct {
	t  *testing.T
	ws *websocket.Conn
}

func (e *env) dial() *client {
	e.t.Helper()
	ws, err := proto.DialAgent(bg, e.socket)
	if err != nil {
		e.t.Fatal(err)
	}
	c := &client{t: e.t, ws: ws}
	e.t.Cleanup(func() { ws.CloseNow() })
	return c
}

func (c *client) send(typ string, payload any) {
	c.t.Helper()
	b, _ := proto.Marshal(typ, payload)
	ctx, cancel := context.WithTimeout(bg, 3*time.Second)
	defer cancel()
	if err := c.ws.Write(ctx, websocket.MessageText, b); err != nil {
		c.t.Fatalf("write %s: %v", typ, err)
	}
}

func (c *client) recv() (proto.Envelope, error) {
	ctx, cancel := context.WithTimeout(bg, 3*time.Second)
	defer cancel()
	_, data, err := c.ws.Read(ctx)
	if err != nil {
		return proto.Envelope{}, err
	}
	return proto.Unmarshal(data)
}

// join sends hello and returns the welcome, or the server's error.
func (c *client) join(h proto.Hello) (*proto.Welcome, *proto.Error) {
	c.t.Helper()
	if h.Proto == 0 {
		h.Proto = proto.Version
	}
	if h.Tool == "" {
		h.Tool = "claude"
	}
	if h.Role == "" {
		h.Role = "developer"
	}
	c.send(proto.TypeHello, h)
	env, err := c.recv()
	if err != nil {
		c.t.Fatalf("no reply to hello: %v", err)
	}
	switch env.Type {
	case proto.TypeWelcome:
		var w proto.Welcome
		json.Unmarshal(env.Payload, &w)
		return &w, nil
	case proto.TypeError:
		var e proto.Error
		json.Unmarshal(env.Payload, &e)
		return nil, &e
	}
	c.t.Fatalf("unexpected reply %q", env.Type)
	return nil, nil
}

func (c *client) sendEvents(evs ...proto.Event) uint64 {
	c.t.Helper()
	c.send(proto.TypeEvents, proto.Events{Events: evs})
	env, err := c.recv()
	if err != nil || env.Type != proto.TypeAck {
		c.t.Fatalf("expected ack, got %v %v", env.Type, err)
	}
	var a proto.Ack
	json.Unmarshal(env.Payload, &a)
	return a.Seq
}

func (e *env) getJSON(path string, v any) int {
	e.t.Helper()
	resp, err := e.http.Get("http://relay" + path)
	if err != nil {
		e.t.Fatal(err)
	}
	defer resp.Body.Close()
	if v != nil {
		json.NewDecoder(resp.Body).Decode(v)
	}
	return resp.StatusCode
}

func (e *env) session(id string) proto.SessionInfo {
	var s proto.SessionInfo
	if code := e.getJSON("/v1/admin/sessions/"+id+"?all=1", &s); code != 200 {
		e.t.Fatalf("GET session: %d", code)
	}
	return s
}

func (e *env) waitAgent(sessionID, name string, pred func(proto.AgentInfo) bool, what string) proto.AgentInfo {
	e.t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		for _, a := range e.session(sessionID).Agents {
			if a.Name == name && pred(a) {
				return a
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	e.t.Fatalf("timed out waiting for agent %s: %s; have %+v", name, what, e.session(sessionID).Agents)
	return proto.AgentInfo{}
}

func TestNewSessionAndJoin(t *testing.T) {
	e := startServer(t)
	a := e.dial()
	w, perr := a.join(proto.Hello{Session: proto.SessionNew, Tool: "claude", Role: "orchestrator", PID: 42, Cwd: "/work"})
	if perr != nil {
		t.Fatal(perr)
	}
	if !ids.Valid(w.Session.ID) || w.Session.Kind != "shared" || !ids.Valid(w.Agent.ID) || w.Token == "" || w.Agent.Name == "" || w.Resumed {
		t.Fatalf("bad welcome: %+v", w)
	}

	b := e.dial()
	w2, perr := b.join(proto.Hello{Session: strings.ToLower(w.Session.ID), Tool: "codex", Role: "qa", Name: "checker"}) // ULID lookup is case-insensitive
	if perr != nil {
		t.Fatal(perr)
	}
	if w2.Session.ID != w.Session.ID || w2.Agent.Name != "checker" {
		t.Fatalf("second agent: %+v", w2)
	}

	sess := e.session(w.Session.ID)
	if len(sess.Agents) != 2 || sess.Kind != "shared" {
		t.Fatalf("session: %+v", sess)
	}
	for _, ag := range sess.Agents {
		if !ag.Connected || ag.Status != "connected" {
			t.Errorf("agent should be connected: %+v", ag)
		}
	}
	if sess.Agents[0].Role != "orchestrator" || sess.Agents[0].PID != 42 || sess.Agents[0].Cwd != "/work" {
		t.Errorf("metadata lost: %+v", sess.Agents[0])
	}
}

func TestJoinErrors(t *testing.T) {
	e := startServer(t)
	first := e.dial()
	w, _ := first.join(proto.Hello{Session: proto.SessionNew, Name: "taken"})

	cases := []struct {
		name string
		h    proto.Hello
		code string
	}{
		{"duplicate name", proto.Hello{Session: w.Session.ID, Name: "taken"}, proto.CodeNameTaken},
		{"bad name", proto.Hello{Session: w.Session.ID, Name: "Not Valid"}, proto.CodeBadName},
		{"reserved name", proto.Hello{Session: w.Session.ID, Name: "user"}, proto.CodeBadName},
		{"unknown session", proto.Hello{Session: "01ARZ3NDEKTSV4RRFFQ69G5FAV"}, proto.CodeSessionNotFound},
		{"garbage session", proto.Hello{Session: "nonsense"}, proto.CodeSessionNotFound},
		{"protocol mismatch", proto.Hello{Proto: 999, Session: w.Session.ID}, proto.CodeProtoMismatch},
		{"missing tool", proto.Hello{Session: w.Session.ID, Tool: "x", Role: "x"}, ""}, // control: valid
	}
	for _, c := range cases {
		cl := e.dial()
		_, perr := cl.join(c.h)
		switch {
		case c.code == "" && perr != nil:
			t.Errorf("%s: unexpected error %+v", c.name, perr)
		case c.code != "" && (perr == nil || perr.Code != c.code):
			t.Errorf("%s: got %+v, want code %s", c.name, perr, c.code)
		}
	}

	// ended session refuses new members
	resp, _ := e.http.Post("http://relay/v1/admin/sessions/"+w.Session.ID+"/end", "application/json", nil)
	resp.Body.Close()
	if _, perr := e.dial().join(proto.Hello{Session: w.Session.ID}); perr == nil || perr.Code != proto.CodeSessionEnded {
		t.Errorf("ended session: %+v", perr)
	}

	// hello with empty tool is refused
	bad := e.dial()
	bad.send(proto.TypeHello, proto.Hello{Proto: proto.Version, Session: proto.SessionNew})
	if env, _ := bad.recv(); env.Type != proto.TypeError {
		t.Errorf("hello without tool: %q", env.Type)
	}
}

func TestFailedJoinLeavesNoEmptySession(t *testing.T) {
	e := startServer(t)
	c := e.dial()
	if _, perr := c.join(proto.Hello{Session: proto.SessionNew, Name: "Bad Name"}); perr == nil {
		t.Fatal("expected error")
	}
	var list []proto.SessionInfo
	e.getJSON("/v1/admin/sessions", &list)
	if len(list) != 0 {
		t.Errorf("empty session left behind: %+v", list)
	}
}

func TestConcurrentJoinsGetUniqueNames(t *testing.T) {
	e := startServer(t)
	first := e.dial()
	w, _ := first.join(proto.Hello{Session: proto.SessionNew})
	const n = 25
	names := make(chan string, n)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ws, err := proto.DialAgent(bg, e.socket)
			if err != nil {
				t.Error(err)
				return
			}
			defer ws.CloseNow()
			c := &client{t: t, ws: ws}
			c.send(proto.TypeHello, proto.Hello{Proto: proto.Version, Session: w.Session.ID, Tool: "codex", Role: "developer"})
			env, err := c.recv()
			if err != nil || env.Type != proto.TypeWelcome {
				t.Errorf("join failed: %v %v", env.Type, err)
				return
			}
			var wel proto.Welcome
			json.Unmarshal(env.Payload, &wel)
			names <- wel.Agent.Name
			time.Sleep(200 * time.Millisecond) // stay connected while others join
		}()
	}
	wg.Wait()
	close(names)
	seen := map[string]bool{w.Agent.Name: true}
	for nm := range names {
		if seen[nm] {
			t.Fatalf("duplicate agent name %q", nm)
		}
		seen[nm] = true
	}
	if len(seen) != n+1 {
		t.Fatalf("%d distinct names, want %d", len(seen), n+1)
	}
}

func TestEventsStoredAckedAndDeduplicated(t *testing.T) {
	e := startServer(t)
	c := e.dial()
	w, _ := c.join(proto.Hello{Session: proto.SessionNew, Name: "rec"})
	now := time.Now().UTC()

	mixed := []proto.Event{
		{Seq: 1, T: now, Type: "start", Meta: json.RawMessage(`{"tool":"claude"}`)},
		{Seq: 2, T: now, Type: "in", B: []byte("hello\x1b[Z")},
		{Seq: 3, T: now, Type: "out", B: []byte("world\r\n")},
		{Seq: 4, T: now, Type: "resize", Meta: json.RawMessage(`{"rows":30,"cols":100}`)},
	}
	if got := c.sendEvents(mixed...); got != 4 {
		t.Fatalf("ack = %d, want 4", got)
	}
	// A trailing raw-only batch must still advance the ack.
	if got := c.sendEvents(proto.Event{Seq: 5, T: now, Type: "inject", B: []byte("injected")}); got != 5 {
		t.Fatalf("raw-only ack = %d, want 5", got)
	}
	// Resending old events is harmless and the ack never goes backwards.
	if got := c.sendEvents(mixed[:2]...); got != 5 {
		t.Fatalf("resend ack = %d, want 5", got)
	}

	// structured events are in SQLite
	structured, err := e.srv.st.Events(bg, w.Agent.ID)
	if err != nil || len(structured) != 2 || structured[0].Type != "start" || structured[1].Type != "resize" {
		t.Fatalf("structured events: %+v %v", structured, err)
	}
	if !strings.Contains(structured[1].Payload, `"rows":30`) {
		t.Errorf("payload lost: %s", structured[1].Payload)
	}

	// raw bytes are in segment files, byte-exact, and the files are private
	dir := filepath.Join(e.paths.RawDir(), w.Session.ID, w.Agent.ID)
	files, _ := filepath.Glob(filepath.Join(dir, "raw-*.jsonl"))
	if len(files) != 1 {
		t.Fatalf("segments: %v", files)
	}
	// os.Chmod cannot express owner-only on Windows (confirmed elsewhere: it
	// only toggles the read-only attribute, always reporting back 0666) - the
	// real protection there is openSegment's ACL, not a POSIX mode bit.
	if runtime.GOOS != "windows" {
		if st, _ := os.Stat(files[0]); st.Mode().Perm()&0o077 != 0 {
			t.Errorf("segment mode %v", st.Mode().Perm())
		}
	}
	data, _ := os.ReadFile(files[0])
	var got []rawLine
	dec := json.NewDecoder(bytes.NewReader(data))
	for dec.More() {
		var l rawLine
		if err := dec.Decode(&l); err != nil {
			t.Fatal(err)
		}
		got = append(got, l)
	}
	// seq 1,2 were resent after being acked and must NOT be appended again.
	if len(got) != 3 || string(got[0].B) != "hello\x1b[Z" || got[0].Type != "in" || string(got[1].B) != "world\r\n" || string(got[2].B) != "injected" {
		t.Errorf("raw lines: %+v", got)
	}
}

func TestResumeContinuesFromAckedSeq(t *testing.T) {
	e := startServer(t)
	c := e.dial()
	w, _ := c.join(proto.Hello{Session: proto.SessionNew, Name: "phoenix"})
	c.sendEvents(proto.Event{Seq: 1, T: time.Now(), Type: "start"}, proto.Event{Seq: 2, T: time.Now(), Type: "out", B: []byte("x")})

	// while still connected, nobody (even with the token) may take over
	rival := e.dial()
	if _, perr := rival.join(proto.Hello{Session: w.Session.ID, Name: "phoenix", Token: w.Token}); perr == nil || perr.Code != proto.CodeAgentLive {
		t.Errorf("takeover of live agent: %+v", perr)
	}

	c.ws.CloseNow() // crash
	e.waitAgent(w.Session.ID, "phoenix", func(a proto.AgentInfo) bool { return a.Status == "disconnected" && !a.Connected }, "disconnected")

	if _, perr := e.dial().join(proto.Hello{Session: w.Session.ID, Name: "phoenix", Token: "wrong"}); perr == nil || perr.Code != proto.CodeBadToken {
		t.Errorf("wrong token: %+v", perr)
	}
	back := e.dial()
	w2, perr := back.join(proto.Hello{Session: w.Session.ID, Name: "phoenix", Token: w.Token})
	if perr != nil {
		t.Fatal(perr)
	}
	if !w2.Resumed || w2.Agent.ID != w.Agent.ID || w2.AckedSeq != 2 || w2.Token != "" {
		t.Fatalf("resume welcome: %+v", w2)
	}
	if got := back.sendEvents(proto.Event{Seq: 3, T: time.Now(), Type: "out", B: []byte("y")}); got != 3 {
		t.Errorf("ack after resume = %d", got)
	}
	// resumed appends to the same raw segment rather than starting over
	files, _ := filepath.Glob(filepath.Join(e.paths.RawDir(), w.Session.ID, w.Agent.ID, "raw-*.jsonl"))
	if len(files) != 1 {
		t.Errorf("segments after resume: %v", files)
	}
}

func TestByeMarksExitedAndEndsSoloSession(t *testing.T) {
	e := startServer(t)
	c := e.dial()
	w, _ := c.join(proto.Hello{Session: proto.SessionNew, Name: "quitter"})
	c.send(proto.TypeBye, proto.Bye{ExitCode: 130})
	a := e.waitAgent(w.Session.ID, "quitter", func(a proto.AgentInfo) bool { return a.Status == "exited" }, "exited")
	if a.ExitCode == nil || *a.ExitCode != 130 || a.Connected {
		t.Errorf("%+v", a)
	}
	if e.session(w.Session.ID).Status != "active" {
		t.Error("shared sessions stay active when an agent exits")
	}

	solo := e.dial()
	sw, _ := solo.join(proto.Hello{Session: ""})
	if sw.Session.Kind != "solo" {
		t.Fatalf("kind = %s", sw.Session.Kind)
	}
	solo.send(proto.TypeBye, proto.Bye{ExitCode: 0})
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && e.session(sw.Session.ID).Status != "ended" {
		time.Sleep(10 * time.Millisecond)
	}
	if e.session(sw.Session.ID).Status != "ended" {
		t.Error("solo session should end when its agent exits")
	}
	var visible []proto.SessionInfo
	e.getJSON("/v1/admin/sessions", &visible)
	for _, s := range visible {
		if s.ID == sw.Session.ID {
			t.Error("solo sessions must be hidden from the default listing")
		}
	}
}

func TestDroppedConnectionMarksDisconnected(t *testing.T) {
	e := startServer(t)
	c := e.dial()
	w, _ := c.join(proto.Hello{Session: proto.SessionNew, Name: "flaky"})
	c.ws.CloseNow()
	e.waitAgent(w.Session.ID, "flaky", func(a proto.AgentInfo) bool { return a.Status == "disconnected" }, "disconnected")
}

func TestDeadPeerIsDetectedByPing(t *testing.T) {
	e := startServer(t, func(o *Options) { o.PingEvery = 40 * time.Millisecond; o.PongTimeout = 80 * time.Millisecond })
	c := e.dial()
	w, _ := c.join(proto.Hello{Session: proto.SessionNew, Name: "frozen"})
	// The client never reads again, so it can't answer pings.
	e.waitAgent(w.Session.ID, "frozen", func(a proto.AgentInfo) bool { return a.Status == "disconnected" }, "ping timeout")
}

func TestHelloTimeout(t *testing.T) {
	e := startServer(t, func(o *Options) { o.HelloTimeout = 100 * time.Millisecond })
	c := e.dial()
	ctx, cancel := context.WithTimeout(bg, 2*time.Second)
	defer cancel()
	if _, _, err := c.ws.Read(ctx); err == nil {
		t.Error("server should close a connection that never says hello")
	}
}

func TestBrowserOriginIsRefused(t *testing.T) {
	e := startServer(t)
	req, _ := http.NewRequest("GET", "http://relay/v1/agent", nil)
	req.Header.Set("Origin", "https://evil.example")
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	resp, err := e.http.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %d, want 403", resp.StatusCode)
	}
}

func TestMalformedMessagesAreRejected(t *testing.T) {
	e := startServer(t)
	for name, payload := range map[string][]byte{"not json": []byte("{{{"), "no type": []byte(`{"v":1}`), "wrong first message": []byte(`{"v":1,"type":"events","payload":{}}`)} {
		c := e.dial()
		ctx, cancel := context.WithTimeout(bg, 2*time.Second)
		c.ws.Write(ctx, websocket.MessageText, payload)
		cancel()
		if env, err := c.recv(); err == nil && env.Type != proto.TypeError {
			t.Errorf("%s: got %q", name, env.Type)
		}
	}
	// oversized batch and seq 0 after a valid hello
	c := e.dial()
	c.join(proto.Hello{Session: proto.SessionNew})
	c.send(proto.TypeEvents, proto.Events{Events: []proto.Event{{Seq: 0, Type: "out"}}})
	if env, _ := c.recv(); env.Type != proto.TypeError {
		t.Errorf("seq 0 accepted: %q", env.Type)
	}
}

func TestAdminSessionCRUD(t *testing.T) {
	e := startServer(t)
	resp, err := e.http.Post("http://relay/v1/admin/sessions", "application/json", strings.NewReader(`{"name":"demo"}`))
	if err != nil {
		t.Fatal(err)
	}
	var created proto.SessionInfo
	json.NewDecoder(resp.Body).Decode(&created)
	resp.Body.Close()
	if resp.StatusCode != 201 || !ids.Valid(created.ID) || created.Name != "demo" || created.Agents == nil {
		t.Fatalf("create: %d %+v", resp.StatusCode, created)
	}
	// an agent can join a pre-created session
	if _, perr := e.dial().join(proto.Hello{Session: created.ID, Name: "first"}); perr != nil {
		t.Fatal(perr)
	}
	var st proto.Status
	if e.getJSON("/v1/admin/status", &st) != 200 || st.Proto != proto.Version || st.Version != "test" || st.Agents != 1 || st.PID != os.Getpid() {
		t.Errorf("status: %+v", st)
	}
	if e.getJSON("/v1/admin/sessions/01ARZ3NDEKTSV4RRFFQ69G5FAV", nil) != 404 {
		t.Error("unknown session should be 404")
	}
	r2, _ := e.http.Post("http://relay/v1/admin/sessions/01ARZ3NDEKTSV4RRFFQ69G5FAV/end", "application/json", nil)
	r2.Body.Close()
	if r2.StatusCode != 404 {
		t.Errorf("end unknown: %d", r2.StatusCode)
	}
	long := strings.Repeat("x", 100)
	r3, _ := e.http.Post("http://relay/v1/admin/sessions", "application/json", strings.NewReader(`{"name":"`+long+`"}`))
	r3.Body.Close()
	if r3.StatusCode != 400 {
		t.Errorf("long name: %d", r3.StatusCode)
	}
}

func TestRestartMarksAgentsDisconnectedAndKeepsData(t *testing.T) {
	e := startServer(t)
	c := e.dial()
	w, _ := c.join(proto.Hello{Session: proto.SessionNew, Name: "survivor"})
	c.sendEvents(proto.Event{Seq: 1, T: time.Now(), Type: "start"})

	// hard-stop the server (no bye from the agent), then start a new one on the same home
	ctx, cancel := context.WithTimeout(bg, 3*time.Second)
	e.srv.Shutdown(ctx)
	cancel()
	os.Remove(e.socket)

	srv2, err := New(Options{Paths: e.paths, Version: "test2", Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	ln, _ := net.Listen("unix", e.socket)
	go srv2.Serve(ln)
	t.Cleanup(func() { c2, cc := context.WithTimeout(bg, 3*time.Second); defer cc(); srv2.Shutdown(c2) })

	e.http = proto.HTTPClient(e.socket)
	s := e.session(w.Session.ID)
	if len(s.Agents) != 1 || s.Agents[0].Status != "disconnected" {
		t.Fatalf("after restart: %+v", s)
	}
	back := e.dial()
	w2, perr := back.join(proto.Hello{Session: w.Session.ID, Name: "survivor", Token: w.Token})
	if perr != nil || w2.AckedSeq != 1 {
		t.Fatalf("resume after daemon restart: %+v %v", w2, perr)
	}
}

func TestShutdownDisconnectsAgents(t *testing.T) {
	e := startServer(t)
	c := e.dial()
	w, _ := c.join(proto.Hello{Session: proto.SessionNew, Name: "victim"})
	ctx, cancel := context.WithTimeout(bg, 3*time.Second)
	defer cancel()
	go e.srv.Shutdown(ctx)
	rctx, rcancel := context.WithTimeout(bg, 3*time.Second)
	defer rcancel()
	if _, _, err := c.ws.Read(rctx); err == nil {
		t.Error("expected the connection to be closed on shutdown")
	}
	_ = w
}

// TestResumeAnExitedAgentIsAllowed settles the hardening item M-mesh-6's
// plan flagged explicitly: an agent that said Bye (a clean exit, not just a
// dropped connection) can still resume with its original token, matching
// the whole point of resume - relaunching the exact same `relay <tool>
// --session=X --name=Y` after a crash or a deliberate quit should pick the
// same identity back up rather than forcing a fresh name.
func TestResumeAnExitedAgentIsAllowed(t *testing.T) {
	e := startServer(t)
	c := e.dial()
	w, _ := c.join(proto.Hello{Session: proto.SessionNew, Name: "phoenix"})
	c.send(proto.TypeBye, proto.Bye{ExitCode: 1})
	a := e.waitAgent(w.Session.ID, "phoenix", func(a proto.AgentInfo) bool { return a.Status == "exited" }, "exited")
	if a.ExitCode == nil || *a.ExitCode != 1 {
		t.Fatalf("%+v", a)
	}

	back := e.dial()
	w2, perr := back.join(proto.Hello{Session: w.Session.ID, Name: "phoenix", Token: w.Token})
	if perr != nil {
		t.Fatalf("resuming an exited agent: %+v", perr)
	}
	if !w2.Resumed || w2.Agent.ID != w.Agent.ID {
		t.Fatalf("resume welcome: %+v", w2)
	}
	a2 := e.waitAgent(w.Session.ID, "phoenix", func(a proto.AgentInfo) bool { return a.Status == "connected" }, "connected again")
	if a2.ExitCode != nil {
		t.Errorf("exit code should be cleared on resume, got %v", *a2.ExitCode)
	}
}
