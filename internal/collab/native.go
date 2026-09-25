package collab

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/thesahibnanda-max/relay/internal/bus"
	"github.com/thesahibnanda-max/relay/internal/hooks"
	"github.com/thesahibnanda-max/relay/internal/proto"
	"github.com/thesahibnanda-max/relay/internal/state"
	"github.com/thesahibnanda-max/relay/internal/transcript"
)

// Native signals: what the tools themselves tell us (Claude hooks, transcript
// files) is authoritative and beats guessing from the screen.

const (
	busyTTL        = 5 * time.Minute // a "busy" nobody refreshes or ends does not last forever
	busyQuietStale = 4 * time.Second // ...nor does one while the terminal has gone quiet (see state.Signal)
	maxStopBlocks  = 5               // consecutive Stop-continues before we let the tool stop
)

type native struct {
	mu          sync.Mutex
	ctx         context.Context
	trPath      string
	trCancel    context.CancelFunc
	stopBlocks  int
	started     time.Time
	cwd         string
	upload      bool
	codexOnce   sync.Once
	copilotOnce sync.Once
}

// SetUploadTurns controls whether conversation turns are sent to the daemon
// (off with --record=off: then relay_get_context has nothing to show).
func (s *Session) SetUploadTurns(on bool) {
	s.nat.mu.Lock()
	s.nat.upload = on
	s.nat.mu.Unlock()
}

// startNative begins the tool-specific watchers once the tool is running.
func (s *Session) startNative(ctx context.Context) {
	s.nat.mu.Lock()
	s.nat.ctx = ctx
	s.nat.started = time.Now()
	s.nat.cwd, _ = os.Getwd()
	s.nat.mu.Unlock()
	if s.tool == "codex" && s.lk != nil {
		s.nat.codexOnce.Do(func() { go s.followCodex(ctx) })
	}
	if s.tool == "copilot" && s.lk != nil {
		s.nat.copilotOnce.Do(func() { go s.followCopilot(ctx) })
	}
}

// followCodex waits for this agent's rollout file to appear (Codex writes it at
// the first message) and follows it.
func (s *Session) followCodex(ctx context.Context) {
	q := transcript.CodexQuery{Cwd: s.nat.cwd, Since: s.nat.started, Name: s.ident.Agent.Name, Session: s.ident.Session.ID}
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	for {
		if path := transcript.LocateCodex(q); path != "" {
			s.setTranscript(path)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// followCopilot waits for this agent's events.jsonl to appear (Copilot writes
// it once the session starts) and follows it.
func (s *Session) followCopilot(ctx context.Context) {
	q := transcript.CopilotQuery{Cwd: s.nat.cwd, Since: s.nat.started, Name: s.ident.Agent.Name, Session: s.ident.Session.ID}
	t := time.NewTicker(500 * time.Millisecond)
	defer t.Stop()
	for {
		if path := transcript.LocateCopilot(q); path != "" {
			s.setTranscript(path)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}

// setTranscript follows a (new) transcript file, dropping the previous one.
func (s *Session) setTranscript(path string) {
	parser := transcript.ForTool(s.tool)
	s.nat.mu.Lock()
	defer s.nat.mu.Unlock()
	if parser == nil || path == "" || path == s.nat.trPath || s.nat.ctx == nil {
		return
	}
	if s.nat.trCancel != nil {
		s.nat.trCancel()
	}
	ctx, cancel := context.WithCancel(s.nat.ctx)
	s.nat.trPath, s.nat.trCancel = path, cancel
	tl := &transcript.Tailer{Path: path, Parse: parser, Fn: s.onRecord}
	go tl.Run(ctx)
}

// onRecord handles one transcript record: upload turns, confirm delivery, and
// (for Codex and Copilot, whose transcripts mark turn boundaries) update the
// tool's state.
func (s *Session) onRecord(rec transcript.Record) {
	s.nat.mu.Lock()
	upload := s.nat.upload
	s.nat.mu.Unlock()
	for _, t := range rec.Turns {
		if upload && s.lk != nil {
			at := t.TS
			if at.IsZero() {
				at = time.Now()
			}
			meta, _ := json.Marshal(t)
			s.lk.Send(proto.Event{T: at, Type: proto.EventTurn, Meta: meta})
		}
		if t.Role == "user" || t.Role == "system" {
			s.bus.Confirm(transcript.MsgIDs(t.Text)) // the model has demonstrably received these
		}
	}
	h := s.handle.Load()
	if h == nil || rec.Signal == "" {
		return
	}
	switch rec.Signal {
	case transcript.SigTaskStarted:
		h.ClearSignal("rollout-plan")
		h.Signal(state.Signal{Source: "rollout", State: state.Busy, Conf: state.High, TTL: 30 * time.Minute, QuietStale: busyQuietStale})
	case transcript.SigTaskComplete, transcript.SigAborted:
		h.Signal(state.Signal{Source: "rollout", State: state.Idle, Conf: state.High})
	case transcript.SigPlanPending:
		// "Implement this plan?" is waiting for the user: a message typed now
		// would answer it (Codex ran queued messages in plan mode here). Hold
		// until the next turn starts.
		h.Signal(state.Signal{Source: "rollout", State: state.Idle, Conf: state.High})
		h.Signal(state.Signal{Source: "rollout-plan", State: state.Dialog, Conf: state.High})
	}
}

// HandleHook serves one Claude hook event and returns the hook's stdout ("" = nothing to say).
func (s *Session) HandleHook(payload json.RawMessage) string {
	var in hooks.Input
	if json.Unmarshal(payload, &in) != nil {
		return ""
	}
	h := s.handle.Load()
	sig := func(st state.State, ttl time.Duration) {
		if h == nil {
			return
		}
		g := state.Signal{Source: "hook", State: st, Conf: state.High, TTL: ttl}
		if st == state.Busy {
			g.QuietStale = busyQuietStale
		}
		h.Signal(g)
	}
	if in.TranscriptPath != "" {
		s.setTranscript(in.TranscriptPath)
	}

	switch in.HookEventName {
	case "SessionStart":
		sig(state.Idle, 0)
	case "UserPromptSubmit":
		s.nat.mu.Lock()
		s.nat.stopBlocks = 0
		s.nat.mu.Unlock()
		sig(state.Busy, busyTTL)
	case "Notification":
		if in.NotificationType == "permission_prompt" || strings.Contains(strings.ToLower(in.Message), "permission") {
			sig(state.Dialog, 10*time.Minute) // cleared by the next tool result, prompt or stop
		}
	case "PostToolUse":
		sig(state.Busy, busyTTL) // also ends a permission dialog: the tool ran
		if msgs := s.bus.TakeForHook(false); len(msgs) > 0 {
			return hooks.AdditionalContext("PostToolUse", bus.ComposeHook(msgs, true))
		}
	case "Stop":
		s.nat.mu.Lock()
		blocked := s.nat.stopBlocks
		s.nat.mu.Unlock()
		if blocked < maxStopBlocks {
			if msgs := s.bus.TakeForHook(true); len(msgs) > 0 {
				s.nat.mu.Lock()
				s.nat.stopBlocks++
				s.nat.mu.Unlock()
				sig(state.Busy, busyTTL) // the tool carries on working: it is not idle
				return hooks.Block(bus.ComposeHook(msgs, false))
			}
		}
		sig(state.Idle, 0)
	}
	return ""
}
