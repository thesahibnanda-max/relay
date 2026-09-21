// Package transcript reads the tools' own conversation logs (Claude's
// ~/.claude/projects/**.jsonl, Codex's ~/.codex/sessions/**/rollout-*.jsonl).
// They are the ground truth for what happened: precise turn boundaries,
// tool calls, and proof that a message reached the model. Relay only ever
// READS them.
package transcript

import (
	"bytes"
	"context"
	"io"
	"os"
	"regexp"
	"time"
)

// Turn is one normalised event of a conversation.
type Turn struct {
	ID   string    `json:"id,omitempty"` // the tool's own id for it, when there is one
	TS   time.Time `json:"ts"`
	Role string    `json:"role"` // user | assistant | tool_call | tool_result | system
	Text string    `json:"text"`
	Tool string    `json:"tool,omitempty"` // for tool_call / tool_result
}

// Signals a transcript can raise about the tool's state.
const (
	SigTaskStarted  = "task_started"
	SigTaskComplete = "task_complete"
	SigPlanPending  = "plan_pending" // a plan was proposed and the user has not answered
	SigAborted      = "aborted"
)

// Record is what one transcript line means to Relay.
type Record struct {
	Turns  []Turn
	Signal string
}

// Parser turns a line into a Record. Lines it does not understand yield an empty Record.
type Parser interface {
	Parse(line []byte) Record
}

// MaxTurnText bounds one stored turn.
const MaxTurnText = 8 << 10

func clip(s string) string {
	if len(s) <= MaxTurnText {
		return s
	}
	cut := MaxTurnText
	for cut > 0 && s[cut]&0xC0 == 0x80 { // do not split a UTF-8 character
		cut--
	}
	return s[:cut] + "…[truncated]"
}

// ForTool returns the parser for a tool name ("claude", "codex").
func ForTool(tool string) Parser {
	switch tool {
	case "claude":
		return ClaudeParser{}
	case "codex":
		return CodexParser{}
	}
	return nil
}

// MsgIDs extracts the relay message ids ("msg <ULID>]") in text: seeing one in
// a transcript proves the message reached the model.
var msgIDRe = regexp.MustCompile(`(?m)^\[relay \| from [^\n\]]* \| msg ([0-9A-HJKMNP-TV-Z]{26})\]$`)

func MsgIDs(text string) []string {
	var out []string
	for _, m := range msgIDRe.FindAllStringSubmatch(text, -1) {
		out = append(out, m[1])
	}
	return out
}

// Tailer follows a transcript file, calling fn for every record as lines
// complete. A file that does not exist yet is waited for. On the first read a
// large existing file is entered near its end (recent history is what matters).
type Tailer struct {
	Path  string
	Parse Parser
	Fn    func(Record)
	Every time.Duration // poll interval (default 250 ms)
	// Backlog is how much of an existing file to read on start (default 2 MiB).
	Backlog int64
}

func (t *Tailer) Run(ctx context.Context) {
	every := t.Every
	if every == 0 {
		every = 250 * time.Millisecond
	}
	backlog := t.Backlog
	if backlog == 0 {
		backlog = 2 << 20
	}
	var off int64
	first := true
	var partial []byte
	for {
		if f, err := os.Open(t.Path); err == nil {
			st, _ := f.Stat()
			if st != nil {
				if first {
					if st.Size() > backlog {
						off = st.Size() - backlog
						partial = nil
						first = false
						// drop the (probably partial) first line after the cut
						f.Seek(off, io.SeekStart)
						buf := make([]byte, 1)
						for {
							n, err := f.Read(buf)
							off += int64(n)
							if err != nil || buf[0] == '\n' {
								break
							}
						}
					}
					first = false
				}
				if st.Size() < off { // truncated or replaced
					off, partial = 0, nil
				}
				if st.Size() > off {
					f.Seek(off, io.SeekStart)
					data, _ := io.ReadAll(io.LimitReader(f, 4<<20))
					off += int64(len(data))
					data = append(partial, data...)
					partial = nil
					for len(data) > 0 {
						i := bytes.IndexByte(data, '\n')
						if i < 0 {
							partial = append([]byte(nil), data...)
							break
						}
						if line := bytes.TrimSpace(data[:i]); len(line) > 0 {
							if rec := t.Parse.Parse(line); len(rec.Turns) > 0 || rec.Signal != "" {
								t.Fn(rec)
							}
						}
						data = data[i+1:]
					}
				}
			}
			f.Close()
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(every):
		}
	}
}
