package collab

import (
	"context"
	"encoding/json"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/thesahibnanda-max/relay/internal/adaptor/launch"
	"github.com/thesahibnanda-max/relay/internal/bus"
	"github.com/thesahibnanda-max/relay/internal/hooks"
	"github.com/thesahibnanda-max/relay/internal/proto"
	"github.com/thesahibnanda-max/relay/internal/state"
	"github.com/thesahibnanda-max/relay/internal/transcript"
)

// Native signals: what the tools themselves tell us (Claude hooks, transcript
// files) is authoritative and beats guessing from the screen.

const (
	busyTTL        = 5 * time.Minute  // a "busy" nobody refreshes or ends does not last forever
	busyQuietStale = 4 * time.Second  // ...nor does one while the terminal has gone quiet (see state.Signal)
	maxStopBlocks  = 5                // consecutive Stop-continues before we let the tool stop
	agyStateTTL    = 10 * time.Second // agy's re-sent database state (see transcript.AgyDBTailer)
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
	agyOnce     sync.Once
	toolLog     string          // the log the tool writes for this launch (agy)
	briefing    string          // typed into a conversation that lacks it (agy)
	briefed     map[string]bool // conversations known to hold the briefing (agy)
}

// SetToolLog names the diagnostic log the tool writes for this launch.
func (s *Session) SetToolLog(path string) {
	s.nat.mu.Lock()
	s.nat.toolLog = path
	s.nat.mu.Unlock()
}

// SetBriefing records the launch briefing, for tools whose conversation can
// be replaced mid-session (agy's /new).
func (s *Session) SetBriefing(b string) {
	s.nat.mu.Lock()
	s.nat.briefing = strings.TrimSpace(b)
	if s.nat.briefed == nil {
		s.nat.briefed = map[string]bool{}
	}
	s.nat.mu.Unlock()
}

// briefingMark is what identifies this launch's briefing in a conversation:
// its first sentence, up to the session id.
func (s *Session) briefingMark() string {
	b := s.nat.briefing
	if i := strings.Index(b, ")"); i > 0 {
		return b[:i+1]
	}
	return b
}

// noteBriefed records that conversation path holds the briefing, if
// userText (a user turn in it) contains it. A briefing still waiting to be
// typed is then withdrawn: this conversation already has one (the -i
// briefing of a conversation agy resumed, say).
func (s *Session) noteBriefed(path, userText string) {
	s.nat.mu.Lock()
	m := s.briefingMark()
	hit := m != "" && strings.Contains(userText, m)
	if hit {
		s.nat.briefed[path] = true
	}
	s.nat.mu.Unlock()
	if hit {
		s.bus.WithdrawBootstrap()
	}
}

func (s *Session) briefedIn(path string) bool {
	s.nat.mu.Lock()
	defer s.nat.mu.Unlock()
	return s.nat.briefed[path]
}

// openGateIfLeftBriefed opens the startup gate when agy moves on from a
// conversation that holds the briefing: startup is over even if that turn
// never finished where relay could see it (/new while it ran). Conversations
// agy made for prompts that raced its sign-in hold no briefing and keep the
// gate shut.
func (s *Session) openGateIfLeftBriefed(ctx context.Context, next string) {
	if !s.gateClosed.Load() {
		return
	}
	s.nat.mu.Lock()
	prev, m := s.nat.trPath, s.briefingMark()
	known := s.nat.briefed[prev]
	s.nat.mu.Unlock()
	if prev == "" || prev == next || m == "" {
		return
	}
	if !known {
		for _, t := range transcript.AgyUserTurns(ctx, prev) {
			if strings.Contains(t, m) {
				known = true
				break
			}
		}
	}
	if known {
		s.nat.mu.Lock()
		s.nat.briefed[prev] = true
		s.nat.mu.Unlock()
		s.openGate()
	}
}

// briefIfNeeded types the briefing into conversation path, once, if it is
// still the live one and went idle without it - after /new, or a
// conversation agy switched to on its own.
func (s *Session) briefIfNeeded(path string) {
	s.nat.mu.Lock()
	b := s.nat.briefing
	need := b != "" && path != "" && path == s.nat.trPath && !s.nat.briefed[path]
	if need {
		s.nat.briefed[path] = true // once per conversation, whatever happens next
	}
	s.nat.mu.Unlock()
	if need {
		s.bus.AddBootstrap(b + "\n\n" + launch.BriefingTurnTail)
	}
}

// onAgyRecord handles a record from agy conversation path, the live one.
func (s *Session) onAgyRecord(path string, rec transcript.Record) {
	for _, t := range rec.Turns {
		if t.Role == "user" {
			s.noteBriefed(path, t.Text)
		}
	}
	s.onRecord(rec)
	if rec.Signal != transcript.SigAgyIdle || s.handle.Load() == nil {
		return
	}
	if s.gateClosed.Load() {
		// Only this launch's briefing turn ending opens the gate: a resumed
		// conversation's history, a conversation agy made for a prompt that
		// raced its sign-in, or one just created and still empty going idle
		// is not that.
		if !s.briefedIn(path) {
			return
		}
		s.openGate()
	}
	s.briefIfNeeded(path)
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
	if s.tool == "agy" && s.lk != nil {
		s.nat.agyOnce.Do(func() { go s.followAgy(ctx) })
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

// followAgy follows the log relay asked agy to write for this launch (see
// the agy adaptor's Prepare): it names the conversation this process is on,
// authoritatively - at start, after /new and on resume - so its database is
// followed without guessing from directories or timing. A switch to another
// conversation after the first means the model starts without the relay
// briefing, so it is typed again.
func (s *Session) followAgy(ctx context.Context) {
	s.nat.mu.Lock()
	logPath := s.nat.toolLog
	s.nat.mu.Unlock()
	if logPath == "" {
		return
	}
	subagents := map[string]bool{} // goroutines starting a subagent's conversation
	f := &transcript.AgyLogFollower{Path: logPath, Fn: func(ev transcript.AgyLogEvent) {
		if ev.HasInput {
			s.Accepted(ev.Input)
			return
		}
		if ev.SubagentStart {
			subagents[ev.Goroutine] = true
			return
		}
		if ev.Conversation == "" {
			return
		}
		if ev.Created && subagents[ev.Goroutine] {
			delete(subagents, ev.Goroutine)
			return // a subagent's conversation, not the one on screen
		}
		// The newest conversation is the live one (agy may create several
		// while signing in, one per prompt that raced it; /new creates one).
		// Whether it needs the briefing is decided from its own contents,
		// once it is idle (see onRecord).
		path := transcript.AgyConversationDB(ev.Conversation)
		s.openGateIfLeftBriefed(ctx, path)
		s.setAgyTranscript(path, !ev.Created)
	}}
	f.Run(ctx)
}

// setAgyTranscript follows a (new) agy conversation database, dropping the
// previous one; history is skipped for a resumed conversation. It reports
// whether it switched.
func (s *Session) setAgyTranscript(path string, resumed bool) bool {
	s.nat.mu.Lock()
	defer s.nat.mu.Unlock()
	if path == "" || path == s.nat.trPath || s.nat.ctx == nil {
		return false
	}
	if s.nat.trCancel != nil {
		s.nat.trCancel()
	}
	ctx, cancel := context.WithCancel(s.nat.ctx)
	s.nat.trPath, s.nat.trCancel = path, cancel
	// A briefing still waiting was meant for the conversation just left.
	defer s.bus.WithdrawBootstrap()
	fn := func(r transcript.Record) {
		s.nat.mu.Lock()
		live := s.nat.trPath == path
		s.nat.mu.Unlock()
		if live { // a record from a conversation already switched away from is stale
			s.onAgyRecord(path, r)
		}
	}
	tl := &transcript.AgyDBTailer{Path: path, Fn: fn, SkipExisting: resumed}
	go tl.Run(ctx)
	return true
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
	case transcript.SigAgyBusy:
		// agy's database is authoritative and re-sent every few seconds, so
		// a short TTL (and no quiet-staleness: a silent tool run is still a
		// turn in progress) keeps it exact without ever outliving the tailer.
		h.Signal(state.Signal{Source: "agydb", State: state.Busy, Conf: state.High, TTL: agyStateTTL})
	case transcript.SigAgyIdle:
		h.Signal(state.Signal{Source: "agydb", State: state.Idle, Conf: state.High, TTL: agyStateTTL})
	case transcript.SigAgyDialog:
		h.Signal(state.Signal{Source: "agydb-dialog", State: state.Dialog, Conf: state.High, TTL: agyStateTTL})
	case transcript.SigAgyDialogClear:
		h.ClearSignal("agydb-dialog")
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
