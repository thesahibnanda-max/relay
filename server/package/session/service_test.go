package session

import (
	"context"
	"errors"
	"sort"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/thesahibnanda-max/relay/server/package/database/mongodb"
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
	// A strictly increasing, fake "created at" so ordering assertions in
	// tests are deterministic instead of racing the real clock's resolution.
	now := time.Unix(0, 0).UTC().Add(time.Duration(f.seq) * time.Millisecond)
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

func (f *fakeMessages) SetState(ctx context.Context, mongoURL, messageID, state string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	m, ok := f.messages[messageID]
	if !ok {
		return nil
	}
	m.State = state
	m.UpdatedAt = time.Now().UTC()
	f.messages[messageID] = m
	return nil
}

func (f *fakeMessages) ListPending(ctx context.Context, mongoURL, sessionID, toAgentID string) ([]mongodb.Message, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	var out []mongodb.Message
	for _, m := range f.messages {
		if m.SessionID == sessionID && m.ToAgentID == toAgentID && m.State != mongodb.MessageStateAcknowledged {
			out = append(out, m)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	return out, nil
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

var _ repository.MessageRepository = (*fakeMessages)(nil)

func newTestService(t *testing.T) (Interface, *fakeAgents, *fakeMessages) {
	t.Helper()
	agents := newFakeAgents()
	messages := newFakeMessages()
	svc, err := New(fakeShards{}, newFakeSessions(), agents, messages)
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
