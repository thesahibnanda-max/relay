package transcript

import (
	"strings"
	"testing"
)

func fuzzParser(f *testing.F, p Parser, seeds ...string) {
	for _, s := range seeds {
		f.Add([]byte(s))
	}
	f.Fuzz(func(t *testing.T, line []byte) {
		rec := p.Parse(line)
		for _, tn := range rec.Turns {
			if len(tn.Text) > MaxTurnText+len("…[truncated]") {
				t.Fatalf("unbounded turn text: %d bytes", len(tn.Text))
			}
		}
		_ = MsgIDs(strings.Repeat("x", 3) + string(line))
	})
}

func FuzzClaudeParser(f *testing.F) {
	fuzzParser(f, ClaudeParser{},
		`{"type":"user","message":{"role":"user","content":"hi"}}`,
		`{"type":"assistant","message":{"content":[{"type":"tool_use","id":"x","name":"Bash","input":{"a":1}}]}}`,
		`{"type":"attachment","attachment":{"type":"hook_additional_context","content":["a","b"]}}`,
		`{"type":"user","message":{"content":[{"type":"tool_result","content":[{"type":"text","text":"ok"}]}]}}`,
		`{"type":"user","message":{"content":123}}`, `{"message":null}`, `{`, ``)
}

func FuzzCodexParser(f *testing.F) {
	fuzzParser(f, CodexParser{},
		`{"timestamp":"2026-09-19T20:34:33.100Z","type":"event_msg","payload":{"type":"task_started"}}`,
		`{"type":"event_msg","payload":{"type":"task_complete","last_agent_message":"<proposed_plan>"}}`,
		`{"type":"event_msg","payload":{"type":"item_completed","item":{"type":"McpToolCall","server":"a","tool":"b","arguments":{},"result":{"content":[{"text":"x"}]}}}}`,
		`{"type":"event_msg","payload":{"type":"item_completed","item":{"type":"CommandExecution","command":["a","b"],"stdout":"o"}}}`,
		`{"type":"event_msg","payload":{"type":"item_completed","item":{"type":"CommandExecution","command":"notalist"}}}`, `{"payload":1}`, `x`)
}
