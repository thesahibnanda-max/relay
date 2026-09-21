package transcript

import (
	"encoding/json"
	"strings"
	"time"
)

// CodexParser reads Codex's rollout JSONL. Only `event_msg` records are used:
// task_started / task_complete give exact turn boundaries, and item_completed
// carries each finished item once (the parallel `response_item` records repeat it).
type CodexParser struct{}

type codexLine struct {
	Timestamp string          `json:"timestamp"`
	Type      string          `json:"type"`
	Payload   json.RawMessage `json:"payload"`
}

type codexEvent struct {
	Type             string          `json:"type"`
	LastAgentMessage string          `json:"last_agent_message"`
	Item             json.RawMessage `json:"item"`
}

type codexItem struct {
	Type    string `json:"type"`
	ID      string `json:"id"`
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
	Server    string          `json:"server"`
	Tool      string          `json:"tool"`
	Arguments json.RawMessage `json:"arguments"`
	Result    struct {
		Content []struct {
			Text string `json:"text"`
		} `json:"content"`
	} `json:"result"`
	Command          json.RawMessage `json:"command"`
	AggregatedOutput string          `json:"aggregated_output"`
	Stdout           string          `json:"stdout"`
}

func (c codexItem) text() string {
	var parts []string
	for _, p := range c.Content {
		if p.Text != "" {
			parts = append(parts, p.Text)
		}
	}
	return strings.Join(parts, "\n")
}

func (CodexParser) Parse(line []byte) Record {
	var l codexLine
	if json.Unmarshal(line, &l) != nil || l.Type != "event_msg" {
		return Record{}
	}
	var ev codexEvent
	if json.Unmarshal(l.Payload, &ev) != nil {
		return Record{}
	}
	at, _ := time.Parse(time.RFC3339Nano, l.Timestamp)
	switch ev.Type {
	case "task_started":
		return Record{Signal: SigTaskStarted}
	case "task_complete":
		if strings.Contains(ev.LastAgentMessage, "<proposed_plan>") {
			return Record{Signal: SigPlanPending}
		}
		return Record{Signal: SigTaskComplete}
	case "turn_aborted":
		return Record{Signal: SigAborted}
	case "item_completed":
		var it codexItem
		if json.Unmarshal(ev.Item, &it) != nil {
			return Record{}
		}
		switch it.Type {
		case "UserMessage":
			return Record{Turns: []Turn{{ID: it.ID, TS: at, Role: "user", Text: clip(it.text())}}}
		case "AgentMessage":
			return Record{Turns: []Turn{{ID: it.ID, TS: at, Role: "assistant", Text: clip(it.text())}}}
		case "McpToolCall":
			var res []string
			for _, c := range it.Result.Content {
				res = append(res, c.Text)
			}
			name := it.Server + "." + it.Tool
			return Record{Turns: []Turn{
				{ID: it.ID, TS: at, Role: "tool_call", Tool: name, Text: clip(string(it.Arguments))},
				{ID: it.ID, TS: at, Role: "tool_result", Tool: name, Text: clip(strings.Join(res, "\n"))},
			}}
		case "CommandExecution":
			var argv []string
			_ = json.Unmarshal(it.Command, &argv)
			cmd := strings.Join(argv, " ")
			if cmd == "" {
				cmd = string(it.Command)
			}
			out := it.AggregatedOutput
			if out == "" {
				out = it.Stdout
			}
			return Record{Turns: []Turn{
				{ID: it.ID, TS: at, Role: "tool_call", Tool: "shell", Text: clip(cmd)},
				{ID: it.ID, TS: at, Role: "tool_result", Tool: "shell", Text: clip(out)},
			}}
		}
	}
	return Record{}
}
