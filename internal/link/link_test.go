package link

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thesahibnanda-max/relay/internal/daemon"
	"github.com/thesahibnanda-max/relay/internal/eventlog"
	"github.com/thesahibnanda-max/relay/internal/proto"
	"github.com/thesahibnanda-max/relay/internal/relayhome"
)

var bg = context.Background()

// testDaemon is a restartable in-process daemon on a fixed home.
type testDaemon struct {
	t     *testing.T
	paths relayhome.Paths
	mu    sync.Mutex
	srv   *daemon.Server
	tune  func(*daemon.Options) // optional daemon settings (tests)
	logs  syncBuf               // the daemon's log, for failure reports
}

type syncBuf struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (s *syncBuf) Write(p []byte) (int, error) { s.mu.Lock(); defer s.mu.Unlock(); return s.b.Write(p) }
func (s *syncBuf) tail(n int) string {
	s.mu.Lock()
	defer s.mu.Unlock()
	lines := strings.Split(strings.TrimSpace(s.b.String()), "\n")
	if len(lines) > n {
		lines = lines[len(lines)-n:]
	}
	return strings.Join(lines, "\n")
}

func newTestDaemon(t *testing.T) *testDaemon {
	root, err := os.MkdirTemp("", "rl")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { os.RemoveAll(root) })
	d := &testDaemon{t: t, paths: relayhome.Paths{Root: root}}
	d.start()
	t.Cleanup(d.stop)
	return d
}

func (d *testDaemon) start() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.srv != nil {
		return
	}
	opt := daemon.Options{Paths: d.paths, Version: "t", Log: slog.New(slog.NewTextHandler(&d.logs, nil))}
	if d.tune != nil {
		d.tune(&opt)
	}
	srv, err := daemon.New(opt)
	if err != nil {
		d.t.Fatal(err)
	}
	os.Remove(d.paths.SocketPath())
	ln, err := net.Listen("unix", d.paths.SocketPath())
	if err != nil {
		d.t.Fatal(err)
	}
	go srv.Serve(ln)
	d.srv = srv
}

func (d *testDaemon) stop() {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.srv == nil {
		return
	}
	ctx, cancel := context.WithTimeout(bg, 3*time.Second)
	defer cancel()
	d.srv.Shutdown(ctx)
	d.srv = nil
}

func (d *testDaemon) ensure(context.Context) error { d.start(); return nil }

func (d *testDaemon) session(id string) proto.SessionInfo {
	var s proto.SessionInfo
	c := proto.HTTPClient(d.paths.SocketPath())
	resp, err := c.Get("http://relay/v1/admin/sessions/" + id + "?all=1")
	if err != nil {
		d.t.Fatal(err)
	}
	defer resp.Body.Close()
	json.NewDecoder(resp.Body).Decode(&s)
	return s
}

func (d *testDaemon) waitFor(what string, cond func() bool) {
	d.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		if cond() {
			return
		}
		time.Sleep(15 * time.Millisecond)
	}
	d.t.Fatalf("timed out waiting for %s", what)
}

func (d *testDaemon) opts(h proto.Hello) Options {
	if h.Tool == "" {
		h.Tool = "claude"
	}
	if h.Role == "" {
		h.Role = "developer"
	}
	return Options{Paths: d.paths, Hello: h, EnsureDaemon: d.ensure, BackoffMin: 10 * time.Millisecond, BackoffMax: 50 * time.Millisecond}
}

func rawLines(t *testing.T, d *testDaemon, session, agent string) []string {
	t.Helper()
	files, _ := filepath.Glob(filepath.Join(d.paths.RawDir(), session, agent, "raw-*.jsonl"))
	var out []string
	for _, f := range files {
		data, _ := os.ReadFile(f)
		dec := json.NewDecoder(bytes.NewReader(data))
		for dec.More() {
			var l struct {
				Seq uint64 `json:"seq"`
				B   []byte `json:"b"`
			}
			if err := dec.Decode(&l); err != nil {
				t.Fatal(err)
			}
			out = append(out, string(l.B))
		}
	}
	return out
}

func TestConnectSendAndCloseFlushes(t *testing.T) {
	d := newTestDaemon(t)
	c, err := Connect(bg, d.opts(proto.Hello{Session: proto.SessionNew, Name: "writer"}))
	if err != nil {
		t.Fatal(err)
	}
	id := c.Identity()
	if id.Agent.Name != "writer" || id.Session.ID == "" || id.Token == "" || !c.Online() {
		t.Fatalf("identity: %+v", id)
	}
	c.Send(proto.Event{T: time.Now(), Type: "start", Meta: json.RawMessage(`{"tool":"claude"}`)})
	for i := 0; i < 500; i++ {
		c.Send(proto.Event{T: time.Now(), Type: "out", B: []byte{byte('a' + i%26)}})
	}
	c.Close(3)

	ag := d.session(id.Session.ID).Agents[0]
	if ag.Status != "exited" || ag.ExitCode == nil || *ag.ExitCode != 3 {
		t.Fatalf("agent after close: %+v", ag)
	}
	lines := rawLines(t, d, id.Session.ID, id.Agent.ID)
	if len(lines) != 500 {
		t.Fatalf("stored %d raw events, want 500", len(lines))
	}
	for i, l := range lines {
		if l != string(rune('a'+i%26)) {
			t.Fatalf("raw event %d out of order: %q", i, l)
		}
	}
}

func TestNilClientIsSafe(t *testing.T) {
	var c *Client
	c.Send(proto.Event{Type: "out"})
	c.Close(0)
	if c.Online() || c.Err() != nil || c.Dropped() != 0 || c.Identity().Agent.ID != "" {
		t.Error("nil client must be an inert no-op")
	}
}

func TestConnectErrorsSurfaceAsProtoError(t *testing.T) {
	d := newTestDaemon(t)
	_, err := Connect(bg, d.opts(proto.Hello{Session: "01ARZ3NDEKTSV4RRFFQ69G5FAV"}))
	var pe *proto.Error
	if !errors.As(err, &pe) || pe.Code != proto.CodeSessionNotFound {
		t.Errorf("err = %v", err)
	}
	d.stop()
	if _, err := Connect(bg, d.opts(proto.Hello{Session: proto.SessionNew})); err == nil {
		t.Error("connecting to a stopped daemon must fail")
	}
}

// The core promise: events sent while the daemon is down (and across its
// restart) arrive exactly once, in order, for the same agent.
func TestSurvivesDaemonRestartWithoutLossOrDuplication(t *testing.T) {
	d := newTestDaemon(t)
	c, err := Connect(bg, d.opts(proto.Hello{Session: proto.SessionNew, Name: "steady"}))
	if err != nil {
		t.Fatal(err)
	}
	id := c.Identity()

	send := func(from, to int) {
		for i := from; i < to; i++ {
			c.Send(proto.Event{T: time.Now(), Type: "out", B: []byte{byte(i >> 8), byte(i)}})
		}
	}
	send(0, 300)
	d.waitFor("first batch stored", func() bool { return len(rawLines(t, d, id.Session.ID, id.Agent.ID)) == 300 })

	d.stop() // daemon dies
	d.waitFor("client notices", func() bool { return !c.Online() })
	send(300, 800) // sent while it is down: buffered
	d.start()      // comes back (EnsureDaemon would also do this)
	send(800, 1000)

	c.Close(0)
	if err := c.Err(); err != nil {
		t.Fatalf("client gave up: %v", err)
	}
	lines := rawLines(t, d, id.Session.ID, id.Agent.ID)
	if len(lines) != 1000 {
		t.Fatalf("stored %d events, want 1000", len(lines))
	}
	for i, l := range lines {
		if l != string([]byte{byte(i >> 8), byte(i)}) {
			t.Fatalf("event %d lost, duplicated or reordered", i)
		}
	}
	if c.Identity().Agent.ID != id.Agent.ID {
		t.Error("agent identity changed across the restart")
	}
	sessions := d.session(id.Session.ID)
	if len(sessions.Agents) != 1 || sessions.Agents[0].Status != "exited" {
		t.Errorf("after close: %+v", sessions.Agents)
	}
}

func TestReconnectRestartsDaemonViaEnsure(t *testing.T) {
	d := newTestDaemon(t)
	var ensured int
	var mu sync.Mutex
	o := d.opts(proto.Hello{Session: proto.SessionNew})
	o.EnsureDaemon = func(ctx context.Context) error { mu.Lock(); ensured++; mu.Unlock(); return d.ensure(ctx) }
	c, err := Connect(bg, o)
	if err != nil {
		t.Fatal(err)
	}
	d.stop()
	d.waitFor("offline", func() bool { return !c.Online() })
	d.waitFor("back online via ensure", func() bool { return c.Online() })
	mu.Lock()
	defer mu.Unlock()
	if ensured == 0 {
		t.Error("EnsureDaemon was never called during reconnect")
	}
	c.Close(0)
}

func TestSendNeverBlocksAndOverflowDropsRawFirst(t *testing.T) {
	d := newTestDaemon(t)
	o := d.opts(proto.Hello{Session: proto.SessionNew})
	o.MaxBuffered = 50
	o.EnsureDaemon = func(context.Context) error { return errors.New("daemon stays down") }
	c, err := Connect(bg, o)
	if err != nil {
		t.Fatal(err)
	}
	d.stop()
	d.waitFor("offline", func() bool { return !c.Online() })

	done := make(chan struct{})
	go func() {
		c.Send(proto.Event{Type: "start"})
		for i := 0; i < 5000; i++ {
			c.Send(proto.Event{Type: "out", B: []byte("x")})
		}
		c.Send(proto.Event{Type: "exit"})
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Send blocked while the daemon was down")
	}
	if c.Dropped() == 0 {
		t.Error("expected raw events to be dropped on overflow")
	}
	c.mu.Lock()
	var kinds []string
	for _, e := range c.buf {
		kinds = append(kinds, e.Type)
	}
	c.mu.Unlock()
	if len(kinds) > 50 || kinds[0] != "start" || kinds[len(kinds)-1] != "exit" {
		t.Errorf("structured events must survive overflow; buffer = %v (len %d)", kinds[:3], len(kinds))
	}
	start := time.Now()
	c.Close(1) // cannot flush: must give up quickly, not hang
	if time.Since(start) > flushOnClose+time.Second {
		t.Errorf("Close hung for %v", time.Since(start))
	}
}

func TestStopsSyncingWhenDaemonRefusesResume(t *testing.T) {
	d := newTestDaemon(t)
	c, err := Connect(bg, d.opts(proto.Hello{Session: proto.SessionNew, Name: "orphan"}))
	if err != nil {
		t.Fatal(err)
	}
	id := c.Identity()
	d.stop()
	d.waitFor("offline", func() bool { return !c.Online() })

	// While the daemon is down, end the session behind the client's back.
	d.start()
	hc := proto.HTTPClient(d.paths.SocketPath())
	resp, _ := hc.Post("http://relay/v1/admin/sessions/"+id.Session.ID+"/end", "application/json", nil)
	resp.Body.Close()

	d.waitFor("client to give up", func() bool { return c.Err() != nil })
	var pe *proto.Error
	if !errors.As(c.Err(), &pe) || pe.Code != proto.CodeSessionEnded {
		t.Errorf("Err = %v", c.Err())
	}
	done := make(chan struct{})
	go func() { c.Send(proto.Event{Type: "out"}); c.Close(0); close(done) }()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("Send/Close blocked after fatal error")
	}
}

// A new process resuming an existing agent must continue numbering after the
// daemon's acked position instead of restarting at 1 (which would be ignored
// as duplicates).
func TestResumeFromNewProcessContinuesSeq(t *testing.T) {
	d := newTestDaemon(t)
	c1, err := Connect(bg, d.opts(proto.Hello{Session: proto.SessionNew, Name: "relaunched"}))
	if err != nil {
		t.Fatal(err)
	}
	id := c1.Identity()
	for i := 0; i < 10; i++ {
		c1.Send(proto.Event{T: time.Now(), Type: "out", B: []byte("first")})
	}
	d.waitFor("first stored", func() bool { return len(rawLines(t, d, id.Session.ID, id.Agent.ID)) == 10 })
	c1.cancel() // simulate the process being killed: no bye
	<-c1.done
	d.waitFor("marked disconnected", func() bool { return d.session(id.Session.ID).Agents[0].Status == "disconnected" })

	c2, err := Connect(bg, d.opts(proto.Hello{Session: id.Session.ID, Name: "relaunched", Token: id.Token}))
	if err != nil {
		t.Fatal(err)
	}
	if !c2.Identity().Resumed {
		t.Error("expected a resumed welcome")
	}
	c2.Send(proto.Event{T: time.Now(), Type: "out", B: []byte("second")})
	c2.Close(0)
	lines := rawLines(t, d, id.Session.ID, id.Agent.ID)
	if len(lines) != 11 || lines[10] != "second" {
		t.Fatalf("lines after resume: %v", lines)
	}
}

// TestReconnectAfterResumeUsesTheOriginalToken guards against a real bug:
// adopt() only updates c.token when a Welcome carries one, and a resume's
// Welcome never does (Token is only set on first registration - see
// proto.Welcome). A fresh Client whose very first Hello was itself a
// resume (Options.Hello.Token set, as here) must still remember that same
// token for its OWN later automatic reconnects (a network blip, or the
// daemon bouncing), or the second reconnect attempt goes out with an empty
// token, which the daemon reads as a brand new registration attempt -
// colliding on the still-occupied name and failing for good.
func TestReconnectAfterResumeUsesTheOriginalToken(t *testing.T) {
	d := newTestDaemon(t)
	c1, err := Connect(bg, d.opts(proto.Hello{Session: proto.SessionNew, Name: "phoenix"}))
	if err != nil {
		t.Fatal(err)
	}
	id := c1.Identity()
	c1.cancel() // simulate the process being killed: no bye
	<-c1.done
	d.waitFor("disconnected", func() bool { return d.session(id.Session.ID).Agents[0].Status == "disconnected" })

	c2, err := Connect(bg, d.opts(proto.Hello{Session: id.Session.ID, Name: "phoenix", Token: id.Token}))
	if err != nil {
		t.Fatal(err)
	}
	if !c2.Identity().Resumed {
		t.Fatal("expected a resumed welcome")
	}

	// Force a SECOND reconnect within c2's own lifetime.
	d.stop()
	d.waitFor("c2 offline", func() bool { return !c2.Online() })
	d.start()
	d.waitFor("c2 back online", func() bool { return c2.Online() })
	if err := c2.Err(); err != nil {
		t.Fatalf("c2 gave up reconnecting: %v", err)
	}
	if !c2.Identity().Resumed {
		t.Error("the second reconnect should also come back as resumed, not a fresh registration")
	}
	if c2.Identity().Agent.ID != id.Agent.ID {
		t.Error("agent identity changed across the second reconnect")
	}
	c2.Close(0)
}

func TestFromEventlog(t *testing.T) {
	code := 7
	cases := []struct {
		name string
		in   eventlog.Event
		ok   bool
		want func(proto.Event) bool
	}{
		{"typed key", eventlog.Event{Type: "in", Bytes: []byte("a")}, true, func(e proto.Event) bool { return string(e.B) == "a" && e.Type == "in" }},
		{"shift-tab", eventlog.Event{Type: "in", Bytes: []byte("\x1b[Z")}, true, nil},
		{"one mouse report", eventlog.Event{Type: "in", Bytes: []byte("\x1b[<35;10;20M")}, false, nil},
		{"burst of mouse reports", eventlog.Event{Type: "in", Bytes: []byte("\x1b[<35;10;20M\x1b[<35;11;21M\x1b[<0;5;5m")}, false, nil},
		{"mouse mixed with a key is kept", eventlog.Event{Type: "in", Bytes: []byte("\x1b[<35;10;20Mx")}, true, nil},
		{"output containing mouse text is kept", eventlog.Event{Type: "out", Bytes: []byte("\x1b[<35;10;20M")}, true, nil},
		{"empty", eventlog.Event{Type: "out"}, false, nil},
		{"inject", eventlog.Event{Type: "inject", Bytes: []byte("hi")}, true, nil},
		{"start", eventlog.Event{Type: "start", Tool: "claude", Args: []string{"-x"}}, true, func(e proto.Event) bool { return strings.Contains(string(e.Meta), `"tool":"claude"`) }},
		{"resize", eventlog.Event{Type: "resize", Rows: 30, Cols: 100}, true, func(e proto.Event) bool { return strings.Contains(string(e.Meta), `"rows":30`) }},
		{"exit", eventlog.Event{Type: "exit", Code: &code}, true, func(e proto.Event) bool { return strings.Contains(string(e.Meta), `"code":7`) }},
		{"unknown", eventlog.Event{Type: "weird"}, false, nil},
	}
	for _, c := range cases {
		ev, ok := FromEventlog(c.in)
		if ok != c.ok {
			t.Errorf("%s: ok=%v want %v", c.name, ok, c.ok)
			continue
		}
		if ok && c.want != nil && !c.want(ev) {
			t.Errorf("%s: bad conversion %+v", c.name, ev)
		}
	}
}

func TestCallAndDeliverAcrossTwoAgents(t *testing.T) {
	d := newTestDaemon(t)
	var mu sync.Mutex
	var got []proto.MessageView
	a, err := Connect(bg, Options{Paths: d.paths, Hello: proto.Hello{Session: proto.SessionNew, Tool: "claude", Role: "dev", Name: "alice"}, EnsureDaemon: d.ensure})
	if err != nil {
		t.Fatal(err)
	}
	defer a.Close(0)
	b, err := Connect(bg, Options{Paths: d.paths, EnsureDaemon: d.ensure,
		Hello:     proto.Hello{Session: a.Identity().Session.ID, Tool: "codex", Role: "qa", Name: "bob"},
		OnDeliver: func(m proto.MessageView) { mu.Lock(); got = append(got, m); mu.Unlock() }})
	if err != nil {
		t.Fatal(err)
	}
	defer b.Close(0)

	var l proto.ListAgentsResult
	if err := a.Call(bg, proto.OpListAgents, struct{}{}, &l); err != nil || len(l.Agents) != 2 {
		t.Fatalf("list: %v %+v", err, l)
	}
	var res proto.SendResult
	if err := a.Call(bg, proto.OpSend, proto.SendArgs{To: "bob", Body: "hello"}, &res); err != nil || res.ID == "" {
		t.Fatalf("send: %v %+v", err, res)
	}
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		mu.Lock()
		n := len(got)
		mu.Unlock()
		if n > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(got) != 1 || got[0].Body != "hello" || got[0].From != "alice" {
		t.Fatalf("delivered: %+v", got)
	}
	// errors come back typed
	err = a.Call(bg, proto.OpSend, proto.SendArgs{To: "zed", Body: "x"}, nil)
	var pe *proto.Error
	if !errors.As(err, &pe) || pe.Code != proto.CodeUnknownAgent || len(pe.Agents) != 2 {
		t.Fatalf("typed error: %v", err)
	}
}

func TestOnlyDefinitiveRefusalsStopAnAgentFromReconnecting(t *testing.T) {
	// A transient failure inside a restarting daemon must never permanently cut an agent off.
	for _, code := range []string{proto.CodeInternal, proto.CodeAgentLive, proto.CodeOffline, proto.CodeRateLimited, "something_new"} {
		if definitive(code) {
			t.Errorf("%s is transient: the client must keep retrying", code)
		}
	}
	for _, code := range []string{proto.CodeBadToken, proto.CodeSessionEnded, proto.CodeSessionNotFound, proto.CodeProtoMismatch, proto.CodeNameTaken, proto.CodeSessionFull} {
		if !definitive(code) {
			t.Errorf("%s will never succeed on retry", code)
		}
	}
}
