package transcript

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestClaudeParserOnRealShapes(t *testing.T) {
	p := ClaudeParser{}
	// user prompt as a plain string
	r := p.Parse([]byte(`{"type":"user","uuid":"u1","timestamp":"2026-09-19T21:11:49.000Z","message":{"role":"user","content":"Run the bash command"}}`))
	if len(r.Turns) != 1 || r.Turns[0].Role != "user" || r.Turns[0].Text != "Run the bash command" || r.Turns[0].TS.IsZero() {
		t.Fatalf("%+v", r)
	}
	// assistant tool call
	r = p.Parse([]byte(`{"type":"assistant","uuid":"a1","timestamp":"2026-09-19T21:11:50.000Z","message":{"role":"assistant","content":[{"type":"thinking","thinking":"hm"},{"type":"tool_use","id":"toolu_1","name":"Bash","input":{"command":"echo hi"}}]}}`))
	if len(r.Turns) != 1 || r.Turns[0].Role != "tool_call" || r.Turns[0].Tool != "Bash" || !strings.Contains(r.Turns[0].Text, "echo hi") {
		t.Fatalf("thinking must be skipped: %+v", r)
	}
	// tool result, content as array and as string
	r = p.Parse([]byte(`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_1","content":[{"type":"text","text":"hi"}]}]}}`))
	if len(r.Turns) != 1 || r.Turns[0].Role != "tool_result" || r.Turns[0].Text != "hi" || r.Turns[0].ID != "toolu_1" {
		t.Fatalf("%+v", r)
	}
	r = p.Parse([]byte(`{"type":"user","message":{"role":"user","content":[{"type":"tool_result","tool_use_id":"t2","content":"plain"}]}}`))
	if r.Turns[0].Text != "plain" {
		t.Fatalf("%+v", r)
	}
	// assistant text
	r = p.Parse([]byte(`{"type":"assistant","message":{"role":"assistant","content":[{"type":"text","text":"done."}]}}`))
	if len(r.Turns) != 1 || r.Turns[0].Role != "assistant" || r.Turns[0].Text != "done." {
		t.Fatalf("%+v", r)
	}
	// a relay message delivered by a hook shows up as an attachment
	r = p.Parse([]byte(`{"type":"attachment","uuid":"h1","timestamp":"2026-09-19T21:11:50.990Z","attachment":{"type":"hook_additional_context","content":["[relay | from bob | task | high | msg 01M2XP1EQFJ780GSC4ED8KNECA]\nhi"],"hookEvent":"PostToolUse"}}`))
	if len(r.Turns) != 1 || r.Turns[0].Role != "system" {
		t.Fatalf("%+v", r)
	}
	if ids := MsgIDs(r.Turns[0].Text); len(ids) != 1 || ids[0] != "01M2XP1EQFJ780GSC4ED8KNECA" {
		t.Fatalf("ids %v", ids)
	}
	// Claude's own insertions (isMeta) are system turns: this is where a Stop-continue
	// delivery leaves its evidence
	r = p.Parse([]byte(`{"type":"user","isMeta":true,"uuid":"m1","message":{"role":"user","content":"Stop hook feedback:\n[relay | from user | task | normal | msg 01M2XRXJPVXTD905SR2JW7KRJR]\nhello"}}`))
	if len(r.Turns) != 1 || r.Turns[0].Role != "system" || len(MsgIDs(r.Turns[0].Text)) != 1 {
		t.Fatalf("%+v", r)
	}
	// noise is ignored
	for _, l := range []string{`{"type":"queue-operation"}`, `{"type":"last-prompt"}`, `not json`, `{"type":"attachment","attachment":{"type":"hook_success"}}`} {
		if r := p.Parse([]byte(l)); len(r.Turns) != 0 || r.Signal != "" {
			t.Errorf("%s should yield nothing: %+v", l, r)
		}
	}
}

func TestCodexParserOnRealShapes(t *testing.T) {
	p := CodexParser{}
	line := func(typ, payload string) []byte {
		return fmt.Appendf(nil, `{"timestamp":"2026-09-19T20:34:33.100Z","type":"event_msg","payload":{"type":%q,%s}}`, typ, payload)
	}
	if r := p.Parse(line("task_started", `"turn_id":"t1","collaboration_mode_kind":"default"`)); r.Signal != SigTaskStarted {
		t.Fatalf("%+v", r)
	}
	if r := p.Parse(line("task_complete", `"turn_id":"t1","last_agent_message":"PONG-7"`)); r.Signal != SigTaskComplete {
		t.Fatalf("%+v", r)
	}
	// a finished plan waits for the user's decision: NOT a delivery point
	if r := p.Parse(line("task_complete", `"last_agent_message":"Here:\n<proposed_plan>\n1. x\n</proposed_plan>"`)); r.Signal != SigPlanPending {
		t.Fatalf("%+v", r)
	}
	if r := p.Parse(line("turn_aborted", `"turn_id":"t1"`)); r.Signal != SigAborted {
		t.Fatalf("%+v", r)
	}
	r := p.Parse(line("item_completed", `"item":{"type":"UserMessage","id":"i1","content":[{"type":"text","text":"[relay | from lead (orchestrator) | task | normal | msg 01M2XP1EQFJ780GSC4ED8KNECA]\nhello"}]}`))
	if len(r.Turns) != 1 || r.Turns[0].Role != "user" || len(MsgIDs(r.Turns[0].Text)) != 1 || r.Turns[0].TS.IsZero() {
		t.Fatalf("%+v", r)
	}
	r = p.Parse(line("item_completed", `"item":{"type":"AgentMessage","id":"i2","content":[{"type":"Text","text":"PONG-7"}],"phase":"final_answer"}`))
	if len(r.Turns) != 1 || r.Turns[0].Role != "assistant" || r.Turns[0].Text != "PONG-7" {
		t.Fatalf("%+v", r)
	}
	r = p.Parse(line("item_completed", `"item":{"type":"McpToolCall","id":"i3","server":"relay","tool":"relay_send","arguments":{"to":"lead"},"result":{"content":[{"type":"text","text":"{\"msg_id\":\"X\"}"}]}}`))
	if len(r.Turns) != 2 || r.Turns[0].Tool != "relay.relay_send" || r.Turns[1].Role != "tool_result" || !strings.Contains(r.Turns[1].Text, "msg_id") {
		t.Fatalf("%+v", r)
	}
	r = p.Parse(line("item_completed", `"item":{"type":"CommandExecution","id":"i4","command":["/bin/bash","-lc","ls -la"],"aggregated_output":"total 0"}`))
	if len(r.Turns) != 2 || r.Turns[0].Text != "/bin/bash -lc ls -la" || r.Turns[1].Text != "total 0" {
		t.Fatalf("%+v", r)
	}
	// reasoning, response_item duplicates and other records are ignored
	for _, l := range []string{
		string(line("item_completed", `"item":{"type":"Reasoning","id":"r"}`)),
		`{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"dup"}]}}`,
		`{"type":"turn_context","payload":{}}`, `garbage`,
	} {
		if r := p.Parse([]byte(l)); len(r.Turns) != 0 || r.Signal != "" {
			t.Errorf("%s should yield nothing: %+v", l, r)
		}
	}
}

func TestCopilotParserOnRealShapes(t *testing.T) {
	p := CopilotParser{}
	// shapes below are copied verbatim (field names, nesting) from a real
	// copilot 1.0.88 session's events.jsonl, captured live in an isolated
	// tmux server per the issue #17 plan's live-verification phase.
	r := p.Parse([]byte(`{"type":"user.message","data":{"content":"say hello","messageId":"m1","turnId":"0"},"id":"e1","timestamp":"2026-09-25T17:04:43.874Z"}`))
	if len(r.Turns) != 1 || r.Turns[0].Role != "user" || r.Turns[0].Text != "say hello" || r.Turns[0].TS.IsZero() {
		t.Fatalf("%+v", r)
	}
	if r := p.Parse([]byte(`{"type":"assistant.turn_start","data":{"turnId":"0"},"id":"e2","timestamp":"2026-09-25T17:04:43.878Z"}`)); r.Signal != SigTaskStarted {
		t.Fatalf("%+v", r)
	}
	if r := p.Parse([]byte(`{"type":"assistant.turn_end","data":{"turnId":"0"},"id":"e3","timestamp":"2026-09-25T17:04:46.520Z"}`)); r.Signal != SigTaskComplete {
		t.Fatalf("%+v", r)
	}
	if r := p.Parse([]byte(`{"type":"permission.requested","data":{"requestId":"r1","agentMode":"interactive","permissionMode":"manual"},"id":"e4","timestamp":"2026-09-25T17:04:46.600Z"}`)); r.Signal != SigPlanPending {
		t.Fatalf("a pending permission dialog must map to the shared dialog signal: %+v", r)
	}
	if r := p.Parse([]byte(`{"type":"permission.completed","data":{"requestId":"r1","toolCallId":"t1"},"id":"e5","timestamp":"2026-09-25T17:04:47.000Z"}`)); r.Signal != SigTaskStarted {
		t.Fatalf("resolving a dialog resumes work: %+v", r)
	}
	// ask_user is a separate dialog mechanism from permission prompts
	// (confirmed live: it emits no permission.requested at all), captured
	// from the same real session.
	if r := p.Parse([]byte(`{"type":"tool.execution_start","data":{"toolCallId":"c1","toolName":"ask_user","arguments":{"message":"What would you like me to work on?"}},"id":"e6","timestamp":"2026-09-25T17:04:47.500Z"}`)); r.Signal != SigPlanPending {
		t.Fatalf("ask_user must be treated as a dialog: %+v", r)
	}
	if r := p.Parse([]byte(`{"type":"tool.execution_start","data":{"toolCallId":"c2","toolName":"shell","arguments":{}},"id":"e7","timestamp":"2026-09-25T17:04:47.600Z"}`)); r.Signal != "" {
		t.Fatalf("an ordinary tool starting is not a dialog: %+v", r)
	}
	if r := p.Parse([]byte(`{"type":"tool.execution_complete","data":{"toolCallId":"c1","success":true},"id":"e8","timestamp":"2026-09-25T17:04:48.000Z"}`)); r.Signal != SigTaskStarted {
		t.Fatalf("a completed tool resumes work: %+v", r)
	}
	// events we do not act on, and malformed/corrupted lines: never crash, never signal
	for _, l := range []string{
		`{"type":"session.start","data":{"sessionId":"s1","context":{"cwd":"/x"}}}`,
		`{"type":"session.shutdown","data":{"shutdownType":"routine"}}`,
		`{"type":"tool.execution_start","data":{"toolCallId":"t1","toolName":"shell"}}`,
		`not json`, "", `{`, `{"type":"user.message","data":{"content":""}}`,
	} {
		if r := p.Parse([]byte(l)); len(r.Turns) != 0 || r.Signal != "" {
			t.Errorf("%q should yield nothing: %+v", l, r)
		}
	}
}

func TestClipKeepsUTF8AndMarksTruncation(t *testing.T) {
	long := strings.Repeat("é", MaxTurnText) // 2 bytes each
	got := clip(long)
	if len(got) > MaxTurnText+len("…[truncated]") || !strings.HasSuffix(got, "…[truncated]") {
		t.Fatalf("len %d", len(got))
	}
	if strings.ContainsRune(strings.TrimSuffix(got, "…[truncated]"), '�') {
		t.Fatal("split a character")
	}
	if clip("short") != "short" {
		t.Fatal("short text untouched")
	}
}

func TestTailerFollowsAppendsAndPartialLines(t *testing.T) {
	path := filepath.Join(t.TempDir(), "t.jsonl")
	var mu sync.Mutex
	var got []string
	tl := &Tailer{Path: path, Parse: ClaudeParser{}, Every: 10 * time.Millisecond, Fn: func(r Record) {
		mu.Lock()
		for _, tn := range r.Turns {
			got = append(got, tn.Text)
		}
		mu.Unlock()
	}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go tl.Run(ctx) // the file does not exist yet

	time.Sleep(50 * time.Millisecond)
	f, _ := os.OpenFile(path, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o600)
	defer f.Close()
	mk := func(s string) string {
		return fmt.Sprintf(`{"type":"user","message":{"role":"user","content":%q}}`, s)
	}
	f.WriteString(mk("one") + "\n")
	half := mk("two")
	f.WriteString(half[:20]) // a line still being written
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(got) == 1 })
	f.WriteString(half[20:] + "\n" + mk("three") + "\n")
	waitFor(t, func() bool { mu.Lock(); defer mu.Unlock(); return len(got) == 3 })
	mu.Lock()
	defer mu.Unlock()
	if strings.Join(got, ",") != "one,two,three" {
		t.Fatalf("%v", got)
	}
}

func TestTailerEntersLargeFilesNearTheEnd(t *testing.T) {
	path := filepath.Join(t.TempDir(), "big.jsonl")
	var b strings.Builder
	for i := 0; i < 2000; i++ {
		fmt.Fprintf(&b, `{"type":"user","message":{"role":"user","content":"line-%04d padding padding padding padding"}}`+"\n", i)
	}
	os.WriteFile(path, []byte(b.String()), 0o600)
	var mu sync.Mutex
	var got []string
	tl := &Tailer{Path: path, Parse: ClaudeParser{}, Every: 10 * time.Millisecond, Backlog: 10 << 10, Fn: func(r Record) {
		mu.Lock()
		got = append(got, r.Turns[0].Text)
		mu.Unlock()
	}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go tl.Run(ctx)
	// Lines arrive one at a time: wait for the last one, not just for "some".
	waitFor(t, func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(got) > 10 && strings.HasPrefix(got[len(got)-1], "line-1999")
	})
	mu.Lock()
	defer mu.Unlock()
	if got[0] == "line-0000 padding padding padding padding" || !strings.HasPrefix(got[len(got)-1], "line-1999") {
		t.Fatalf("expected only the recent tail, first=%q last=%q n=%d", got[0], got[len(got)-1], len(got))
	}
	for _, g := range got { // no half-lines
		if !strings.HasPrefix(g, "line-") {
			t.Fatalf("partial line %q", g)
		}
	}
}

func waitFor(t *testing.T, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if ok() {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("timed out")
}

func writeRollout(t *testing.T, root, name, cwd, developer string, mod time.Time) string {
	t.Helper()
	dir := filepath.Join(root, mod.Format("2006"), mod.Format("01"), mod.Format("02"))
	os.MkdirAll(dir, 0o755)
	p := filepath.Join(dir, name)
	body := fmt.Sprintf(`{"type":"session_meta","payload":{"cwd":%q}}`+"\n", cwd)
	if developer != "" {
		body += fmt.Sprintf(`{"type":"response_item","payload":{"type":"message","role":"developer","content":[{"type":"input_text","text":%q}]}}`+"\n", developer)
	}
	os.WriteFile(p, []byte(body), 0o644)
	os.Chtimes(p, mod, mod)
	return p
}

func TestLocateCodex(t *testing.T) {
	root := t.TempDir()
	cwd := t.TempDir()
	start := time.Now().Add(-time.Minute)
	sid := "01M2XNZTGDP5NB5JJ1709WZ5WV"

	// nothing yet
	if got := LocateCodex(CodexQuery{Root: root, Cwd: cwd, Since: start, Name: "coder", Session: sid}); got != "" {
		t.Fatalf("%q", got)
	}
	old := writeRollout(t, root, "rollout-old.jsonl", cwd, "", start.Add(-time.Hour))
	elsewhere := writeRollout(t, root, "rollout-else.jsonl", "/some/other/dir", "", time.Now())
	_, _ = old, elsewhere
	if got := LocateCodex(CodexQuery{Root: root, Cwd: cwd, Since: start, Name: "coder", Session: sid}); got != "" {
		t.Fatalf("stale and foreign rollouts must not match: %q", got)
	}
	// two relay agents share a directory: the briefing tells them apart
	mine := writeRollout(t, root, "rollout-a.jsonl", cwd, `You are "coder", working in a Relay session (id `+sid+`) alongside...`, time.Now())
	theirs := writeRollout(t, root, "rollout-b.jsonl", cwd, `You are "tester", working in a Relay session (id `+sid+`) alongside...`, time.Now())
	if got := LocateCodex(CodexQuery{Root: root, Cwd: cwd, Since: start, Name: "coder", Session: sid}); got != mine {
		t.Fatalf("want %s, got %s", mine, got)
	}
	if got := LocateCodex(CodexQuery{Root: root, Cwd: cwd, Since: start, Name: "tester", Session: sid}); got != theirs {
		t.Fatalf("want %s, got %s", theirs, got)
	}
	// an agent whose briefing was typed (no developer message): never steal a relay-briefed rollout
	if got := LocateCodex(CodexQuery{Root: root, Cwd: cwd, Since: start, Name: "reviewer", Session: sid}); got != "" {
		t.Fatalf("must not claim another agent's rollout: %s", got)
	}
	// a lone plain rollout in the directory is ours
	root2 := t.TempDir()
	plain := writeRollout(t, root2, "rollout-p.jsonl", cwd, "", time.Now())
	if got := LocateCodex(CodexQuery{Root: root2, Cwd: cwd, Since: start, Name: "coder", Session: sid}); got != plain {
		t.Fatalf("want %s, got %s", plain, got)
	}
}

func writeCopilotSession(t *testing.T, root, id, cwd, userText string, mod time.Time) string {
	t.Helper()
	dir := filepath.Join(root, id)
	os.MkdirAll(dir, 0o755)
	p := filepath.Join(dir, "events.jsonl")
	body := fmt.Sprintf(`{"type":"session.start","data":{"sessionId":%q,"context":{"cwd":%q}}}`+"\n", id, cwd)
	if userText != "" {
		body += fmt.Sprintf(`{"type":"user.message","data":{"content":%q}}`+"\n", userText)
	}
	os.WriteFile(p, []byte(body), 0o644)
	os.Chtimes(p, mod, mod)
	return p
}

func TestLocateCopilot(t *testing.T) {
	root := t.TempDir()
	cwd := t.TempDir()
	start := time.Now().Add(-time.Minute)
	sid := "01M2XNZTGDP5NB5JJ1709WZ5WV"

	// nothing yet
	if got := LocateCopilot(CopilotQuery{Root: root, Cwd: cwd, Since: start, Name: "coder", Session: sid}); got != "" {
		t.Fatalf("%q", got)
	}
	old := writeCopilotSession(t, root, "old", cwd, "", start.Add(-time.Hour))
	elsewhere := writeCopilotSession(t, root, "else", "/some/other/dir", "", time.Now())
	_, _ = old, elsewhere
	if got := LocateCopilot(CopilotQuery{Root: root, Cwd: cwd, Since: start, Name: "coder", Session: sid}); got != "" {
		t.Fatalf("stale and foreign sessions must not match: %q", got)
	}
	// two relay agents share a directory: the typed bootstrap message tells them apart
	mine := writeCopilotSession(t, root, "a", cwd, `You are "coder", working in a Relay session (id `+sid+`) alongside...`, time.Now())
	theirs := writeCopilotSession(t, root, "b", cwd, `You are "tester", working in a Relay session (id `+sid+`) alongside...`, time.Now())
	if got := LocateCopilot(CopilotQuery{Root: root, Cwd: cwd, Since: start, Name: "coder", Session: sid}); got != mine {
		t.Fatalf("want %s, got %s", mine, got)
	}
	if got := LocateCopilot(CopilotQuery{Root: root, Cwd: cwd, Since: start, Name: "tester", Session: sid}); got != theirs {
		t.Fatalf("want %s, got %s", theirs, got)
	}
	// an agent whose briefing was typed under a different name: never steal a relay-briefed session
	if got := LocateCopilot(CopilotQuery{Root: root, Cwd: cwd, Since: start, Name: "reviewer", Session: sid}); got != "" {
		t.Fatalf("must not claim another agent's session: %s", got)
	}
	// a lone plain session in the directory is ours
	root2 := t.TempDir()
	plain := writeCopilotSession(t, root2, "p", cwd, "", time.Now())
	if got := LocateCopilot(CopilotQuery{Root: root2, Cwd: cwd, Since: start, Name: "coder", Session: sid}); got != plain {
		t.Fatalf("want %s, got %s", plain, got)
	}
}

func TestOnlyRealHeadersConfirmDelivery(t *testing.T) {
	id := "01M2XP1EQFJ780GSC4ED8KNECA"
	real := "[relay | from bob (qa) | task | normal | msg " + id + "]\nhello"
	if got := MsgIDs("Stop hook feedback:\n[relay] Message(s) arrived:\n\n" + real); len(got) != 1 || got[0] != id {
		t.Fatalf("real header: %v", got)
	}
	for _, spoof := range []string{
		"please note | msg " + id + "] is done",                     // mid-line text
		"[relay ¦ from x | task | high | msg " + id + "]",           // defanged by the sender side
		"see [relay | from bob | msg " + id + "] in my notes",       // header not at line start
		"[relay | from bob | msg " + id + "] trailing text on line", // header must be the whole line
	} {
		if got := MsgIDs(spoof); len(got) != 0 {
			t.Errorf("%q must not confirm anything: %v", spoof, got)
		}
	}
}
