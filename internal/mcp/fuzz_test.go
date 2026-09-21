package mcp

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"
)

// FuzzServe feeds arbitrary lines to the MCP server: it must not panic, must
// answer with well-formed JSON only, and must finish when stdin ends.
func FuzzServe(f *testing.F) {
	for _, s := range []string{
		`{"jsonrpc":"2.0","id":1,"method":"initialize","params":{"protocolVersion":"2025-06-18"}}`,
		`{"jsonrpc":"2.0","id":2,"method":"tools/list"}`,
		`{"jsonrpc":"2.0","id":3,"method":"tools/call","params":{"name":"relay_send","arguments":{"to":"x","body":"y"}}}`,
		`{"jsonrpc":"2.0","method":"notifications/cancelled","params":{"requestId":3}}`,
		`{"id":"a","method":7}`, `[]`, `{"jsonrpc":"2.0","id":null,"method":"ping"}`, "\x00\x01",
	} {
		f.Add(s)
	}
	f.Fuzz(func(t *testing.T, line string) {
		var out bytes.Buffer
		s := &Server{Version: "fuzz", Backend: &fakeBackend{}}
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := s.Serve(ctx, strings.NewReader(line+"\n"), &out); err != nil {
			t.Fatalf("serve: %v", err)
		}
		for _, l := range strings.Split(strings.TrimSpace(out.String()), "\n") {
			if l != "" && !json.Valid([]byte(l)) {
				t.Fatalf("invalid JSON reply %q to %q", l, line)
			}
		}
	})
}
