package proto

import (
	"encoding/json"
	"testing"
)

// FuzzEnvelope: the daemon decodes whatever a peer sends. Nothing may panic.
func FuzzEnvelope(f *testing.F) {
	for _, seed := range []string{
		`{"v":2,"type":"hello","payload":{"proto":2,"tool":"claude","role":"dev"}}`,
		`{"v":2,"type":"rpc","payload":{"id":"1","op":"send","args":{"to":"x","body":"y"}}}`,
		`{"type":"events","payload":{"events":[{"seq":1,"t":"2026-01-01T00:00:00Z","type":"out","b":"AAEC"}]}}`,
		`{}`, `[]`, `null`, `{"type":"x","payload":`, "\x00", `{"type":"rpc","payload":{"args":"\u0000"}}`,
	} {
		f.Add([]byte(seed))
	}
	f.Fuzz(func(t *testing.T, data []byte) {
		env, err := Unmarshal(data)
		if err != nil {
			return
		}
		var h Hello
		var r RPC
		var e Events
		var s SendArgs
		_ = json.Unmarshal(env.Payload, &h)
		_ = json.Unmarshal(env.Payload, &r)
		_ = json.Unmarshal(env.Payload, &e)
		_ = json.Unmarshal(r.Args, &s)
		_, _ = ParsePriority(s.Priority)
		_ = ValidRole(h.Role)
		_ = ValidTool(h.Tool)
		if b, err := Marshal(env.Type, env.Payload); err == nil && len(b) == 0 {
			t.Fatal("empty marshal")
		}
	})
}
