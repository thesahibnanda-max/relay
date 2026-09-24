// Package session is the business logic tying sharding and the
// Mongo/Postgres repositories together: creating global sessions, joining
// (or resuming) an agent into one, and every messaging operation - send,
// list_agents, wait, get_context, approve/reject, message-state and
// delivery-state reporting, and the periodic TTL/disconnect sweep. This is
// full local-daemon parity for messaging semantics; the one deliberate,
// permanent difference is relay_get_context meaning "recent message
// history" rather than a terminal transcript, since this server never
// receives raw terminal bytes from any agent.
package session

import (
	"context"
	"crypto/subtle"
	"errors"
	"fmt"
	"time"

	"github.com/oklog/ulid/v2"

	"github.com/thesahibnanda-max/relay/server/package/config"
	"github.com/thesahibnanda-max/relay/server/package/database/mongodb"
	"github.com/thesahibnanda-max/relay/server/package/database/repository"
	"github.com/thesahibnanda-max/relay/server/package/sharding"
)

// SessionNew is the sentinel Hello.Session value meaning "create a fresh
// global session," mirroring the local daemon's proto.SessionNew.
const SessionNew = "NEW"

// MaxBodyBytes mirrors the local daemon's own message body cap.
const MaxBodyBytes = 32 << 10

// MaxHops mirrors the local daemon's own reply-chain limit: a chain is held
// again for human approval every time it crosses a multiple of this many hops.
const MaxHops = 8

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
	// Role policy, checked again on every join/resume in case the role
	// changed between runs (mirrors the local daemon's own Hello fields).
	ApproveInbound bool
	CanInterrupt   bool
	CanBroadcast   bool
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
	// PendingFor returns everything actually in flight toward delivery for
	// agentID (never held, which must stay invisible until approved) -
	// what a (re)connecting agent gets replayed.
	PendingFor(ctx context.Context, sessionID, agentID string) ([]mongodb.Message, error)
	// MarkDispatched records that a deliver frame was successfully pushed to
	// the target's live connection - not confirmation it was received.
	MarkDispatched(ctx context.Context, sessionID, messageID string) error
	// Acknowledge records that the recipient's transport confirmed a
	// delivery (the wire `ack` frame, auto-sent on every deliver) - a thin,
	// authorization-checked wrapper around ReportState.
	Acknowledge(ctx context.Context, sessionID, agentID, messageID string) error
	// ReportState records what the recipient's own local scheduler actually
	// did with a message (injected/acknowledged/done) - the msg_state RPC.
	// Only the message's recipient may report on it; an out-of-order or
	// duplicate report is a safe no-op via the same transition table
	// SetState already enforces.
	ReportState(ctx context.Context, sessionID, agentID, messageID, state string) error
	// Wait blocks (up to timeout) for a reply to messageID or for it to
	// reach mongodb.MessageStateAcknowledged. Only the message's sender or
	// recipient may wait on it.
	Wait(ctx context.Context, sessionID, agentID, messageID string, timeout time.Duration) (WaitOutcome, error)
	// Context returns forAgentName's recent message history in this
	// session (up to limit, chronological order) - the data behind the
	// global session's relay_get_context equivalent.
	Context(ctx context.Context, sessionID, agentID, forAgentName string, limit int) ([]mongodb.Message, error)
	// ListHeld returns every message currently held for agentID, oldest
	// first - always scoped to the caller's own mail, secure by
	// construction (no id parameter to ask for someone else's).
	ListHeld(ctx context.Context, sessionID, agentID string) ([]mongodb.Message, error)
	// Approve releases a held message (messageID == "" picks the oldest
	// held for agentID) back to normal delivery and returns it, so the
	// caller (ws/hub.go) can push it live if the recipient is online.
	Approve(ctx context.Context, sessionID, agentID, messageID string) (mongodb.Message, error)
	// Reject permanently refuses a held message and notifies its sender.
	Reject(ctx context.Context, sessionID, agentID, messageID string) (mongodb.Message, error)
	// Sweep runs one TTL-expiry + disconnect-reaper pass directly against
	// shardURL (not resolved via a session id: a sweep scans an entire
	// shard - every session living there - not one session at a time).
	// Called periodically per configured shard from app.Serve's OnStart.
	Sweep(ctx context.Context, shardURL string) error
}

type service struct {
	cfg      config.Config
	shards   sharding.Interface
	sessions repository.SessionRepository
	agents   repository.AgentRepository
	messages repository.MessageRepository
}

func New(
	cfg config.Config,
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
	return service{cfg: cfg, shards: shards, sessions: sessions, agents: agents, messages: messages}, nil
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
// identity isn't already live elsewhere. Its role policy is refreshed from
// this Hello in case the role changed between runs.
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
	if err := s.agents.UpdatePolicy(ctx, shardURL, existing.ID, "connected", req.ApproveInbound, req.CanInterrupt, req.CanBroadcast); err != nil {
		return JoinResult{}, err
	}
	return JoinResult{SessionID: sessionID, AgentID: existing.ID, Name: existing.Name, Resumed: true}, nil
}

// register creates a brand-new agent; an explicit name that's already taken
// in this session is a hard error - no local-daemon-style auto-retry-with-a-
// different-name.
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
		ID:             ulid.Make().String(),
		SessionID:      sessionID,
		Name:           req.Name,
		Tool:           req.Tool,
		Role:           req.Role,
		Status:         "connected",
		TokenHash:      hash,
		ApproveInbound: req.ApproveInbound,
		CanInterrupt:   req.CanInterrupt,
		CanBroadcast:   req.CanBroadcast,
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
	sender, found, err := s.agents.Get(ctx, shardURL, fromAgentID)
	if err != nil {
		return SendOutcome{}, err
	}
	if !found {
		return SendOutcome{}, errors.New("session: sending agent not found")
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
	priority := req.Priority
	if priority == 0 && !sender.CanInterrupt {
		priority = 1 // P0 needs CanInterrupt; otherwise silently downgraded, mirroring the local daemon exactly
	}

	since := time.Now().UTC().Add(-s.cfg.DedupWindow)
	if dup, found, err := s.messages.FindRecentDuplicate(ctx, shardURL, sessionID, fromAgentID, target.ID, kind, req.Body, since); err != nil {
		return SendOutcome{}, err
	} else if found {
		return SendOutcome{
			MessageID: dup.ID, TargetAgentID: target.ID, State: dup.State,
			Kind: dup.Kind, Priority: dup.Priority, Note: "duplicate of a recent identical message",
		}, nil
	}

	thread, hops := "", 0
	if req.ReplyTo != "" {
		if parent, found, err := s.messages.Get(ctx, shardURL, req.ReplyTo); err != nil {
			return SendOutcome{}, err
		} else if found {
			thread, hops = parent.Thread, parent.Hops+1
			// A reply is definitive proof the parent was handled, regardless
			// of whatever state it was in - mirrors the local daemon's own
			// "answered by <id>" rule.
			_, _ = s.messages.SetState(ctx, shardURL, parent.ID, mongodb.MessageStateDone)
		}
	}

	id := ulid.Make().String()
	if thread == "" {
		thread = id // the root of its own thread
	}
	state, detail := mongodb.MessageStateQueued, ""
	switch {
	case hops >= MaxHops && hops%MaxHops == 0:
		state = mongodb.MessageStateHeld
		detail = fmt.Sprintf("hop limit: this reply chain is %d messages deep; a human must approve it to continue", hops)
	case target.ApproveInbound:
		state = mongodb.MessageStateHeld
		detail = "awaiting human approval (target runs with --approve-inbound)"
	}

	now := time.Now().UTC()
	created, err := s.messages.Create(ctx, shardURL, mongodb.Message{
		ID: id, SessionID: sessionID, FromAgentID: fromAgentID, ToAgentID: target.ID,
		Kind: kind, Priority: priority, Thread: thread, ReplyTo: req.ReplyTo, Body: req.Body,
		Hops: hops, State: state, Detail: detail, ExpiresAt: now.Add(s.cfg.MessageTTL),
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
	_, err = s.messages.SetState(ctx, shardURL, messageID, mongodb.MessageStateDispatched)
	return err
}

func (s service) Acknowledge(ctx context.Context, sessionID, agentID, messageID string) error {
	return s.ReportState(ctx, sessionID, agentID, messageID, mongodb.MessageStateAcknowledged)
}

func (s service) ReportState(ctx context.Context, sessionID, agentID, messageID, state string) error {
	switch state {
	case mongodb.MessageStateInjected, mongodb.MessageStateAcknowledged, mongodb.MessageStateDone:
	default:
		return fmt.Errorf("session: cannot report state %q", state)
	}
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
		return errors.New("session: only the recipient can report a message's state")
	}
	_, err = s.messages.SetState(ctx, shardURL, messageID, state)
	return err
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

func (s service) ListHeld(ctx context.Context, sessionID, agentID string) ([]mongodb.Message, error) {
	shardURL, err := s.shards.ShardURLFor(ctx, sessionID)
	if err != nil {
		return nil, err
	}
	return s.messages.ListHeld(ctx, shardURL, sessionID, agentID)
}

func (s service) Approve(ctx context.Context, sessionID, agentID, messageID string) (mongodb.Message, error) {
	shardURL, err := s.shards.ShardURLFor(ctx, sessionID)
	if err != nil {
		return mongodb.Message{}, err
	}
	msg, err := s.resolveHeld(ctx, shardURL, sessionID, agentID, messageID)
	if err != nil {
		return mongodb.Message{}, err
	}
	if _, err := s.messages.SetState(ctx, shardURL, msg.ID, mongodb.MessageStateQueued); err != nil {
		return mongodb.Message{}, err
	}
	msg.State = mongodb.MessageStateQueued
	return msg, nil
}

func (s service) Reject(ctx context.Context, sessionID, agentID, messageID string) (mongodb.Message, error) {
	shardURL, err := s.shards.ShardURLFor(ctx, sessionID)
	if err != nil {
		return mongodb.Message{}, err
	}
	msg, err := s.resolveHeld(ctx, shardURL, sessionID, agentID, messageID)
	if err != nil {
		return mongodb.Message{}, err
	}
	if _, err := s.messages.SetState(ctx, shardURL, msg.ID, mongodb.MessageStateRejected); err != nil {
		return mongodb.Message{}, err
	}
	msg.State = mongodb.MessageStateRejected
	if err := s.notifySender(ctx, shardURL, msg, "was rejected by a human"); err != nil {
		return mongodb.Message{}, err
	}
	return msg, nil
}

// resolveHeld looks up the message to approve/reject: an empty messageID
// picks the oldest currently held for agentID (mirrors the local daemon's
// own ApproveArgs{ID omitempty} semantics), otherwise the named message must
// actually be held for agentID.
func (s service) resolveHeld(ctx context.Context, shardURL, sessionID, agentID, messageID string) (mongodb.Message, error) {
	if messageID == "" {
		held, err := s.messages.ListHeld(ctx, shardURL, sessionID, agentID)
		if err != nil {
			return mongodb.Message{}, err
		}
		if len(held) == 0 {
			return mongodb.Message{}, errors.New("session: nothing is held for you")
		}
		return held[0], nil
	}
	msg, found, err := s.messages.Get(ctx, shardURL, messageID)
	if err != nil {
		return mongodb.Message{}, err
	}
	if !found || msg.ToAgentID != agentID || msg.State != mongodb.MessageStateHeld {
		return mongodb.Message{}, fmt.Errorf("session: no held message %q for you", messageID)
	}
	return msg, nil
}

// Sweep is a periodic maintenance pass, mirroring the local daemon's own
// sweep exactly: expire due messages, reap agents gone for good, fail
// whatever was still pending for them, and notify every affected sender.
func (s service) Sweep(ctx context.Context, shardURL string) error {
	now := time.Now().UTC()
	expired, err := s.messages.ExpireDue(ctx, shardURL, now)
	if err != nil {
		return err
	}
	for _, m := range expired {
		if err := s.notifySender(ctx, shardURL, m, "expired before it was delivered"); err != nil {
			return err
		}
	}

	cutoff := now.Add(-s.cfg.DisconnectGrace)
	gone, err := s.agents.ReapGone(ctx, shardURL, cutoff)
	if err != nil {
		return err
	}
	for _, a := range gone {
		failed, err := s.messages.FailPendingFor(ctx, shardURL, a.ID)
		if err != nil {
			return err
		}
		for _, m := range failed {
			if err := s.notifySender(ctx, shardURL, m, fmt.Sprintf("could not be delivered: %s is gone for good", a.Name)); err != nil {
				return err
			}
		}
	}
	return nil
}

// notifySender creates a synthetic notify-kind message back to a message's
// original sender explaining what happened to it - mirroring the local
// daemon's own rule exactly: never notices about notices, and never for a
// message with no real sender (server-generated messages have no FromAgentID).
func (s service) notifySender(ctx context.Context, shardURL string, original mongodb.Message, reason string) error {
	if original.Kind == "notify" || original.FromAgentID == "" {
		return nil
	}
	target := original.ToAgentID
	if agent, found, err := s.agents.Get(ctx, shardURL, original.ToAgentID); err == nil && found {
		target = agent.Name
	}
	now := time.Now().UTC()
	_, err := s.messages.Create(ctx, shardURL, mongodb.Message{
		ID:        ulid.Make().String(),
		SessionID: original.SessionID,
		ToAgentID: original.FromAgentID,
		Kind:      "notify",
		Priority:  mongodb.DefaultMessagePriority,
		Thread:    original.Thread,
		Body:      fmt.Sprintf("your message to %s %s (id %s)", target, reason, original.ID),
		ExpiresAt: now.Add(s.cfg.MessageTTL),
	})
	return err
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
