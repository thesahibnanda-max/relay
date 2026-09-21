package transcript

import (
	"encoding/json"
	"strings"
	"time"
)

// ClaudeParser reads Claude Code's transcript JSONL.
type ClaudeParser struct{}

type claudeLine struct {
	Type      string `json:"type"`
	UUID      string `json:"uuid"`
	Timestamp string `json:"timestamp"`
	IsMeta    bool   `json:"isMeta"`
	Message   struct {
		Role    string          `json:"role"`
		Content json.RawMessage `json:"content"`
	} `json:"message"`
	Attachment struct {
		Type    string          `json:"type"`
		Content json.RawMessage `json:"content"`
	} `json:"attachment"`
}

type claudeBlock struct {
	Type      string          `json:"type"`
	Text      string          `json:"text"`
	Name      string          `json:"name"`
	ID        string          `json:"id"`
	Input     json.RawMessage `json:"input"`
	ToolUseID string          `json:"tool_use_id"`
	Content   json.RawMessage `json:"content"`
}

func ts(s string) time.Time {
	t, _ := time.Parse(time.RFC3339Nano, s)
	return t
}

// textOf flattens a content value that is a string or an array of {type:text,text} blocks.
func textOf(raw json.RawMessage) string {
	if len(raw) == 0 {
		return ""
	}
	var s string
	if json.Unmarshal(raw, &s) == nil {
		return s
	}
	var blocks []claudeBlock
	if json.Unmarshal(raw, &blocks) == nil {
		var parts []string
		for _, b := range blocks {
			if b.Text != "" {
				parts = append(parts, b.Text)
			}
		}
		return strings.Join(parts, "\n")
	}
	var strs []string
	if json.Unmarshal(raw, &strs) == nil {
		return strings.Join(strs, "\n")
	}
	return ""
}

func (ClaudeParser) Parse(line []byte) Record {
	var l claudeLine
	if json.Unmarshal(line, &l) != nil {
		return Record{}
	}
	at := ts(l.Timestamp)
	var out []Turn
	switch l.Type {
	case "user", "assistant":
		role := l.Message.Role
		if role == "" {
			role = l.Type
		}
		if l.IsMeta { // text Claude itself inserted (e.g. "Stop hook feedback:"), not something the user typed
			role = "system"
		}
		// plain string content
		var s string
		if json.Unmarshal(l.Message.Content, &s) == nil {
			if strings.TrimSpace(s) != "" {
				out = append(out, Turn{ID: l.UUID, TS: at, Role: role, Text: clip(s)})
			}
			break
		}
		var blocks []claudeBlock
		if json.Unmarshal(l.Message.Content, &blocks) != nil {
			break
		}
		for _, b := range blocks {
			switch b.Type {
			case "text":
				if strings.TrimSpace(b.Text) != "" {
					out = append(out, Turn{ID: l.UUID, TS: at, Role: role, Text: clip(b.Text)})
				}
			case "tool_use":
				out = append(out, Turn{ID: b.ID, TS: at, Role: "tool_call", Tool: b.Name, Text: clip(string(b.Input))})
			case "tool_result":
				out = append(out, Turn{ID: b.ToolUseID, TS: at, Role: "tool_result", Text: clip(textOf(b.Content))})
			}
		}
	case "attachment":
		// text a hook injected into the conversation (how mid-turn relay messages arrive)
		if l.Attachment.Type == "hook_additional_context" {
			if txt := textOf(l.Attachment.Content); txt != "" {
				out = append(out, Turn{ID: l.UUID, TS: at, Role: "system", Text: clip(txt)})
			}
		}
	}
	return Record{Turns: out}
}
