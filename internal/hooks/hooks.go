// Package hooks is Relay's side of Claude Code's hook protocol. Hooks are
// registered for ONE launch only, through an inline --settings file in the
// agent's run directory (Claude merges it with the user's own hooks and never
// stores it), and each hook is a `relay hook` process that asks the running
// agent what to do. If the agent is gone the hook does nothing.
package hooks

import (
	"encoding/json"
	"strings"
)

// Events Relay listens to.
var Events = []string{"SessionStart", "UserPromptSubmit", "PostToolUse", "Stop", "Notification"}

// Input is the part of Claude's hook payload Relay uses.
type Input struct {
	HookEventName    string `json:"hook_event_name"`
	SessionID        string `json:"session_id"`
	TranscriptPath   string `json:"transcript_path"`
	Cwd              string `json:"cwd"`
	Prompt           string `json:"prompt"`
	ToolName         string `json:"tool_name"`
	Message          string `json:"message"`
	NotificationType string `json:"notification_type"`
	StopHookActive   bool   `json:"stop_hook_active"`
}

// AdditionalContext is the hook output that adds text to the model's context
// (PostToolUse) without interrupting it.
func AdditionalContext(event, text string) string {
	b, _ := json.Marshal(map[string]any{"hookSpecificOutput": map[string]any{"hookEventName": event, "additionalContext": text}})
	return string(b)
}

// Block is the Stop hook output that keeps the turn going, with reason as the
// instruction for the next step.
func Block(reason string) string {
	b, _ := json.Marshal(map[string]any{"decision": "block", "reason": reason})
	return string(b)
}

// Quote makes s safe as one word in a POSIX shell command line.
func Quote(s string) string { return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'" }

// Command is the shell command line of a Relay hook for one event.
func Command(relayExe, runDir, event string) string {
	return Quote(relayExe) + " hook " + event + " --dir " + Quote(runDir)
}

// SettingsJSON renders the per-launch settings file.
func SettingsJSON(relayExe, runDir string) []byte {
	h := map[string]any{}
	for _, ev := range Events {
		entry := map[string]any{"hooks": []any{map[string]any{"type": "command", "command": Command(relayExe, runDir, ev), "timeout": 10}}}
		if ev == "PostToolUse" {
			entry["matcher"] = "*"
		}
		h[ev] = []any{entry}
	}
	b, _ := json.Marshal(map[string]any{"hooks": h})
	return b
}
