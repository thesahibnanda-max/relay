// Package session is the business logic tying sharding and the
// Mongo/Postgres repositories together: creating global sessions, joining
// (or resuming) an agent into one, and the bare v1 send/list_agents
// operations. This is deliberately the "skeleton" scope - no priorities,
// hop-limits, rate-limits, or delivery state machine yet; see the project
// plan for what's explicitly deferred.
package session

import (
	"context"
	"errors"
	"fmt"

	"github.com/oklog/ulid/v2"

	"github.com/thesahibnanda-max/relay/server/package/database/mongodb"
	"github.com/thesahibnanda-max/relay/server/package/database/repository"
	"github.com/thesahibnanda-max/relay/server/package/sharding"
)

// SessionNew is the sentinel Hello.Session value meaning "create a fresh
// global session," mirroring the local daemon's proto.SessionNew.
const SessionNew = "NEW"

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

// Interface is the session service every WebSocket connection's Hello/Send/
// ListAgents handling goes through.
type Interface interface {
	Join(ctx context.Context, req JoinRequest) (JoinResult, error)
	// Send returns both the new message's id and the resolved target
	// agent's id, so a caller (the WS hub) that keeps its own live
	// connection registry keyed by agent id can push a delivery without a
	// second name lookup.
	Send(ctx context.Context, sessionID, fromAgentID, toName, body string) (messageID, targetAgentID string, err error)
	ListAgents(ctx context.Context, sessionID string) ([]mongodb.Agent, error)
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
		return JoinResult{}, fmt.Errorf("session: %q not found", sessionID)
	}

	if req.Token != "" {
		return s.resume(ctx, shardURL, sessionID, req)
	}
	return s.register(ctx, shardURL, sessionID, req)
}

// resume looks up an existing agent by name and admits it only if the
// presented token's hash matches what was stored at registration.
func (s service) resume(ctx context.Context, shardURL, sessionID string, req JoinRequest) (JoinResult, error) {
	existing, found, err := s.agents.GetByName(ctx, shardURL, sessionID, req.Name)
	if err != nil {
		return JoinResult{}, err
	}
	if !found || existing.TokenHash != hashToken(req.Token) {
		return JoinResult{}, errors.New("session: bad resume token")
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
		return JoinResult{}, fmt.Errorf("session: name %q is already taken in this session", req.Name)
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

func (s service) Send(ctx context.Context, sessionID, fromAgentID, toName, body string) (string, string, error) {
	shardURL, err := s.shards.ShardURLFor(ctx, sessionID)
	if err != nil {
		return "", "", err
	}
	target, found, err := s.agents.GetByName(ctx, shardURL, sessionID, toName)
	if err != nil {
		return "", "", err
	}
	if !found {
		return "", "", fmt.Errorf("session: no agent named %q in this session", toName)
	}
	created, err := s.messages.Create(ctx, shardURL, mongodb.Message{
		ID:          ulid.Make().String(),
		SessionID:   sessionID,
		FromAgentID: fromAgentID,
		ToAgentID:   target.ID,
		Body:        body,
	})
	if err != nil {
		return "", "", err
	}
	return created.ID, target.ID, nil
}

func (s service) ListAgents(ctx context.Context, sessionID string) ([]mongodb.Agent, error) {
	shardURL, err := s.shards.ShardURLFor(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	return s.agents.ListBySession(ctx, shardURL, sessionID)
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
