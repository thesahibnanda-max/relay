// Package session is the business logic tying sharding and the
// Mongo/Postgres repositories together: creating global sessions, joining
// (or resuming) an agent into one, and Phase 1's send/list_agents/wait/
// get_context/acknowledge operations. Priorities, hop-limits, rate-limits,
// dedup and --approve-inbound are deliberately not enforced yet - see the
// project plan's Phase 2 table for how each slots into this model later.
package session

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/thesahibnanda-max/relay/server/package/database/mongodb"
	"github.com/thesahibnanda-max/relay/server/package/database/repository"
	"github.com/thesahibnanda-max/relay/server/package/sharding"
)

// SessionNew is the sentinel Hello.Session value meaning "create a fresh
// global session," mirroring the local daemon's proto.SessionNew.
const SessionNew = "NEW"

// MaxBodyBytes mirrors the local daemon's own message body cap.
const MaxBodyBytes = 32 << 10

const (
	waitPollInterval    = 200 * time.Millisecond
	defaultContextLimit = 20
	maxContextLimit     = 50
)

// Sentinel Join errors, each mirroring one of the local daemon's own
// proto.Code* constants - ws/hub.go maps these back to the matching wire
// error code, so the CLI's existing friendly() error messages work
// identically for global and local sessions.
var (
	ErrAgentLive       = errors.New("session: agent is already connected")
	ErrBadToken        = errors.New("session: bad resume token")
	ErrSessionNotFound = errors.New("session: session not found")
	ErrNameTaken       = errors.New("session: name is already taken in this session")
)

// JoinRequest is what a connecting agent's Hello carries.
type JoinRequest struct {
	SessionID string // "", SessionNew, or an existing session's ULID
	Name      string
	Token     string // non-empty means "resume this exact agent"
	Tool      string
	Role      string
}

// JoinResult is what a Welcome is built from.
type JoinResult struct {
	SessionID string
	AgentID   string
	Name      string
	Token     string // set only on fresh registration, never on resume
	Resumed   bool
}

// Interface is the session service every WebSocket connection's RPCs go
// through.
type Interface interface {
	Join(ctx context.Context, req JoinRequest) (JoinResult, error)
	// Disconnect marks an agent disconnected - called when its connection
	// closes, so ListAgents/resume reflect reality instead of the agent
	// looking "connected" forever.
	Disconnect(ctx context.Context, sessionID, agentID string) error
	Send(ctx context.Context, sessionID, fromAgentID string, req SendRequest) (SendOutcome, error)
	ListAgents(ctx context.Context, sessionID string) ([]mongodb.Agent, error)
	// PendingFor returns everything still owed to agentID (not yet
	// acknowledged) - what a (re)connecting agent gets replayed.
	PendingFor(ctx context.Context, sessionID, agentID string) ([]mongodb.Message, error)
	// MarkDispatched records that a deliver frame was successfully pushed to
	// the target's live connection - not confirmation it was received.
	MarkDispatched(ctx context.Context, sessionID, messageID string) error
	// Acknowledge records that the recipient actually got a message. Only
	// the recipient may acknowledge its own message.
	Acknowledge(ctx context.Context, sessionID, agentID, messageID string) error
	// Wait blocks (up to timeout) for a reply to messageID or for it to
	// reach mongodb.MessageStateAcknowledged. Only the message's sender or
	// recipient may wait on it.
	Wait(ctx context.Context, sessionID, agentID, messageID string, timeout time.Duration) (WaitOutcome, error)
	// Context returns forAgentName's recent message history in this
	// session (up to limit, chronological order) - the data behind the
	// global session's relay_get_context equivalent. Unlike the local
	// daemon's version, this is message history, not a terminal transcript:
	// the server never receives raw terminal bytes from any agent.
	Context(ctx context.Context, sessionID, agentID, forAgentName string, limit int) ([]mongodb.Message, error)
}

type service struct {
	shards   sharding.Interface
	sessions repository.SessionRepository
	agents   repository.AgentRepository
	messages repository.MessageRepository
}

func New(
	shards sharding.Interface,
	sessions repository.SessionRepository,
	agents repository.AgentRepository,
	messages repository.MessageRepository,
) (Interface, error) {
	if shards == nil {
		return nil, errors.New("session: shards is nil")
	}
	if sessions == nil {
		return nil, errors.New("session: sessions repository is nil")
	}
	if agents == nil {
		return nil, errors.New("session: agents repository is nil")
	}
	if messages == nil {
		return nil, errors.New("session: messages repository is nil")
	}
	return service{shards: shards, sessions: sessions, agents: agents, messages: messages}, nil
}

func (s service) Join(ctx context.Context, req JoinRequest) (JoinResult, error) {
	if req.Name == "" {
		return JoinResult{}, errors.New("session: name is required")
	}

	sessionID := req.SessionID
	isNew := sessionID == "" || sessionID == SessionNew
	if isNew {
		sessionID = ulid.Make().String()
	}

	shardURL, err := s.shards.ShardURLFor(ctx, sessionID)
	if err != nil {
		return JoinResult{}, fmt.Errorf("session: resolving shard: %w", err)
	}

	if isNew {
		if err := s.ensureCollections(ctx, shardURL); err != nil {
			return JoinResult{}, err
		}
		if _, err := s.sessions.Create(ctx, shardURL, mongodb.Session{ID: sessionID, Status: "active"}); err != nil {
			return JoinResult{}, fmt.Errorf("session: creating session: %w", err)
		}
	} else if _, found, err := s.sessions.Get(ctx, shardURL, sessionID); err != nil {
		return JoinResult{}, err
	} else if !found {
		return JoinResult{}, fmt.Errorf("%w: %q", ErrSessionNotFound, sessionID)
	}

	if req.Token != "" {
		return s.resume(ctx, shardURL, sessionID, req)
	}
	return s.register(ctx, shardURL, sessionID, req)
}

// resume looks up an existing agent by name and admits it only if the
// presented token's hash matches what was stored at registration, and the
// identity isn't already live elsewhere.
func (s service) resume(ctx context.Context, shardURL, sessionID string, req JoinRequest) (JoinResult, error) {
	existing, found, err := s.agents.GetByName(ctx, shardURL, sessionID, req.Name)
	if err != nil {
		return JoinResult{}, err
	}
	presented := hashToken(req.Token)
	if !found || subtle.ConstantTimeCompare([]byte(existing.TokenHash), []byte(presented)) != 1 {
		return JoinResult{}, ErrBadToken
	}
	if existing.Status == "connected" {
		return JoinResult{}, ErrAgentLive
	}
	if err := s.agents.SetStatus(ctx, shardURL, existing.ID, "connected"); err != nil {
		return JoinResult{}, err
	}
	return JoinResult{SessionID: sessionID, AgentID: existing.ID, Name: existing.Name, Resumed: true}, nil
}

// register creates a brand-new agent; an explicit name that's already taken
// in this session is a hard error - no local-daemon-style auto-retry-with-a-
// different-name in this skeleton pass.
func (s service) register(ctx context.Context, shardURL, sessionID string, req JoinRequest) (JoinResult, error) {
	if _, found, err := s.agents.GetByName(ctx, shardURL, sessionID, req.Name); err != nil {
		return JoinResult{}, err
	} else if found {
		return JoinResult{}, fmt.Errorf("%w: %q", ErrNameTaken, req.Name)
	}

	token, hash, err := newToken()
	if err != nil {
		return JoinResult{}, err
	}
	created, err := s.agents.Create(ctx, shardURL, mongodb.Agent{
		ID:        ulid.Make().String(),
		SessionID: sessionID,
		Name:      req.Name,
		Tool:      req.Tool,
		Role:      req.Role,
		Status:    "connected",
		TokenHash: hash,
	})
	if err != nil {
		return JoinResult{}, err
	}
	return JoinResult{SessionID: sessionID, AgentID: created.ID, Name: created.Name, Token: token}, nil
}

func (s service) Disconnect(ctx context.Context, sessionID, agentID string) error {
	shardURL, err := s.shards.ShardURLFor(ctx, sessionID)
	if err != nil {
		return err
	}
	return s.agents.SetStatus(ctx, shardURL, agentID, "disconnected")
}

func (s service) Send(ctx context.Context, sessionID, fromAgentID string, req SendRequest) (SendOutcome, error) {
	if len(req.Body) == 0 {
		return SendOutcome{}, errors.New("session: body is required")
	}
	if len(req.Body) > MaxBodyBytes {
		return SendOutcome{}, fmt.Errorf("session: body exceeds %d bytes", MaxBodyBytes)
	}
	shardURL, err := s.shards.ShardURLFor(ctx, sessionID)
	if err != nil {
		return SendOutcome{}, err
	}
	target, found, err := s.agents.GetByName(ctx, shardURL, sessionID, req.To)
	if err != nil {
		return SendOutcome{}, err
	}
	if !found {
		return SendOutcome{}, fmt.Errorf("session: no agent named %q in this session", req.To)
	}
	kind := req.Kind
	if kind == "" {
		kind = mongodb.DefaultMessageKind
	}
	created, err := s.messages.Create(ctx, shardURL, mongodb.Message{
		ID:          ulid.Make().String(),
		SessionID:   sessionID,
		FromAgentID: fromAgentID,
		ToAgentID:   target.ID,
		Kind:        kind,
		Priority:    req.Priority,
		ReplyTo:     req.ReplyTo,
		Body:        req.Body,
	})
	if err != nil {
		return SendOutcome{}, err
	}
	return SendOutcome{
		MessageID: created.ID, TargetAgentID: target.ID, State: created.State,
		Kind: created.Kind, Priority: created.Priority,
	}, nil
}

func (s service) ListAgents(ctx context.Context, sessionID string) ([]mongodb.Agent, error) {
	shardURL, err := s.shards.ShardURLFor(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	return s.agents.ListBySession(ctx, shardURL, sessionID)
}

func (s service) PendingFor(ctx context.Context, sessionID, agentID string) ([]mongodb.Message, error) {
	shardURL, err := s.shards.ShardURLFor(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	return s.messages.ListPending(ctx, shardURL, sessionID, agentID)
}

func (s service) MarkDispatched(ctx context.Context, sessionID, messageID string) error {
	shardURL, err := s.shards.ShardURLFor(ctx, sessionID)
	if err != nil {
		return err
	}
	return s.messages.SetState(ctx, shardURL, messageID, mongodb.MessageStateDispatched)
}

func (s service) Acknowledge(ctx context.Context, sessionID, agentID, messageID string) error {
	shardURL, err := s.shards.ShardURLFor(ctx, sessionID)
	if err != nil {
		return err
	}
	msg, found, err := s.messages.Get(ctx, shardURL, messageID)
	if err != nil {
		return err
	}
	if !found {
		return fmt.Errorf("session: message %q not found", messageID)
	}
	if msg.ToAgentID != agentID {
		return errors.New("session: only the recipient can acknowledge a message")
	}
	return s.messages.SetState(ctx, shardURL, messageID, mongodb.MessageStateAcknowledged)
}

func (s service) Wait(ctx context.Context, sessionID, agentID, messageID string, timeout time.Duration) (WaitOutcome, error) {
	shardURL, err := s.shards.ShardURLFor(ctx, sessionID)
	if err != nil {
		return WaitOutcome{}, err
	}
	msg, found, err := s.messages.Get(ctx, shardURL, messageID)
	if err != nil {
		return WaitOutcome{}, err
	}
	if !found {
		return WaitOutcome{}, fmt.Errorf("session: message %q not found", messageID)
	}
	if msg.FromAgentID != agentID && msg.ToAgentID != agentID {
		return WaitOutcome{}, errors.New("session: not authorized to wait on this message")
	}

	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(waitPollInterval)
	defer ticker.Stop()

	for {
		if reply, found, err := s.messages.FindReply(ctx, shardURL, sessionID, messageID); err != nil {
			return WaitOutcome{}, err
		} else if found {
			return WaitOutcome{State: msg.State, Reply: &reply}, nil
		}
		if current, found, err := s.messages.Get(ctx, shardURL, messageID); err != nil {
			return WaitOutcome{}, err
		} else if found && current.State == mongodb.MessageStateAcknowledged {
			return WaitOutcome{State: current.State}, nil
		}
		select {
		case <-waitCtx.Done():
			return WaitOutcome{TimedOut: true}, nil
		case <-ticker.C:
		}
	}
}

func (s service) Context(ctx context.Context, sessionID, agentID, forAgentName string, limit int) ([]mongodb.Message, error) {
	if forAgentName == "" {
		return nil, errors.New("session: agent name is required")
	}
	shardURL, err := s.shards.ShardURLFor(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	target, found, err := s.agents.GetByName(ctx, shardURL, sessionID, forAgentName)
	if err != nil {
		return nil, err
	}
	if !found {
		return nil, fmt.Errorf("session: no agent named %q in this session", forAgentName)
	}
	switch {
	case limit <= 0:
		limit = defaultContextLimit
	case limit > maxContextLimit:
		limit = maxContextLimit
	}
	return s.messages.ListForAgent(ctx, shardURL, sessionID, target.ID, limit)
}

func (s service) ensureCollections(ctx context.Context, shardURL string) error {
	if err := s.sessions.EnsureCollection(ctx, shardURL); err != nil {
		return err
	}
	if err := s.agents.EnsureCollection(ctx, shardURL); err != nil {
		return err
	}
	return s.messages.EnsureCollection(ctx, shardURL)
}
