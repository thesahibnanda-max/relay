package bus

import (
	"context"
	"fmt"
	"regexp"
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
	inflight    bool      // being typed right now (guarded by Bus.mu)
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
	// ownTurn: the tool's current (or imminent) turn was started by what we
	// last typed; ownBusy once it was seen busy; ownSince when we typed it.
	ownTurn  bool
	ownBusy  bool
	ownSince time.Time

	// OnDeliver, if set, is called after a message was typed into the tool.
	OnDeliver func(id string)
}

type report struct{ id, state string }

func New(env Env) *Bus {
	return &Bus{env: env, now: time.Now, seen: map[string]struct{}{}, acked: map[string]struct{}{}, wake: make(chan struct{}, 1)}
}

// Poke makes the scheduler look again now (e.g. a gate outside the bus opened).
func (b *Bus) Poke() { b.poke() }

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
	// Not de-duplicated like a message: a tool can need briefing again (agy
	// after /new). One waiting briefing is enough: a newer one replaces it.
	b.mu.Lock()
	for _, p := range b.queue {
		if p.ID == BootstrapID && !p.inflight {
			p.Body = text
			b.mu.Unlock()
			return
		}
	}
	delete(b.seen, BootstrapID)
	b.mu.Unlock()
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

func (b *Bus) queuedLocked(p *pending) bool {
	for _, q := range b.queue {
		if q == p {
			return true
		}
	}
	return false
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
		if p.ID == BootstrapID || p.inflight {
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
	// With nothing queued there is nothing to decide - but the end of a turn
	// we started must still be seen, or a later turn would pass for ours.
	if len(b.queue) == 0 && !b.ownTurn && b.injectedAt.IsZero() {
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
	// Is the running turn one our own injection started? Until it ends.
	if b.ownTurn {
		if snap.State == state.Busy {
			b.ownBusy = true
		}
		if (b.ownBusy && snap.State == state.Idle) || (!b.ownBusy && now.Sub(b.ownSince) > coolingMax) {
			b.ownTurn, b.ownBusy = false, false
		}
	}
	cooling := !b.injectedAt.IsZero()
	ownTurn := b.ownTurn
	b.mu.Unlock()
	if len(order) == 0 {
		return
	}

	sit := Situation{
		State:      snap,
		UserTyping: !b.env.UserQuietFor(typingQuiet),
		DraftDirty: b.env.DraftDirty(),
		Cooling:    cooling,
		OwnTurn:    ownTurn,
	}
	head := order[0]
	eff := b.effective(head)
	b.mu.Lock()
	sit.Interrupted = !head.interrupted.IsZero()
	b.mu.Unlock()
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
	// The decision was taken without the lock: a hook or the model's
	// inbox/ack may have taken some of these meanwhile. Only what is still
	// queued is typed, and it is marked in flight so nothing else hands it
	// out while the (possibly slow) injection runs.
	b.mu.Lock()
	var live []*pending
	for _, p := range batch {
		if !p.inflight && b.queuedLocked(p) {
			p.inflight = true
			live = append(live, p)
		}
	}
	b.mu.Unlock()
	if len(live) == 0 {
		return
	}
	batch = live
	texts := make([]string, len(batch))
	for i, p := range batch {
		texts[i] = Compose(p.MessageView)
	}
	err := b.env.Inject(ctx, strings.Join(texts, "\n\n"))
	b.mu.Lock()
	for _, p := range batch {
		p.inflight = false
	}
	if err != nil {
		b.mu.Unlock()
		return // context ended or the terminal is gone; the message stays queued
	}
	gone := map[*pending]bool{}
	for _, p := range batch {
		if !b.queuedLocked(p) {
			continue // acknowledged by the model while it was being typed
		}
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
	b.ownTurn, b.ownBusy, b.ownSince = true, false, b.injectedAt
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
	from := headerField(m.From)
	if role := headerField(m.FromRole); role != "" && role != "agent" {
		from += " (" + role + ")"
	}
	head := fmt.Sprintf("[relay | from %s | %s | %s | msg %s]", from, headerField(m.Kind), proto.PriorityName(m.Priority), headerField(m.ID))
	text := head + "\n" + Defang(strings.TrimSpace(CleanText(m.Body)))
	if (m.Kind == "task" || m.Kind == "question") && m.From != "user" && m.From != "relay" {
		text += fmt.Sprintf("\n%s to=%q, reply_to=%q. Text you write in this terminal is not seen by %s.]", answerHint, headerField(m.From), headerField(m.ID), headerField(m.From))
	}
	return text
}

const answerHint = "[Answer with the relay_send tool:"

// headerField keeps a header field to a plain character set, whatever a peer
// sent (a global session's server does not police names or kinds): it can
// never close the header, open a new line or smuggle a separator in.
func headerField(s string) string {
	var b strings.Builder
	for _, r := range s {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', strings.ContainsRune("._:@-", r):
			b.WriteRune(r)
		default:
			b.WriteRune('_')
		}
		if b.Len() >= 64 {
			break
		}
	}
	return b.String()
}

// CleanText removes what a message body must never carry into a terminal or
// a model's input: C0/C1 controls (keeping newline and tab; CRs become
// newlines) and the Unicode bidi overrides that make text read differently
// from what it is. It runs before Defang, so nothing Defang looks for can be
// assembled afterwards by a later stripping step.
func CleanText(s string) string {
	s = strings.ReplaceAll(s, "\r\n", "\n")
	var b strings.Builder
	for _, r := range s {
		switch {
		case r == '\r':
			b.WriteByte('\n')
		case r == '\n' || r == '\t':
			b.WriteRune(r)
		case r < 0x20 || r == 0x7f || (r >= 0x80 && r <= 0x9f):
		case r >= 0x202a && r <= 0x202e, r >= 0x2066 && r <= 0x2069, r == 0x200e, r == 0x200f, r == 0x061c:
		default:
			b.WriteRune(r)
		}
	}
	return b.String()
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
		if p.ID == BootstrapID || p.inflight {
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
// It also catches look-alikes (spacing, letter case) and the answer hint
// Compose appends, which a body could otherwise fake.
func Defang(body string) string {
	body = forgedHeader.ReplaceAllString(body, "[relay ¦")
	return forgedHint.ReplaceAllString(body, "(Answer with the relay_send tool:")
}

var (
	forgedHeader = regexp.MustCompile(`(?i)\[\s*relay\s*\|`)
	forgedHint   = regexp.MustCompile(`(?i)\[\s*answer\s+with\s+the\s+relay_send\s+tool\s*:`)
)
