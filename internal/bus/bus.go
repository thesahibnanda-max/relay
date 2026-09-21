package bus

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/thesahibnanda-max/relay/internal/proto"
	"github.com/thesahibnanda-max/relay/internal/state"
)

// Tunables.
const (
	tick        = 50 * time.Millisecond
	typingQuiet = 1500 * time.Millisecond // a keystroke this recent means "user is typing"
	coolingMax  = 5 * time.Second         // give up waiting for the tool to react to an injection
	agingStep   = 5 * time.Minute         // waiting this long promotes a message one level
	seenMax     = 4096
)

// Injected message and report states (the values the daemon stores).
const (
	StateInjected     = "injected"
	StateAcknowledged = "acknowledged"
)

// BootstrapID marks the launch briefing, which is typed once like a message
// but is not a routed message (nothing is reported to the daemon for it).
const BootstrapID = "bootstrap"

// Env is the bus's view of the running agent. Implementations must not block
// for long; Inject may wait for a safe moment in the user's input stream.
type Env interface {
	Snapshot() state.Snapshot
	UserQuietFor(d time.Duration) bool
	DraftDirty() bool
	Inject(ctx context.Context, text string) error
	Interrupt() error
	// Report tells the daemon what happened to a message.
	Report(ctx context.Context, id, state string) error
}

type pending struct {
	proto.MessageView
	arrived     time.Time
	seq         int
	interrupted time.Time // when Esc was sent for it (zero: not yet)
}

// Bus holds the messages waiting for this agent's tool.
type Bus struct {
	env Env
	now func() time.Time

	mu      sync.Mutex
	queue   []*pending
	seen    map[string]struct{} // ids already handled or queued: redelivery is idempotent
	seenOrd []string
	acked   map[string]struct{} // ids already reported acknowledged (transcript sightings repeat)
	seq     int
	reports []report // outcomes the daemon has not yet acknowledged
	wake    chan struct{}

	// cooling: we typed a message at injectedAt and have not seen the tool
	// react (go busy) yet.
	injectedAt time.Time
	sawBusy    bool

	// OnDeliver, if set, is called after a message was typed into the tool.
	OnDeliver func(id string)
}

type report struct{ id, state string }

func New(env Env) *Bus {
	return &Bus{env: env, now: time.Now, seen: map[string]struct{}{}, acked: map[string]struct{}{}, wake: make(chan struct{}, 1)}
}

func (b *Bus) poke() {
	select {
	case b.wake <- struct{}{}:
	default:
	}
}

// Add queues a message pushed by the daemon. Redelivery of a message already
// queued or handled is ignored.
func (b *Bus) Add(m proto.MessageView) {
	b.mu.Lock()
	if _, dup := b.seen[m.ID]; dup {
		b.mu.Unlock()
		return
	}
	b.rememberLocked(m.ID)
	b.seq++
	b.queue = append(b.queue, &pending{MessageView: m, arrived: b.now(), seq: b.seq})
	b.mu.Unlock()
	b.poke()
}

// AddBootstrap queues a one-time briefing to be typed when the tool first
// becomes ready (used when the tool has no system-prompt flag we can use).
func (b *Bus) AddBootstrap(text string) {
	b.Add(proto.MessageView{ID: BootstrapID, From: "relay", Kind: "notify", Priority: P1, Body: text})
}

func (b *Bus) rememberLocked(id string) {
	b.seen[id] = struct{}{}
	b.seenOrd = append(b.seenOrd, id)
	if len(b.seenOrd) > seenMax {
		delete(b.seen, b.seenOrd[0])
		b.seenOrd = b.seenOrd[1:]
	}
}

// Pending returns how many messages are waiting.
func (b *Bus) Pending() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return len(b.queue)
}

// Inbox returns up to limit waiting messages, most urgent first. Unless peek
// is set they are consumed (marked acknowledged) so they are not typed into
// the terminal as well: this is the pull path used when the model asks.
func (b *Bus) Inbox(limit int, peek bool) []proto.MessageView {
	if limit <= 0 {
		limit = 20
	}
	b.mu.Lock()
	order := b.orderedLocked()
	var out []proto.MessageView
	taken := map[*pending]bool{}
	for _, p := range order {
		if len(out) >= limit {
			break
		}
		if p.ID == BootstrapID {
			continue
		}
		out = append(out, p.MessageView)
		taken[p] = true
	}
	if !peek && len(taken) > 0 {
		kept := b.queue[:0]
		for _, p := range b.queue {
			if taken[p] {
				b.reports = append(b.reports, report{p.ID, StateAcknowledged})
				continue
			}
			kept = append(kept, p)
		}
		b.queue = kept
	}
	b.mu.Unlock()
	if !peek {
		b.poke()
	}
	return out
}

// Ack reports that the model has handled a message (also drops it from the
// queue if it was still waiting).
func (b *Bus) Ack(id string) {
	b.mu.Lock()
	for i, p := range b.queue {
		if p.ID == id {
			b.queue = append(b.queue[:i], b.queue[i+1:]...)
			break
		}
	}
	b.reports = append(b.reports, report{id, StateAcknowledged})
	b.mu.Unlock()
	b.poke()
}

// effective is a message's priority after aging: waiting promotes it one
// level per agingStep, down to high (an old message never becomes an
// interrupt, and an interrupt never ages).
func (b *Bus) effective(p *pending) int {
	prio := p.Priority
	if prio <= P1 {
		return prio
	}
	prio -= int(b.now().Sub(p.arrived) / agingStep)
	if prio < P1 {
		prio = P1
	}
	return prio
}

// orderedLocked returns the queue most urgent first, first come first served
// within a level.
func (b *Bus) orderedLocked() []*pending {
	out := append([]*pending(nil), b.queue...)
	sort.SliceStable(out, func(i, j int) bool {
		ei, ej := b.effective(out[i]), b.effective(out[j])
		if ei != ej {
			return ei < ej
		}
		return out[i].seq < out[j].seq
	})
	return out
}

// Run schedules until ctx ends.
func (b *Bus) Run(ctx context.Context) {
	t := time.NewTicker(tick)
	defer t.Stop()
	for {
		b.step(ctx)
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-b.wake:
		}
	}
}

// step delivers at most one thing and flushes reports.
func (b *Bus) step(ctx context.Context) {
	b.flushReports(ctx)

	b.mu.Lock()
	if len(b.queue) == 0 {
		b.mu.Unlock()
		return
	}
	order := b.orderedLocked()
	b.mu.Unlock()

	snap := b.env.Snapshot()
	now := b.now()

	b.mu.Lock()
	// Has the tool reacted to what we last typed?
	if !b.injectedAt.IsZero() {
		if snap.State == state.Busy {
			b.sawBusy = true
		}
		if (b.sawBusy && snap.State == state.Idle) || now.Sub(b.injectedAt) > coolingMax {
			b.injectedAt, b.sawBusy = time.Time{}, false
		}
	}
	cooling := !b.injectedAt.IsZero()
	b.mu.Unlock()

	sit := Situation{
		State:      snap,
		UserTyping: !b.env.UserQuietFor(typingQuiet),
		DraftDirty: b.env.DraftDirty(),
		Cooling:    cooling,
	}
	head := order[0]
	eff := b.effective(head)
	sit.Interrupted = !head.interrupted.IsZero()
	for _, p := range order[1:] {
		if b.effective(p) < eff {
			sit.OthersWaiting = true // cannot happen while order is sorted; keeps the rule true to its table
		}
	}
	verdict, _ := Decide(sit, eff)

	switch verdict {
	case Interrupt:
		if err := b.env.Interrupt(); err == nil {
			b.mu.Lock()
			head.interrupted = now
			b.mu.Unlock()
		}
	case Inject:
		batch := []*pending{head}
		if eff >= P3 { // low-priority notes are digested into one turn
			for _, p := range order[1:] {
				if b.effective(p) >= P3 {
					batch = append(batch, p)
				}
			}
		}
		b.deliver(ctx, batch)
	}
}

func (b *Bus) deliver(ctx context.Context, batch []*pending) {
	texts := make([]string, len(batch))
	for i, p := range batch {
		texts[i] = Compose(p.MessageView)
	}
	if err := b.env.Inject(ctx, strings.Join(texts, "\n\n")); err != nil {
		return // context ended or the terminal is gone; the message stays queued
	}
	b.mu.Lock()
	gone := map[*pending]bool{}
	for _, p := range batch {
		gone[p] = true
		if p.ID != BootstrapID {
			b.reports = append(b.reports, report{p.ID, StateInjected})
		}
	}
	kept := b.queue[:0]
	for _, p := range b.queue {
		if !gone[p] {
			kept = append(kept, p)
		}
	}
	b.queue = kept
	b.injectedAt, b.sawBusy = b.now(), false
	b.mu.Unlock()
	if b.OnDeliver != nil {
		for _, p := range batch {
			b.OnDeliver(p.ID)
		}
	}
}

func (b *Bus) flushReports(ctx context.Context) {
	b.mu.Lock()
	rs := b.reports
	b.reports = nil
	b.mu.Unlock()
	for i, r := range rs {
		rctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err := b.env.Report(rctx, r.id, r.state)
		cancel()
		if err != nil {
			// The daemon is unreachable (it will be retried) or refused: keep the rest.
			b.mu.Lock()
			b.reports = append(append([]report(nil), rs[i:]...), b.reports...)
			b.mu.Unlock()
			return
		}
	}
}

// Compose renders one message as the text typed into the tool: an
// attribution header the model can rely on, the body, and (for messages that
// expect an answer) how to reply.
func Compose(m proto.MessageView) string {
	if m.ID == BootstrapID {
		return m.Body
	}
	from := m.From
	if m.FromRole != "" && m.FromRole != "agent" {
		from += " (" + m.FromRole + ")"
	}
	head := fmt.Sprintf("[relay | from %s | %s | %s | msg %s]", from, m.Kind, proto.PriorityName(m.Priority), m.ID)
	text := head + "\n" + Defang(strings.TrimSpace(m.Body))
	if (m.Kind == "task" || m.Kind == "question") && m.From != "user" && m.From != "relay" {
		text += fmt.Sprintf("\n[Answer with the relay_send tool: to=%q, reply_to=%q. Text you write in this terminal is not seen by %s.]", m.From, m.ID, m.From)
	}
	return text
}

// TakeForHook hands over, for delivery through a tool hook, the messages that
// may go now, removing them from the queue so the terminal path cannot also
// type them. At a tool boundary (all=false) that is what the table calls
// "P1: next safe point": high-priority messages, but never interrupts (those
// press Esc instead). When the tool is about to stop (all=true) everything
// waiting is handed over: continuing the turn beats typing a new prompt.
func (b *Bus) TakeForHook(all bool) []proto.MessageView {
	b.mu.Lock()
	var out []proto.MessageView
	taken := map[*pending]bool{}
	for _, p := range b.orderedLocked() {
		if p.ID == BootstrapID {
			continue
		}
		if !all && (p.Priority <= P0 || b.effective(p) > P1) {
			continue
		}
		out = append(out, p.MessageView)
		taken[p] = true
	}
	if len(taken) > 0 {
		kept := b.queue[:0]
		for _, p := range b.queue {
			if taken[p] {
				b.reports = append(b.reports, report{p.ID, StateInjected})
				continue
			}
			kept = append(kept, p)
		}
		b.queue = kept
	}
	b.mu.Unlock()
	if len(out) > 0 {
		b.poke()
	}
	return out
}

// Confirm reports messages as acknowledged because their ids were seen in the
// tool's own transcript (proof the model received them). Each id is reported once.
func (b *Bus) Confirm(msgIDs []string) {
	b.mu.Lock()
	for _, id := range msgIDs {
		if _, done := b.acked[id]; done {
			continue
		}
		b.acked[id] = struct{}{}
		b.reports = append(b.reports, report{id, StateAcknowledged})
	}
	b.mu.Unlock()
	if len(msgIDs) > 0 {
		b.poke()
	}
}

// ComposeHook renders messages for delivery inside a hook's output.
func ComposeHook(msgs []proto.MessageView, midTurn bool) string {
	var parts []string
	for _, m := range msgs {
		parts = append(parts, Compose(m))
	}
	intro := "[relay] Message(s) from your teammates arrived"
	if midTurn {
		intro += " while you were working. Finish or pause your current step as you judge best, then handle them:"
	} else {
		intro += " just as you were finishing. Handle them before you stop:"
	}
	return intro + "\n\n" + strings.Join(parts, "\n\n")
}

// Defang stops a message body from forging a Relay header. Headers are how the
// receiving model tells who wrote what (and how delivery is confirmed), so
// text that merely looks like one is altered: "[relay |" becomes "[relay ¦".
func Defang(body string) string { return strings.ReplaceAll(body, "[relay |", "[relay ¦") }
