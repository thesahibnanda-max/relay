package session

import (
	"context"
	"errors"
	"fmt"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	mongodriver "go.mongodb.org/mongo-driver/v2/mongo"

	"github.com/thesahibnanda-max/relay/server/package/config"
	"github.com/thesahibnanda-max/relay/server/package/database/mongodb"
	"github.com/thesahibnanda-max/relay/server/package/database/postgres"
	"github.com/thesahibnanda-max/relay/server/package/database/repository"
	"github.com/thesahibnanda-max/relay/server/package/sharding"
)

// The fakes below are in-memory stand-ins for the three repository
// interfaces and the sharding selector, so service's substantial Phase 1
// logic (Wait's poll/timeout, Send's validation/defaulting, resume's
// token/liveness checks) can be unit tested with no real Postgres/Mongo -
// there were previously zero non-integration tests for this package.

const testShardURL = "mongodb://fake-shard"

type fakeShards struct{}

func (fakeShards) ShardURLFor(ctx context.Context, sessionID string) (string, error) {
	return testShardURL, nil
}

var _ sharding.Interface = fakeShards{}

type fakeSessions struct {
	mu       sync.Mutex
	sessions map[string]mongodb.Session
}

func newFakeSessions() *fakeSessions { return &fakeSessions{sessions: map[string]mongodb.Session{}} }

func (f *fakeSessions) EnsureCollection(ctx context.Context, mongoURL string) error { return nil }

func (f *fakeSessions) Create(ctx context.Context, mongoURL string, s mongodb.Session) (mongodb.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.sessions[s.ID] = s
	return s, nil
}

func (f *fakeSessions) Get(ctx context.Context, mongoURL, sessionID string) (mongodb.Session, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	s, ok := f.sessions[sessionID]
	return s, ok, nil
}

func (f *fakeSessions) Delete(ctx context.Context, mongoURL, sessionID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.sessions, sessionID)
	return nil
}

func (f *fakeSessions) ListOlderThan(ctx context.Context, mongoURL string, cutoff time.Time) ([]mongodb.Session, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []mongodb.Session
	for _, s := range f.sessions {
		if s.UpdatedAt.Before(cutoff) {
			out = append(out, s)
		}
	}
	return out, nil
}

var _ repository.SessionRepository = (*fakeSessions)(nil)

type fakeAgents struct {
	mu     sync.Mutex
	agents map[string]mongodb.Agent
}

func newFakeAgents() *fakeAgents { return &fakeAgents{agents: map[string]mongodb.Agent{}} }

func (f *fakeAgents) EnsureCollection(ctx context.Context, mongoURL string) error { return nil }

func (f *fakeAgents) Create(ctx context.Context, mongoURL string, a mongodb.Agent) (mongodb.Agent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	now := time.Now().UTC()
	a.CreatedAt, a.UpdatedAt = now, now
	f.agents[a.ID] = a
	return a, nil
}

func (f *fakeAgents) Get(ctx context.Context, mongoURL, agentID string) (mongodb.Agent, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.agents[agentID]
	return a, ok, nil
}

func (f *fakeAgents) GetByName(ctx context.Context, mongoURL, sessionID, name string) (mongodb.Agent, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	for _, a := range f.agents {
		if a.SessionID == sessionID && a.Name == name {
			return a, true, nil
		}
	}
	return mongodb.Agent{}, false, nil
}

func (f *fakeAgents) ListBySession(ctx context.Context, mongoURL, sessionID string) ([]mongodb.Agent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []mongodb.Agent
	for _, a := range f.agents {
		if a.SessionID == sessionID {
			out = append(out, a)
		}
	}
	return out, nil
}

func (f *fakeAgents) SetStatus(ctx context.Context, mongoURL, agentID, status string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.agents[agentID]
	if !ok {
		return nil
	}
	a.Status = status
	a.UpdatedAt = time.Now().UTC()
	f.agents[agentID] = a
	return nil
}

func (f *fakeAgents) UpdatePolicy(ctx context.Context, mongoURL, agentID, status string, approveInbound, canInterrupt, canBroadcast bool) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	a, ok := f.agents[agentID]
	if !ok {
		return nil
	}
	a.Status, a.ApproveInbound, a.CanInterrupt, a.CanBroadcast = status, approveInbound, canInterrupt, canBroadcast
	a.UpdatedAt = time.Now().UTC()
	f.agents[agentID] = a
	return nil
}

func (f *fakeAgents) MarkAllDisconnected(ctx context.Context, mongoURL string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	for id, a := range f.agents {
		if a.Status == "connected" {
			a.Status = "disconnected"
			f.agents[id] = a
		}
	}
	return nil
}

func (f *fakeAgents) ReapGone(ctx context.Context, mongoURL string, cutoff time.Time) ([]mongodb.Agent, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var gone []mongodb.Agent
	for id, a := range f.agents {
		if a.Status == "disconnected" && !a.UpdatedAt.After(cutoff) {
			a.Status = "exited"
			f.agents[id] = a
			gone = append(gone, a)
		}
	}
	return gone, nil
}

func (f *fakeAgents) DeleteBySession(ctx context.Context, mongoURL, sessionID string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var n int64
	for id, a := range f.agents {
		if a.SessionID == sessionID {
			delete(f.agents, id)
			n++
		}
	}
	return n, nil
}

var _ repository.AgentRepository = (*fakeAgents)(nil)

type fakeMessages struct {
	mu       sync.Mutex
	messages map[string]mongodb.Message
	seq      int
}

func newFakeMessages() *fakeMessages { return &fakeMessages{messages: map[string]mongodb.Message{}} }

func (f *fakeMessages) EnsureCollection(ctx context.Context, mongoURL string) error { return nil }

func (f *fakeMessages) Create(ctx context.Context, mongoURL string, m mongodb.Message) (mongodb.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if m.Metadata == nil {
		m.Metadata = map[string]any{}
	}
	if m.State == "" {
		m.State = mongodb.MessageStateQueued
	}
	f.seq++
	// Anchored to real time (with a strictly increasing nanosecond nudge for
	// deterministic ordering) rather than a fixed epoch - real-time-window
	// comparisons like Send's dedup check compare against actual time.Now(),
	// so a fake "created at" back in 1970 would always look too old to match.
	now := time.Now().UTC().Add(time.Duration(f.seq) * time.Nanosecond)
	m.CreatedAt, m.UpdatedAt = now, now
	f.messages[m.ID] = m
	return m, nil
}

func (f *fakeMessages) Get(ctx context.Context, mongoURL, messageID string) (mongodb.Message, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.messages[messageID]
	return m, ok, nil
}

// fakeMessages.SetState enforces the same forward-only transition table as
// the real repository (package-level nextStates/allowedPredecessors are
// unexported to package repository, so the fake keeps its own minimal copy
// of just the guard it needs: never leave a terminal state, never enter
// held from anything else).
var fakeTerminalStates = map[string]bool{
	mongodb.MessageStateDone: true, mongodb.MessageStateRejected: true,
	mongodb.MessageStateExpired: true, mongodb.MessageStateUndeliverable: true,
}

func (f *fakeMessages) SetState(ctx context.Context, mongoURL, messageID, state string) (bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.messages[messageID]
	if !ok {
		return false, nil
	}
	if fakeTerminalStates[m.State] && m.State != state {
		return false, nil // a terminal state never moves again
	}
	if state == mongodb.MessageStateHeld && m.State != mongodb.MessageStateHeld {
		return false, nil // nothing transitions into held from elsewhere
	}
	m.State = state
	m.UpdatedAt = time.Now().UTC()
	f.messages[messageID] = m
	return true, nil
}

func (f *fakeMessages) ListPending(ctx context.Context, mongoURL, sessionID, toAgentID string) ([]mongodb.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	inFlight := map[string]bool{
		mongodb.MessageStateQueued: true, mongodb.MessageStateDispatched: true, mongodb.MessageStateInjected: true,
	}
	var out []mongodb.Message
	for _, m := range f.messages {
		if m.SessionID == sessionID && m.ToAgentID == toAgentID && inFlight[m.State] {
			out = append(out, m)
		}
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Priority != out[j].Priority {
			return out[i].Priority < out[j].Priority
		}
		return out[i].CreatedAt.Before(out[j].CreatedAt)
	})
	return out, nil
}

func (f *fakeMessages) FindRecentDuplicate(ctx context.Context, mongoURL, sessionID, from, to, kind, body string, since time.Time) (mongodb.Message, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var found *mongodb.Message
	for _, m := range f.messages {
		if m.SessionID != sessionID || m.FromAgentID != from || m.ToAgentID != to || m.Kind != kind || m.Body != body {
			continue
		}
		if m.CreatedAt.Before(since) || fakeTerminalStates[m.State] {
			continue
		}
		if found == nil || m.CreatedAt.Before(found.CreatedAt) {
			mm := m
			found = &mm
		}
	}
	if found == nil {
		return mongodb.Message{}, false, nil
	}
	return *found, true, nil
}

func (f *fakeMessages) CountHeld(ctx context.Context, mongoURL, sessionID, agentID string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var n int64
	for _, m := range f.messages {
		if m.SessionID == sessionID && m.ToAgentID == agentID && m.State == mongodb.MessageStateHeld {
			n++
		}
	}
	return n, nil
}

func (f *fakeMessages) ListHeld(ctx context.Context, mongoURL, sessionID, agentID string) ([]mongodb.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []mongodb.Message
	for _, m := range f.messages {
		if m.SessionID == sessionID && m.ToAgentID == agentID && m.State == mongodb.MessageStateHeld {
			out = append(out, m)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
}

func (f *fakeMessages) ExpireDue(ctx context.Context, mongoURL string, now time.Time) ([]mongodb.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var due []mongodb.Message
	for id, m := range f.messages {
		if fakeTerminalStates[m.State] || m.ExpiresAt.After(now) {
			continue
		}
		due = append(due, m)
		m.State = mongodb.MessageStateExpired
		m.UpdatedAt = now
		f.messages[id] = m
	}
	return due, nil
}

func (f *fakeMessages) FailPendingFor(ctx context.Context, mongoURL, agentID string) ([]mongodb.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var failed []mongodb.Message
	for id, m := range f.messages {
		if m.ToAgentID != agentID || fakeTerminalStates[m.State] {
			continue
		}
		failed = append(failed, m)
		m.State = mongodb.MessageStateUndeliverable
		m.UpdatedAt = time.Now().UTC()
		f.messages[id] = m
	}
	return failed, nil
}

func (f *fakeMessages) FindReply(ctx context.Context, mongoURL, sessionID, replyToID string) (mongodb.Message, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var found *mongodb.Message
	for _, m := range f.messages {
		if m.SessionID != sessionID || m.ReplyTo != replyToID {
			continue
		}
		if found == nil || m.CreatedAt.Before(found.CreatedAt) {
			mm := m
			found = &mm
		}
	}
	if found == nil {
		return mongodb.Message{}, false, nil
	}
	return *found, true, nil
}

func (f *fakeMessages) ListForAgent(ctx context.Context, mongoURL, sessionID, agentID string, limit int) ([]mongodb.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []mongodb.Message
	for _, m := range f.messages {
		if m.SessionID == sessionID && (m.FromAgentID == agentID || m.ToAgentID == agentID) {
			out = append(out, m)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	if limit > 0 && len(out) > limit {
		out = out[len(out)-limit:]
	}
	return out, nil
}

func (f *fakeMessages) DeleteBySession(ctx context.Context, mongoURL, sessionID string) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var n int64
	for id, m := range f.messages {
		if m.SessionID == sessionID {
			delete(f.messages, id)
			n++
		}
	}
	return n, nil
}

var _ repository.MessageRepository = (*fakeMessages)(nil)

// testConfig mirrors config.New's own defaults, so tests exercise the same
// numbers a real deployment starts with unless a test overrides one field.
func testConfig() config.Config {
	return config.Config{
		DedupWindow:     30 * time.Second,
		MessageTTL:      time.Hour,
		DisconnectGrace: 15 * time.Minute,
	}
}

func newTestService(t *testing.T) (Interface, *fakeAgents, *fakeMessages) {
	t.Helper()
	return newTestServiceWithConfig(t, testConfig())
}

func newTestServiceWithConfig(t *testing.T, cfg config.Config) (Interface, *fakeAgents, *fakeMessages) {
	t.Helper()
	agents := newFakeAgents()
	messages := newFakeMessages()
	svc, err := New(cfg, fakeShards{}, newFakeSessions(), agents, messages, newFakeShardMap(), &fakeMongoPool{})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return svc, agents, messages
}

func mustJoin(t *testing.T, svc Interface, sessionID, name string) JoinResult {
	t.Helper()
	result, err := svc.Join(context.Background(), JoinRequest{SessionID: sessionID, Name: name, Tool: "test", Role: "peer"})
	if err != nil {
		t.Fatalf("Join(%s): %v", name, err)
	}
	return result
}

func mustJoinWithPolicy(t *testing.T, svc Interface, sessionID, name string, approveInbound, canInterrupt bool) JoinResult {
	t.Helper()
	result, err := svc.Join(context.Background(), JoinRequest{
		SessionID: sessionID, Name: name, Tool: "test", Role: "peer",
		ApproveInbound: approveInbound, CanInterrupt: canInterrupt,
	})
	if err != nil {
		t.Fatalf("Join(%s): %v", name, err)
	}
	return result
}

func TestSend_RejectsEmptyAndOversizedBody(t *testing.T) {
	svc, _, _ := newTestService(t)
	ctx := context.Background()
	alice := mustJoin(t, svc, SessionNew, "alice")
	mustJoin(t, svc, alice.SessionID, "bob")

	if _, err := svc.Send(ctx, alice.SessionID, alice.AgentID, SendRequest{To: "bob", Body: ""}); err == nil {
		t.Fatal("expected an error for an empty body")
	}
	if _, err := svc.Send(ctx, alice.SessionID, alice.AgentID, SendRequest{To: "bob", Body: strings.Repeat("x", MaxBodyBytes+1)}); err == nil {
		t.Fatal("expected an error for an oversized body")
	}
}

func TestSend_DefaultsKindAndStartsQueued(t *testing.T) {
	svc, _, messages := newTestService(t)
	ctx := context.Background()
	alice := mustJoin(t, svc, SessionNew, "alice")
	mustJoin(t, svc, alice.SessionID, "bob")

	outcome, err := svc.Send(ctx, alice.SessionID, alice.AgentID, SendRequest{To: "bob", Body: "hi"})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if outcome.Kind != mongodb.DefaultMessageKind {
		t.Fatalf("expected default kind %q, got %q", mongodb.DefaultMessageKind, outcome.Kind)
	}
	stored, found, err := messages.Get(ctx, testShardURL, outcome.MessageID)
	if err != nil || !found {
		t.Fatalf("Get: found=%v err=%v", found, err)
	}
	if stored.State != mongodb.MessageStateQueued {
		t.Fatalf("expected a freshly-sent message to be queued, got %q", stored.State)
	}
}

func TestJoin_ResumeRejectsBadTokenAndLiveIdentity(t *testing.T) {
	svc, _, _ := newTestService(t)
	ctx := context.Background()
	alice := mustJoin(t, svc, SessionNew, "alice")

	if _, err := svc.Join(ctx, JoinRequest{SessionID: alice.SessionID, Name: "alice", Token: "not-the-real-token", Tool: "test", Role: "peer"}); err == nil {
		t.Fatal("expected a bad-token error")
	}

	// alice's agent is still "connected" (never disconnected) - resuming
	// with the CORRECT token must be rejected as agent_live, not silently
	// let a second process share the identity.
	if _, err := svc.Join(ctx, JoinRequest{SessionID: alice.SessionID, Name: "alice", Token: alice.Token, Tool: "test", Role: "peer"}); !errors.Is(err, ErrAgentLive) {
		t.Fatalf("expected ErrAgentLive, got %v", err)
	}
}

func TestJoin_ResumeSucceedsAfterDisconnect(t *testing.T) {
	svc, agents, _ := newTestService(t)
	ctx := context.Background()
	alice := mustJoin(t, svc, SessionNew, "alice")

	if err := svc.Disconnect(ctx, alice.SessionID, alice.AgentID); err != nil {
		t.Fatalf("Disconnect: %v", err)
	}

	resumed, err := svc.Join(ctx, JoinRequest{SessionID: alice.SessionID, Name: "alice", Token: alice.Token, Tool: "test", Role: "peer"})
	if err != nil {
		t.Fatalf("Join (resume): %v", err)
	}
	if !resumed.Resumed || resumed.AgentID != alice.AgentID {
		t.Fatalf("expected a successful resume of the same agent, got %+v", resumed)
	}
	agent, found, err := agents.Get(ctx, testShardURL, alice.AgentID)
	if err != nil || !found {
		t.Fatalf("Get: found=%v err=%v", found, err)
	}
	if agent.Status != "connected" {
		t.Fatalf("expected the resumed agent to be connected again, got %q", agent.Status)
	}
}

func TestWait_ReturnsReplyWhenOneArrives(t *testing.T) {
	svc, _, _ := newTestService(t)
	ctx := context.Background()
	alice := mustJoin(t, svc, SessionNew, "alice")
	mustJoin(t, svc, alice.SessionID, "bob")

	sent, err := svc.Send(ctx, alice.SessionID, alice.AgentID, SendRequest{To: "bob", Body: "question"})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	done := make(chan WaitOutcome, 1)
	go func() {
		outcome, err := svc.Wait(ctx, alice.SessionID, alice.AgentID, sent.MessageID, 2*time.Second)
		if err != nil {
			outcome.TimedOut = true // surface the error via a failing assertion below, not a data race on t
		}
		done <- outcome
	}()

	time.Sleep(50 * time.Millisecond) // let Wait start polling before the reply exists
	if _, err := svc.Send(ctx, alice.SessionID, sent.TargetAgentID, SendRequest{To: "alice", Body: "answer", ReplyTo: sent.MessageID}); err != nil {
		t.Fatalf("Send (reply): %v", err)
	}

	select {
	case outcome := <-done:
		if outcome.TimedOut || outcome.Reply == nil || outcome.Reply.Body != "answer" {
			t.Fatalf("unexpected outcome: %+v", outcome)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("Wait never returned")
	}
}

func TestWait_TimesOutWithNoReply(t *testing.T) {
	svc, _, _ := newTestService(t)
	ctx := context.Background()
	alice := mustJoin(t, svc, SessionNew, "alice")
	mustJoin(t, svc, alice.SessionID, "bob")

	sent, err := svc.Send(ctx, alice.SessionID, alice.AgentID, SendRequest{To: "bob", Body: "question"})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	outcome, err := svc.Wait(ctx, alice.SessionID, alice.AgentID, sent.MessageID, 300*time.Millisecond)
	if err != nil {
		t.Fatalf("Wait: %v", err)
	}
	if !outcome.TimedOut {
		t.Fatalf("expected a timeout, got %+v", outcome)
	}
}

func TestWait_RejectsCallerWhoIsNeitherSenderNorRecipient(t *testing.T) {
	svc, _, _ := newTestService(t)
	ctx := context.Background()
	alice := mustJoin(t, svc, SessionNew, "alice")
	mustJoin(t, svc, alice.SessionID, "bob")
	eve := mustJoin(t, svc, alice.SessionID, "eve")

	sent, err := svc.Send(ctx, alice.SessionID, alice.AgentID, SendRequest{To: "bob", Body: "private"})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	if _, err := svc.Wait(ctx, alice.SessionID, eve.AgentID, sent.MessageID, time.Second); err == nil {
		t.Fatal("expected an authorization error")
	}
}

func TestContext_ReturnsChronologicalHistory(t *testing.T) {
	svc, _, _ := newTestService(t)
	ctx := context.Background()
	alice := mustJoin(t, svc, SessionNew, "alice")
	mustJoin(t, svc, alice.SessionID, "bob")

	if _, err := svc.Send(ctx, alice.SessionID, alice.AgentID, SendRequest{To: "bob", Body: "first"}); err != nil {
		t.Fatalf("Send: %v", err)
	}
	if _, err := svc.Send(ctx, alice.SessionID, alice.AgentID, SendRequest{To: "bob", Body: "second"}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	messages, err := svc.Context(ctx, alice.SessionID, alice.AgentID, "bob", 10)
	if err != nil {
		t.Fatalf("Context: %v", err)
	}
	if len(messages) != 2 || messages[0].Body != "first" || messages[1].Body != "second" {
		t.Fatalf("unexpected history: %+v", messages)
	}
}

// ---- Phase 2: priorities, hop-limit, dedup, approve-inbound, sweep --------

func TestSend_DowngradesP0WithoutCanInterrupt(t *testing.T) {
	svc, _, _ := newTestService(t)
	ctx := context.Background()
	alice := mustJoin(t, svc, SessionNew, "alice") // CanInterrupt defaults false
	mustJoin(t, svc, alice.SessionID, "bob")

	outcome, err := svc.Send(ctx, alice.SessionID, alice.AgentID, SendRequest{To: "bob", Body: "urgent", Priority: 0})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if outcome.Priority != 1 {
		t.Fatalf("expected P0 to be silently downgraded to P1 without CanInterrupt, got %d", outcome.Priority)
	}
}

func TestSend_AllowsP0WithCanInterrupt(t *testing.T) {
	svc, _, _ := newTestService(t)
	ctx := context.Background()
	alice := mustJoinWithPolicy(t, svc, SessionNew, "alice", false, true)
	mustJoin(t, svc, alice.SessionID, "bob")

	outcome, err := svc.Send(ctx, alice.SessionID, alice.AgentID, SendRequest{To: "bob", Body: "urgent", Priority: 0})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if outcome.Priority != 0 {
		t.Fatalf("expected P0 to be honored for a sender with CanInterrupt, got %d", outcome.Priority)
	}
}

func TestSend_CoalescesRecentDuplicate(t *testing.T) {
	svc, _, _ := newTestService(t)
	ctx := context.Background()
	alice := mustJoin(t, svc, SessionNew, "alice")
	mustJoin(t, svc, alice.SessionID, "bob")

	first, err := svc.Send(ctx, alice.SessionID, alice.AgentID, SendRequest{To: "bob", Body: "retry me"})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	second, err := svc.Send(ctx, alice.SessionID, alice.AgentID, SendRequest{To: "bob", Body: "retry me"})
	if err != nil {
		t.Fatalf("Send (retry): %v", err)
	}
	if second.MessageID != first.MessageID {
		t.Fatalf("expected the retry to coalesce into %q, got a new message %q", first.MessageID, second.MessageID)
	}
	if second.Note == "" {
		t.Fatal("expected a Note explaining the coalesce")
	}
}

func TestSend_HoldsForApproveInboundTarget(t *testing.T) {
	svc, _, _ := newTestService(t)
	ctx := context.Background()
	alice := mustJoin(t, svc, SessionNew, "alice")
	mustJoinWithPolicy(t, svc, alice.SessionID, "bob", true, false)

	outcome, err := svc.Send(ctx, alice.SessionID, alice.AgentID, SendRequest{To: "bob", Body: "hi"})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if outcome.State != mongodb.MessageStateHeld {
		t.Fatalf("expected held for an approve-inbound target, got %q", outcome.State)
	}
}

// TestSend_HoldsAtHopLimitMultiple builds a real alternating reply chain
// (alice -> bob -> alice -> ...) up to MaxHops and confirms the message
// that crosses the limit is held again for human approval, exactly like the
// local daemon's own hop-limit rule.
func TestSend_HoldsAtHopLimitMultiple(t *testing.T) {
	svc, _, _ := newTestService(t)
	ctx := context.Background()
	alice := mustJoin(t, svc, SessionNew, "alice")
	bob := mustJoin(t, svc, alice.SessionID, "bob")

	fromID, fromIsAlice, toName, replyTo := alice.AgentID, true, "bob", ""
	var last SendOutcome
	for hop := 0; hop <= MaxHops; hop++ {
		outcome, err := svc.Send(ctx, alice.SessionID, fromID, SendRequest{To: toName, Body: fmt.Sprintf("hop %d", hop), ReplyTo: replyTo})
		if err != nil {
			t.Fatalf("Send hop %d: %v", hop, err)
		}
		last, replyTo = outcome, outcome.MessageID
		if fromIsAlice {
			fromID, toName = bob.AgentID, "alice"
		} else {
			fromID, toName = alice.AgentID, "bob"
		}
		fromIsAlice = !fromIsAlice
	}
	if last.State != mongodb.MessageStateHeld {
		t.Fatalf("expected the hop-%d message to be held, got state %q", MaxHops, last.State)
	}
}

func TestSend_ImplicitDoneOnReply(t *testing.T) {
	svc, _, messages := newTestService(t)
	ctx := context.Background()
	alice := mustJoin(t, svc, SessionNew, "alice")
	bob := mustJoin(t, svc, alice.SessionID, "bob")

	question, err := svc.Send(ctx, alice.SessionID, alice.AgentID, SendRequest{To: "bob", Body: "question?"})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if _, err := svc.Send(ctx, alice.SessionID, bob.AgentID, SendRequest{To: "alice", Body: "answer!", ReplyTo: question.MessageID}); err != nil {
		t.Fatalf("Send (reply): %v", err)
	}
	stored, found, err := messages.Get(ctx, testShardURL, question.MessageID)
	if err != nil || !found {
		t.Fatalf("Get: found=%v err=%v", found, err)
	}
	if stored.State != mongodb.MessageStateDone {
		t.Fatalf("expected the answered question to be done, got %q", stored.State)
	}
}

func TestApprove_ReleasesOldestHeldByDefault(t *testing.T) {
	svc, _, _ := newTestService(t)
	ctx := context.Background()
	alice := mustJoin(t, svc, SessionNew, "alice")
	bob := mustJoinWithPolicy(t, svc, alice.SessionID, "bob", true, false)

	first, err := svc.Send(ctx, alice.SessionID, alice.AgentID, SendRequest{To: "bob", Body: "first"})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if _, err := svc.Send(ctx, alice.SessionID, alice.AgentID, SendRequest{To: "bob", Body: "second"}); err != nil {
		t.Fatalf("Send: %v", err)
	}

	approved, err := svc.Approve(ctx, alice.SessionID, bob.AgentID, "")
	if err != nil {
		t.Fatalf("Approve: %v", err)
	}
	if approved.ID != first.MessageID {
		t.Fatalf("expected the oldest held message (%q) to be approved, got %q", first.MessageID, approved.ID)
	}
	if approved.State != mongodb.MessageStateQueued {
		t.Fatalf("expected an approved message to become queued, got %q", approved.State)
	}
}

func TestReject_NotifiesSenderAndIsTerminal(t *testing.T) {
	svc, _, messages := newTestService(t)
	ctx := context.Background()
	alice := mustJoin(t, svc, SessionNew, "alice")
	bob := mustJoinWithPolicy(t, svc, alice.SessionID, "bob", true, false)

	sent, err := svc.Send(ctx, alice.SessionID, alice.AgentID, SendRequest{To: "bob", Body: "please approve"})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	rejected, err := svc.Reject(ctx, alice.SessionID, bob.AgentID, sent.MessageID)
	if err != nil {
		t.Fatalf("Reject: %v", err)
	}
	if rejected.State != mongodb.MessageStateRejected {
		t.Fatalf("expected rejected, got %q", rejected.State)
	}
	if matched, err := messages.SetState(ctx, testShardURL, sent.MessageID, mongodb.MessageStateQueued); err != nil || matched {
		t.Fatalf("expected SetState to refuse moving a terminal message again, matched=%v err=%v", matched, err)
	}

	history, err := svc.Context(ctx, alice.SessionID, alice.AgentID, "alice", 10)
	if err != nil {
		t.Fatalf("Context: %v", err)
	}
	foundNotify := false
	for _, m := range history {
		if m.Kind == "notify" && m.ToAgentID == alice.AgentID {
			foundNotify = true
		}
	}
	if !foundNotify {
		t.Fatal("expected a notify message back to alice explaining the rejection")
	}
}

func TestListHeld_ReturnsOldestFirst(t *testing.T) {
	svc, _, _ := newTestService(t)
	ctx := context.Background()
	alice := mustJoin(t, svc, SessionNew, "alice")
	bob := mustJoinWithPolicy(t, svc, alice.SessionID, "bob", true, false)

	first, err := svc.Send(ctx, alice.SessionID, alice.AgentID, SendRequest{To: "bob", Body: "first"})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	second, err := svc.Send(ctx, alice.SessionID, alice.AgentID, SendRequest{To: "bob", Body: "second"})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	held, err := svc.ListHeld(ctx, alice.SessionID, bob.AgentID)
	if err != nil {
		t.Fatalf("ListHeld: %v", err)
	}
	if len(held) != 2 || held[0].ID != first.MessageID || held[1].ID != second.MessageID {
		t.Fatalf("unexpected held order: %+v", held)
	}
}

func TestReportState_OnlyRecipientCanReport(t *testing.T) {
	svc, _, _ := newTestService(t)
	ctx := context.Background()
	alice := mustJoin(t, svc, SessionNew, "alice")
	bob := mustJoin(t, svc, alice.SessionID, "bob")

	sent, err := svc.Send(ctx, alice.SessionID, alice.AgentID, SendRequest{To: "bob", Body: "hi"})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if err := svc.ReportState(ctx, alice.SessionID, alice.AgentID, sent.MessageID, mongodb.MessageStateInjected); err == nil {
		t.Fatal("expected the sender to be rejected - only the recipient may report state")
	}
	if err := svc.ReportState(ctx, alice.SessionID, bob.AgentID, sent.MessageID, mongodb.MessageStateInjected); err != nil {
		t.Fatalf("ReportState (recipient): %v", err)
	}
}

func TestReportState_RejectsUnknownState(t *testing.T) {
	svc, _, _ := newTestService(t)
	ctx := context.Background()
	alice := mustJoin(t, svc, SessionNew, "alice")
	bob := mustJoin(t, svc, alice.SessionID, "bob")
	sent, err := svc.Send(ctx, alice.SessionID, alice.AgentID, SendRequest{To: "bob", Body: "hi"})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if err := svc.ReportState(ctx, alice.SessionID, bob.AgentID, sent.MessageID, "bogus"); err == nil {
		t.Fatal("expected an error for an unreportable state")
	}
}

func TestSweep_ExpiresDueMessagesAndNotifiesSender(t *testing.T) {
	cfg := testConfig()
	cfg.MessageTTL = time.Millisecond
	svc, _, messages := newTestServiceWithConfig(t, cfg)
	ctx := context.Background()
	alice := mustJoin(t, svc, SessionNew, "alice")
	mustJoin(t, svc, alice.SessionID, "bob")

	sent, err := svc.Send(ctx, alice.SessionID, alice.AgentID, SendRequest{To: "bob", Body: "will expire"})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	time.Sleep(5 * time.Millisecond)

	if err := svc.Sweep(ctx, testShardURL); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	stored, found, err := messages.Get(ctx, testShardURL, sent.MessageID)
	if err != nil || !found {
		t.Fatalf("Get: found=%v err=%v", found, err)
	}
	if stored.State != mongodb.MessageStateExpired {
		t.Fatalf("expected expired, got %q", stored.State)
	}

	history, err := svc.Context(ctx, alice.SessionID, alice.AgentID, "alice", 10)
	if err != nil {
		t.Fatalf("Context: %v", err)
	}
	foundNotify := false
	for _, m := range history {
		if m.Kind == "notify" {
			foundNotify = true
		}
	}
	if !foundNotify {
		t.Fatal("expected a notify message back to alice explaining the expiry")
	}
}

func TestSweep_ReapsGoneAgentsAndFailsPendingMail(t *testing.T) {
	cfg := testConfig()
	cfg.DisconnectGrace = time.Millisecond
	svc, agents, messages := newTestServiceWithConfig(t, cfg)
	ctx := context.Background()
	alice := mustJoin(t, svc, SessionNew, "alice")
	bob := mustJoin(t, svc, alice.SessionID, "bob")

	sent, err := svc.Send(ctx, alice.SessionID, alice.AgentID, SendRequest{To: "bob", Body: "hello"})
	if err != nil {
		t.Fatalf("Send: %v", err)
	}
	if err := svc.Disconnect(ctx, alice.SessionID, bob.AgentID); err != nil {
		t.Fatalf("Disconnect: %v", err)
	}
	time.Sleep(5 * time.Millisecond)

	if err := svc.Sweep(ctx, testShardURL); err != nil {
		t.Fatalf("Sweep: %v", err)
	}

	agent, found, err := agents.Get(ctx, testShardURL, bob.AgentID)
	if err != nil || !found {
		t.Fatalf("Get: found=%v err=%v", found, err)
	}
	if agent.Status != "exited" {
		t.Fatalf("expected bob to be reaped as exited, got %q", agent.Status)
	}

	stored, found, err := messages.Get(ctx, testShardURL, sent.MessageID)
	if err != nil || !found {
		t.Fatalf("Get: found=%v err=%v", found, err)
	}
	if stored.State != mongodb.MessageStateUndeliverable {
		t.Fatalf("expected undeliverable, got %q", stored.State)
	}
}

// fakeShardMap is an in-memory stand-in for repository.ShardMapRepository,
// only used by the DeleteSessionsOlderThan tests below - Count/GetBySessionID
// aren't exercised by that code path but must exist to satisfy the interface.
type fakeShardMap struct {
	mu   sync.Mutex
	rows map[string]bool // sessionID -> exists
}

func newFakeShardMap() *fakeShardMap { return &fakeShardMap{rows: map[string]bool{}} }

func (f *fakeShardMap) Count(ctx context.Context) (int64, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return int64(len(f.rows)), nil
}

func (f *fakeShardMap) GetBySessionID(ctx context.Context, sessionID string) (postgres.SessionShardMap, bool, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.rows[sessionID] {
		return postgres.SessionShardMap{SessionID: sessionID}, true, nil
	}
	return postgres.SessionShardMap{}, false, nil
}

func (f *fakeShardMap) Create(ctx context.Context, sessionID string, mongoURLID uint) (postgres.SessionShardMap, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.rows[sessionID] = true
	return postgres.SessionShardMap{SessionID: sessionID, MongoURLID: mongoURLID}, nil
}

func (f *fakeShardMap) DeleteBySessionID(ctx context.Context, sessionID string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	delete(f.rows, sessionID)
	return nil
}

var _ repository.ShardMapRepository = (*fakeShardMap)(nil)

// fakeMongoPool is an in-memory stand-in for mongodb.Interface, only used by
// the DeleteSessionsOlderThan tests below - it runs WithTransaction's fn
// directly against the given ctx (no real session/rollback), since proving
// genuine ACID atomicity is the real-DB integration test's job (see
// server/integration); this fake exists to unit-test the orchestration logic
// (the idle check, the delete-ordering, the report) fast and without a
// database. Database/Close/Ping aren't exercised by that code path.
type fakeMongoPool struct {
	mu          sync.Mutex
	txnCalls    int
	failWithErr error // if set, WithTransaction returns this without calling fn
}

func (f *fakeMongoPool) Database(mongoURL string) (*mongodriver.Database, error) {
	return nil, errors.New("session test: Database is not used by these tests")
}

func (f *fakeMongoPool) Close(ctx context.Context) error { return nil }

func (f *fakeMongoPool) Ping(ctx context.Context) error { return nil }

func (f *fakeMongoPool) WithTransaction(ctx context.Context, mongoURL string, fn func(sessCtx context.Context) error) error {
	f.mu.Lock()
	f.txnCalls++
	failErr := f.failWithErr
	f.mu.Unlock()
	if failErr != nil {
		return failErr
	}
	return fn(ctx)
}

var _ mongodb.Interface = (*fakeMongoPool)(nil)

// newTestServiceForDelete builds a service with direct access to every fake
// DeleteSessionsOlderThan touches - kept separate from newTestServiceWithConfig
// (used by every other test in this file) so those ~25 call sites never need
// to change for a capability only these new tests exercise.
func newTestServiceForDelete(t *testing.T) (svc Interface, sessions *fakeSessions, agents *fakeAgents, messages *fakeMessages, shardMap *fakeShardMap, mongoPool *fakeMongoPool) {
	t.Helper()
	sessions = newFakeSessions()
	agents = newFakeAgents()
	messages = newFakeMessages()
	shardMap = newFakeShardMap()
	mongoPool = &fakeMongoPool{}
	svc, err := New(testConfig(), fakeShards{}, sessions, agents, messages, shardMap, mongoPool)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return svc, sessions, agents, messages, shardMap, mongoPool
}

func TestDeleteSessionsOlderThan_RemovesFullyIdleSessionAtomically(t *testing.T) {
	svc, sessions, agents, messages, shardMap, mongoPool := newTestServiceForDelete(t)
	ctx := context.Background()
	old := time.Now().UTC().Add(-2 * time.Hour)
	cutoff := time.Now().UTC().Add(-time.Hour)

	sess, _ := sessions.Create(ctx, testShardURL, mongodb.Session{ID: "sess-1", UpdatedAt: old})
	agent, _ := agents.Create(ctx, testShardURL, mongodb.Agent{ID: "agent-1", SessionID: sess.ID, Status: "disconnected"})
	agents.mu.Lock()
	a := agents.agents[agent.ID]
	a.UpdatedAt = old
	agents.agents[agent.ID] = a
	agents.mu.Unlock()
	messages.Create(ctx, testShardURL, mongodb.Message{ID: "msg-1", SessionID: sess.ID})
	shardMap.Create(ctx, sess.ID, 1)

	rep, err := svc.DeleteSessionsOlderThan(ctx, testShardURL, cutoff)
	if err != nil {
		t.Fatalf("DeleteSessionsOlderThan: %v", err)
	}
	if rep.Sessions != 1 || rep.Agents != 1 || rep.Messages != 1 {
		t.Fatalf("unexpected report: %+v", rep)
	}
	if mongoPool.txnCalls != 1 {
		t.Fatalf("expected exactly 1 transaction, got %d", mongoPool.txnCalls)
	}
	if _, found, _ := sessions.Get(ctx, testShardURL, sess.ID); found {
		t.Error("session should be gone")
	}
	if _, found, _ := shardMap.GetBySessionID(ctx, sess.ID); found {
		t.Error("shard map row should be gone")
	}
}

func TestDeleteSessionsOlderThan_SkipsSessionWithAConnectedAgent(t *testing.T) {
	svc, sessions, agents, _, _, mongoPool := newTestServiceForDelete(t)
	ctx := context.Background()
	old := time.Now().UTC().Add(-2 * time.Hour)
	cutoff := time.Now().UTC().Add(-time.Hour)

	sess, _ := sessions.Create(ctx, testShardURL, mongodb.Session{ID: "sess-2", UpdatedAt: old})
	agent, _ := agents.Create(ctx, testShardURL, mongodb.Agent{ID: "agent-2", SessionID: sess.ID, Status: "connected"})
	agents.mu.Lock()
	a := agents.agents[agent.ID]
	a.UpdatedAt = old
	agents.agents[agent.ID] = a
	agents.mu.Unlock()

	rep, err := svc.DeleteSessionsOlderThan(ctx, testShardURL, cutoff)
	if err != nil {
		t.Fatalf("DeleteSessionsOlderThan: %v", err)
	}
	if rep.Sessions != 0 {
		t.Fatalf("a session with a connected agent must survive, got report %+v", rep)
	}
	if mongoPool.txnCalls != 0 {
		t.Fatalf("no transaction should have been attempted, got %d", mongoPool.txnCalls)
	}
	if _, found, _ := sessions.Get(ctx, testShardURL, sess.ID); !found {
		t.Error("session should still exist")
	}
}

func TestDeleteSessionsOlderThan_SkipsSessionWithRecentAgentActivity(t *testing.T) {
	svc, sessions, agents, _, _, _ := newTestServiceForDelete(t)
	ctx := context.Background()
	sessionLooksOld := time.Now().UTC().Add(-2 * time.Hour)
	cutoff := time.Now().UTC().Add(-time.Hour)

	// The session document itself looks old, but its one agent was active
	// after cutoff - proves the session's own (stale-by-design) UpdatedAt
	// alone would have been wrong, and the per-agent check is load-bearing.
	sess, _ := sessions.Create(ctx, testShardURL, mongodb.Session{ID: "sess-3", UpdatedAt: sessionLooksOld})
	agents.Create(ctx, testShardURL, mongodb.Agent{ID: "agent-3", SessionID: sess.ID, Status: "disconnected"})
	// fakeAgents.Create already stamps UpdatedAt to time.Now(), i.e. recent.

	rep, err := svc.DeleteSessionsOlderThan(ctx, testShardURL, cutoff)
	if err != nil {
		t.Fatalf("DeleteSessionsOlderThan: %v", err)
	}
	if rep.Sessions != 0 {
		t.Fatalf("a session with a recently-active agent must survive, got report %+v", rep)
	}
	if _, found, _ := sessions.Get(ctx, testShardURL, sess.ID); !found {
		t.Error("session should still exist")
	}
}

func TestDeleteSessionsOlderThan_HandlesASessionWithNoAgentsAtAll(t *testing.T) {
	svc, sessions, _, _, _, mongoPool := newTestServiceForDelete(t)
	ctx := context.Background()
	old := time.Now().UTC().Add(-2 * time.Hour)
	cutoff := time.Now().UTC().Add(-time.Hour)

	sess, _ := sessions.Create(ctx, testShardURL, mongodb.Session{ID: "sess-4", UpdatedAt: old})

	rep, err := svc.DeleteSessionsOlderThan(ctx, testShardURL, cutoff)
	if err != nil {
		t.Fatalf("DeleteSessionsOlderThan: %v", err)
	}
	if rep.Sessions != 1 {
		t.Fatalf("an ancient, agent-less session should be deleted by age alone, got report %+v", rep)
	}
	if mongoPool.txnCalls != 1 {
		t.Fatalf("expected exactly 1 transaction, got %d", mongoPool.txnCalls)
	}
	if _, found, _ := sessions.Get(ctx, testShardURL, sess.ID); found {
		t.Error("session should be gone")
	}
}

// TestDeleteSessionsOlderThan_PropagatesTransactionErrorsWithoutTouchingState
// proves the orchestration layer surfaces a failed transaction as an error
// rather than swallowing it or reporting a partial success - it does NOT
// prove real cross-collection ACID rollback (this fake has no real
// transaction to roll back); that's server/integration's job, against the
// real driver and a real replica set.
func TestDeleteSessionsOlderThan_PropagatesTransactionErrorsWithoutTouchingState(t *testing.T) {
	svc, sessions, agents, _, _, mongoPool := newTestServiceForDelete(t)
	ctx := context.Background()
	old := time.Now().UTC().Add(-2 * time.Hour)
	cutoff := time.Now().UTC().Add(-time.Hour)

	sess, _ := sessions.Create(ctx, testShardURL, mongodb.Session{ID: "sess-5", UpdatedAt: old})
	agent, _ := agents.Create(ctx, testShardURL, mongodb.Agent{ID: "agent-5", SessionID: sess.ID, Status: "disconnected"})
	agents.mu.Lock()
	a := agents.agents[agent.ID]
	a.UpdatedAt = old
	agents.agents[agent.ID] = a
	agents.mu.Unlock()

	mongoPool.failWithErr = errors.New("simulated transaction failure")
	_, err := svc.DeleteSessionsOlderThan(ctx, testShardURL, cutoff)
	if err == nil {
		t.Fatal("expected an error from the failed transaction")
	}
	if _, found, _ := sessions.Get(ctx, testShardURL, sess.ID); !found {
		t.Error("session should NOT have been deleted: the transaction never committed")
	}
	if _, found, _ := agents.Get(ctx, testShardURL, agent.ID); !found {
		t.Error("agent should NOT have been deleted: the transaction never committed")
	}
}
