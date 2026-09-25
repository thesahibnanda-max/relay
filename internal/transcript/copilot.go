package transcript

import (
	"encoding/json"
	"time"
)

// CopilotParser reads GitHub Copilot CLI's per-session event log
// (~/.copilot/session-state/{id}/events.jsonl, or under $COPILOT_HOME). The
// format is undocumented and explicitly marked unstable upstream (GitHub has
// an open feature request to formalize it) - so every line is parsed
// defensively: malformed JSON or an unrecognized type yields an empty
// Record, never an error, exactly like CodexParser's contract.
//
// Verified live against copilot 1.0.88 (see the issue #17 plan's "live
// verification phase"): every event is shaped
// {"type":"...","data":{...},"id":"...","timestamp":"...","parentId":"..."}.
// A single user prompt can produce more than one assistant.turn_start/
// assistant.turn_end pair (the model can carry straight on into a second
// internal turn after finishing a tool call, with no new user.message in
// between) - matching this codebase's existing treatment of Codex's
// task_started/task_complete, a turn boundary is trusted as-is rather than
// debounced, since the same brief tool-continuation window already exists
// there.
//
// permission.requested/permission.completed are a real, structured
// alternative to screen-scraping for Copilot's own permission dialogs
// (confirmed live: they bracket a shell/write tool call that needs
// approval). This is more reliable than ScreenRules alone, which is kept as
// a defense-in-depth backstop for the brief window before this file's
// tailer catches up.
//
// Copilot's ask_user tool is a SEPARATE dialog mechanism from permission
// prompts (confirmed live: a real session hit it, and it emits no
// permission.requested at all - only a tool.execution_start with
// toolName "ask_user"). It opens its own free-text input box waiting on the
// human, so it is treated as a dialog exactly like a permission prompt;
// tool.execution_complete closing ANY tool (not just ask_user) resumes work,
// matching how permission.completed is already handled.
type CopilotParser struct{}

type copilotLine struct {
	Type      string `json:"type"`
	Timestamp string `json:"timestamp"`
	Data      struct {
		Content  string `json:"content"`  // user.message
		ToolName string `json:"toolName"` // tool.execution_start
	} `json:"data"`
}

func (CopilotParser) Parse(line []byte) Record {
	var l copilotLine
	if json.Unmarshal(line, &l) != nil {
		return Record{}
	}
	at, _ := time.Parse(time.RFC3339Nano, l.Timestamp)
	switch l.Type {
	case "assistant.turn_start":
		return Record{Signal: SigTaskStarted}
	case "assistant.turn_end":
		return Record{Signal: SigTaskComplete}
	case "permission.requested":
		return Record{Signal: SigPlanPending} // reused generically: a dialog is awaiting the user's decision
	case "tool.execution_start":
		if l.Data.ToolName == "ask_user" {
			return Record{Signal: SigPlanPending}
		}
	case "permission.completed", "tool.execution_complete":
		return Record{Signal: SigTaskStarted} // the tool is about to run/finish: busy, not idle
	case "user.message":
		if l.Data.Content != "" {
			return Record{Turns: []Turn{{TS: at, Role: "user", Text: clip(l.Data.Content)}}}
		}
	}
	return Record{}
}
