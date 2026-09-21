package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/thesahibnanda-max/relay/internal/proto"
)

type fakeBackend struct {
	calls   chan string
	pending int
	fail    error
	block   bool
}

func (f *fakeBackend) Call(ctx context.Context, op string, args json.RawMessage) (json.RawMessage, int, error) {
	if f.calls != nil {
		f.calls <- op + " " + string(args)
	}
	if f.block {
		<-ctx.Done()
		return nil, 0, ctx.Err()
	}
	if f.fail != nil {
		return nil, 0, f.fail
	}
	return json.RawMessage(`{"msg_id":"01ABC"}`), f.pending, nil
}

type client struct {
	t   *testing.T
	in  io.WriteCloser
	out *bufio.Reader
}

func start(t *testing.T, b Backend) *client {
	t.Helper()
	ir, iw := io.Pipe()
	or, ow := io.Pipe()
	s := &Server{Version: "test", Backend: b, Instructions: "be nice"}
	go func() { s.Serve(context.Background(), ir, ow); ow.Close() }()
	t.Cleanup(func() { iw.Close() })
	return &client{t: t, in: iw, out: bufio.NewReader(or)}
}

func (c *client) send(v any) {
	b, _ := json.Marshal(v)
	c.in.Write(append(b, '\n'))
}

func (c *client) read() map[string]any {
	c.t.Helper()
	done := make(chan map[string]any, 1)
	go func() {
		line, err := c.out.ReadBytes('\n')
		if err != nil {
			done <- nil
			return
		}
		var m map[string]any
		json.Unmarshal(line, &m)
		done <- m
	}()
	select {
	case m := <-done:
		if m == nil {
			c.t.Fatal("server closed")
		}
		return m
	case <-time.After(3 * time.Second):
		c.t.Fatal("no reply")
		return nil
	}
}

func text(t *testing.T, m map[string]any) (string, bool) {
	t.Helper()
	res := m["result"].(map[string]any)
	content := res["content"].([]any)[0].(map[string]any)
	isErr, _ := res["isError"].(bool)
	return content["text"].(string), isErr
}

func TestHandshakeAndToolList(t *testing.T) {
	c := start(t, &fakeBackend{})
	c.send(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "initialize", "params": map[string]any{"protocolVersion": "2025-03-26"}})
	r := c.read()["result"].(map[string]any)
	if r["protocolVersion"] != "2025-03-26" || r["serverInfo"].(map[string]any)["name"] != "relay" || r["instructions"] != "be nice" {
		t.Fatalf("initialize: %v", r)
	}
	c.send(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "initialize", "params": map[string]any{"protocolVersion": "1999-01-01"}})
	if v := c.read()["result"].(map[string]any)["protocolVersion"]; v != supportedVersions[0] {
		t.Fatalf("unknown versions negotiate down to the latest we speak, got %v", v)
	}
	c.send(map[string]any{"jsonrpc": "2.0", "method": "notifications/initialized"}) // no reply
	c.send(map[string]any{"jsonrpc": "2.0", "id": 3, "method": "ping"})
	if m := c.read(); m["id"].(float64) != 3 {
		t.Fatalf("ping: %v", m)
	}
	c.send(map[string]any{"jsonrpc": "2.0", "id": 4, "method": "tools/list"})
	tools := c.read()["result"].(map[string]any)["tools"].([]any)
	names := map[string]bool{}
	for _, x := range tools {
		tool := x.(map[string]any)
		names[tool["name"].(string)] = true
		if tool["description"] == "" || tool["inputSchema"].(map[string]any)["type"] != "object" {
			t.Errorf("bad tool %v", tool)
		}
	}
	for _, want := range []string{"relay_whoami", "relay_list_agents", "relay_send", "relay_inbox", "relay_ack", "relay_wait", "relay_get_context"} {
		if !names[want] {
			t.Errorf("missing tool %s", want)
		}
	}
	c.send(map[string]any{"jsonrpc": "2.0", "id": 5, "method": "resources/list"})
	if e := c.read()["error"].(map[string]any); e["code"].(float64) != -32601 {
		t.Fatalf("unknown methods: %v", e)
	}
}

func TestToolCallForwardsAndAddsInboxHint(t *testing.T) {
	fb := &fakeBackend{calls: make(chan string, 4), pending: 2}
	c := start(t, fb)
	c.send(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": "relay_send", "arguments": map[string]any{"to": "bob", "body": "hi"}}})
	txt, isErr := text(t, c.read())
	if isErr || !strings.Contains(txt, "01ABC") || !strings.Contains(txt, "2 relay message(s) are waiting") {
		t.Fatalf("result: %q err=%v", txt, isErr)
	}
	if got := <-fb.calls; got != `send {"body":"hi","to":"bob"}` {
		t.Fatalf("forwarded %q", got)
	}
	// missing arguments become an empty object
	c.send(map[string]any{"jsonrpc": "2.0", "id": 2, "method": "tools/call", "params": map[string]any{"name": "relay_whoami"}})
	c.read()
	if got := <-fb.calls; got != "whoami {}" {
		t.Fatalf("forwarded %q", got)
	}
	c.send(map[string]any{"jsonrpc": "2.0", "id": 3, "method": "tools/call", "params": map[string]any{"name": "nope"}})
	if c.read()["error"] == nil {
		t.Fatal("unknown tool must be a protocol error")
	}
}

func TestToolErrorsAreReadableAndListAgents(t *testing.T) {
	c := start(t, &fakeBackend{fail: &proto.Error{Code: proto.CodeUnknownAgent, Message: `no agent named "dave"`, Agents: []proto.PeerInfo{{Name: "bob", Role: "qa", Tool: "codex", State: "idle"}}}})
	c.send(map[string]any{"jsonrpc": "2.0", "id": 1, "method": "tools/call", "params": map[string]any{"name": "relay_send", "arguments": map[string]any{"to": "dave", "body": "x"}}})
	txt, isErr := text(t, c.read())
	if !isErr || !strings.Contains(txt, "unknown_agent") || !strings.Contains(txt, `"name": "bob"`) {
		t.Fatalf("%q", txt)
	}
}

func TestCancelledCallStopsWork(t *testing.T) {
	fb := &fakeBackend{calls: make(chan string, 1), block: true}
	c := start(t, fb)
	c.send(map[string]any{"jsonrpc": "2.0", "id": 7, "method": "tools/call", "params": map[string]any{"name": "relay_wait", "arguments": map[string]any{"msg_id": "x"}}})
	<-fb.calls
	c.send(map[string]any{"jsonrpc": "2.0", "method": "notifications/cancelled", "params": map[string]any{"requestId": 7}})
	txt, isErr := text(t, c.read())
	if !isErr || !strings.Contains(txt, "canceled") {
		t.Fatalf("%q %v", txt, isErr)
	}
}

// Strict MCP clients (Claude Code) silently drop tools whose schema is invalid.
func TestToolSchemasAreValidJSONSchema(t *testing.T) {
	for _, tool := range Tools() {
		b, _ := json.Marshal(tool)
		var m map[string]any
		json.Unmarshal(b, &m)
		schema := m["inputSchema"].(map[string]any)
		if schema["type"] != "object" {
			t.Errorf("%s: type %v", tool.Name, schema["type"])
		}
		props, ok := schema["properties"].(map[string]any)
		if !ok {
			t.Errorf("%s: properties must be an object", tool.Name)
		}
		if req, present := schema["required"]; present {
			list, ok := req.([]any)
			if !ok || len(list) == 0 {
				t.Errorf("%s: required must be a non-empty array, got %v", tool.Name, req)
			}
			for _, r := range list {
				if _, ok := props[r.(string)]; !ok {
					t.Errorf("%s: required %q is not a property", tool.Name, r)
				}
			}
		}
		for name, p := range props {
			pm := p.(map[string]any)
			if pm["type"] == nil {
				t.Errorf("%s.%s has no type", tool.Name, name)
			}
		}
		if len(tool.Name) > 64 || tool.Description == "" {
			t.Errorf("%s: bad name/description", tool.Name)
		}
	}
}
