package repository

import (
	"context"
	"errors"
	"fmt"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/thesahibnanda-max/relay/server/package/database/mongodb"
)

// nextStates is the forward-only transition table, mirroring the local
// daemon's own state machine exactly (see mongodb.Message's doc comment for
// the state list). held is a start-only state: nothing transitions into it -
// a hop-limited reply or an approve-inbound target's message is created
// directly in held, never moved there from something else.
var nextStates = map[string][]string{
	mongodb.MessageStateHeld: {
		mongodb.MessageStateQueued, mongodb.MessageStateRejected,
		mongodb.MessageStateExpired, mongodb.MessageStateUndeliverable,
	},
	mongodb.MessageStateQueued: {
		mongodb.MessageStateDispatched, mongodb.MessageStateInjected, mongodb.MessageStateAcknowledged,
		mongodb.MessageStateDone, mongodb.MessageStateExpired, mongodb.MessageStateUndeliverable, mongodb.MessageStateRejected,
	},
	mongodb.MessageStateDispatched: {
		mongodb.MessageStateInjected, mongodb.MessageStateAcknowledged, mongodb.MessageStateDone,
		mongodb.MessageStateExpired, mongodb.MessageStateUndeliverable, mongodb.MessageStateRejected,
	},
	mongodb.MessageStateInjected:     {mongodb.MessageStateAcknowledged, mongodb.MessageStateDone},
	mongodb.MessageStateAcknowledged: {mongodb.MessageStateDone},
}

// terminalStates never accept any further transition.
var terminalStates = []string{
	mongodb.MessageStateDone, mongodb.MessageStateRejected,
	mongodb.MessageStateExpired, mongodb.MessageStateUndeliverable,
}

// allowedPredecessors[target] is every state SetState may transition FROM to
// reach target, including target itself (re-asserting the current state is
// always a safe no-op) - the inverse of nextStates, computed once at init.
var allowedPredecessors = invertTransitions(nextStates)

func invertTransitions(next map[string][]string) map[string][]string {
	inv := map[string][]string{}
	seen := map[string]bool{}
	for from, tos := range next {
		seen[from] = true
		for _, to := range tos {
			seen[to] = true
			inv[to] = append(inv[to], from)
		}
	}
	for state := range seen {
		inv[state] = append(inv[state], state)
	}
	return inv
}

// MessageRepository is CRUD over the messages collection - shard-aware, same
// convention as SessionRepository/AgentRepository.
type MessageRepository interface {
	// EnsureCollection creates the messages collection (and its indexes) on
	// the named shard if either is missing.
	EnsureCollection(ctx context.Context, mongoURL string) error
	Create(ctx context.Context, mongoURL string, m mongodb.Message) (mongodb.Message, error)
	Get(ctx context.Context, mongoURL, messageID string) (message mongodb.Message, found bool, err error)
	// SetState advances a message's delivery state through the forward-only
	// transition table (see nextStates) - an illegal or stale transition is
	// a safe no-op, reported via matched=false, not an error.
	SetState(ctx context.Context, mongoURL, messageID, state string) (matched bool, err error)
	// ListPending returns every message actually in flight toward delivery
	// for toAgentID (queued/dispatched/injected - never held, which must
	// stay invisible until approved), priority then age order - what a
	// (re)connecting agent gets replayed, making delivery at-least-once
	// across a crash/reconnect instead of a fire-and-forget push.
	ListPending(ctx context.Context, mongoURL, sessionID, toAgentID string) ([]mongodb.Message, error)
	// FindReply returns the oldest message that replied to replyToID, if any.
	FindReply(ctx context.Context, mongoURL, sessionID, replyToID string) (message mongodb.Message, found bool, err error)
	// ListForAgent returns the most recent messages (up to limit) either
	// to or from agentID, in chronological order - the data behind the
	// global session's relay_get_context equivalent.
	ListForAgent(ctx context.Context, mongoURL, sessionID, agentID string, limit int) ([]mongodb.Message, error)
	// FindRecentDuplicate returns a live (non-terminal) message identical in
	// (from,to,kind,body) created at or after since, oldest first - what
	// makes a client's own retries safe to coalesce instead of duplicating.
	FindRecentDuplicate(ctx context.Context, mongoURL, sessionID, from, to, kind, body string, since time.Time) (message mongodb.Message, found bool, err error)
	// CountHeld reports how many messages are currently held for agentID -
	// what a notice{held} push tells the client.
	CountHeld(ctx context.Context, mongoURL, sessionID, agentID string) (int64, error)
	// ListHeld returns every message held for agentID, oldest first.
	ListHeld(ctx context.Context, mongoURL, sessionID, agentID string) ([]mongodb.Message, error)
	// ExpireDue transitions every non-terminal message whose ExpiresAt has
	// passed to expired, and returns the messages that were expired (so the
	// caller can notify each sender) - a TTL sweep, run periodically.
	ExpireDue(ctx context.Context, mongoURL string, now time.Time) ([]mongodb.Message, error)
	// FailPendingFor transitions every non-terminal message addressed to
	// agentID to undeliverable, and returns the messages that were failed
	// (so the caller can notify each sender) - called once an agent is
	// reaped as gone for good.
	FailPendingFor(ctx context.Context, mongoURL, agentID string) ([]mongodb.Message, error)
}

type messageRepository struct {
	pool mongodb.Interface
}

func NewMessageRepository(pool mongodb.Interface) (MessageRepository, error) {
	if pool == nil {
		return nil, errors.New("repository: pool is nil")
	}
	return messageRepository{pool: pool}, nil
}

func (r messageRepository) EnsureCollection(ctx context.Context, mongoURL string) error {
	db, err := r.pool.Database(mongoURL)
	if err != nil {
		return err
	}
	if err := ensureCollection(ctx, db, mongodb.CollectionMessages); err != nil {
		return err
	}
	col := db.Collection(mongodb.CollectionMessages)
	_, err = col.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "session_id", Value: 1}, {Key: "to_agent_id", Value: 1}, {Key: "state", Value: 1}}},
		{Keys: bson.D{{Key: "session_id", Value: 1}, {Key: "reply_to", Value: 1}}},
		{Keys: bson.D{{Key: "expires_at", Value: 1}}},
	})
	return err
}

func (r messageRepository) Create(ctx context.Context, mongoURL string, m mongodb.Message) (mongodb.Message, error) {
	db, err := r.pool.Database(mongoURL)
	if err != nil {
		return mongodb.Message{}, err
	}
	if m.Metadata == nil {
		m.Metadata = map[string]any{}
	}
	if m.State == "" {
		m.State = mongodb.MessageStateQueued
	}
	now := time.Now().UTC()
	m.CreatedAt, m.UpdatedAt = now, now
	if _, err := db.Collection(mongodb.CollectionMessages).InsertOne(ctx, m); err != nil {
		return mongodb.Message{}, err
	}
	return m, nil
}

func (r messageRepository) Get(ctx context.Context, mongoURL, messageID string) (mongodb.Message, bool, error) {
	db, err := r.pool.Database(mongoURL)
	if err != nil {
		return mongodb.Message{}, false, err
	}
	var m mongodb.Message
	err = db.Collection(mongodb.CollectionMessages).FindOne(ctx, byID(messageID)).Decode(&m)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return mongodb.Message{}, false, nil
	}
	if err != nil {
		return mongodb.Message{}, false, err
	}
	return m, true, nil
}

func (r messageRepository) SetState(ctx context.Context, mongoURL, messageID, state string) (bool, error) {
	db, err := r.pool.Database(mongoURL)
	if err != nil {
		return false, err
	}
	allowed, ok := allowedPredecessors[state]
	if !ok {
		return false, fmt.Errorf("repository: unknown message state %q", state)
	}
	filter := bson.M{"_id": messageID, "state": bson.M{"$in": allowed}}
	update := bson.M{"$set": bson.M{"state": state, "updated_at": time.Now().UTC()}}
	res, err := db.Collection(mongodb.CollectionMessages).UpdateOne(ctx, filter, update)
	if err != nil {
		return false, err
	}
	return res.MatchedCount > 0, nil
}

func (r messageRepository) ListPending(ctx context.Context, mongoURL, sessionID, toAgentID string) ([]mongodb.Message, error) {
	db, err := r.pool.Database(mongoURL)
	if err != nil {
		return nil, err
	}
	filter := bson.M{
		"session_id":  sessionID,
		"to_agent_id": toAgentID,
		"state": bson.M{"$in": bson.A{
			mongodb.MessageStateQueued, mongodb.MessageStateDispatched, mongodb.MessageStateInjected,
		}},
	}
	opts := options.Find().SetSort(bson.D{{Key: "priority", Value: 1}, {Key: "created_at", Value: 1}})
	cur, err := db.Collection(mongodb.CollectionMessages).Find(ctx, filter, opts)
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	var messages []mongodb.Message
	if err := cur.All(ctx, &messages); err != nil {
		return nil, err
	}
	return messages, nil
}

func (r messageRepository) FindReply(ctx context.Context, mongoURL, sessionID, replyToID string) (mongodb.Message, bool, error) {
	db, err := r.pool.Database(mongoURL)
	if err != nil {
		return mongodb.Message{}, false, err
	}
	filter := bson.M{"session_id": sessionID, "reply_to": replyToID}
	opts := options.FindOne().SetSort(bson.D{{Key: "created_at", Value: 1}})
	var m mongodb.Message
	err = db.Collection(mongodb.CollectionMessages).FindOne(ctx, filter, opts).Decode(&m)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return mongodb.Message{}, false, nil
	}
	if err != nil {
		return mongodb.Message{}, false, err
	}
	return m, true, nil
}

func (r messageRepository) ListForAgent(ctx context.Context, mongoURL, sessionID, agentID string, limit int) ([]mongodb.Message, error) {
	db, err := r.pool.Database(mongoURL)
	if err != nil {
		return nil, err
	}
	filter := bson.M{
		"session_id": sessionID,
		"$or":        bson.A{bson.M{"from_agent_id": agentID}, bson.M{"to_agent_id": agentID}},
	}
	// Newest-first with a limit, then reversed below, so a limit smaller than
	// the total history still returns the MOST RECENT messages, not the
	// oldest - matching the local daemon's own "tail" semantics.
	opts := options.Find().SetSort(bson.D{{Key: "created_at", Value: -1}}).SetLimit(int64(limit))
	cur, err := db.Collection(mongodb.CollectionMessages).Find(ctx, filter, opts)
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	var messages []mongodb.Message
	if err := cur.All(ctx, &messages); err != nil {
		return nil, err
	}
	for i, j := 0, len(messages)-1; i < j; i, j = i+1, j-1 {
		messages[i], messages[j] = messages[j], messages[i]
	}
	return messages, nil
}

func (r messageRepository) FindRecentDuplicate(ctx context.Context, mongoURL, sessionID, from, to, kind, body string, since time.Time) (mongodb.Message, bool, error) {
	db, err := r.pool.Database(mongoURL)
	if err != nil {
		return mongodb.Message{}, false, err
	}
	filter := bson.M{
		"session_id": sessionID, "from_agent_id": from, "to_agent_id": to,
		"kind": kind, "body": body,
		"created_at": bson.M{"$gte": since},
		"state":      bson.M{"$nin": terminalStates},
	}
	opts := options.FindOne().SetSort(bson.D{{Key: "created_at", Value: 1}})
	var m mongodb.Message
	err = db.Collection(mongodb.CollectionMessages).FindOne(ctx, filter, opts).Decode(&m)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return mongodb.Message{}, false, nil
	}
	if err != nil {
		return mongodb.Message{}, false, err
	}
	return m, true, nil
}

func (r messageRepository) CountHeld(ctx context.Context, mongoURL, sessionID, agentID string) (int64, error) {
	db, err := r.pool.Database(mongoURL)
	if err != nil {
		return 0, err
	}
	filter := bson.M{"session_id": sessionID, "to_agent_id": agentID, "state": mongodb.MessageStateHeld}
	return db.Collection(mongodb.CollectionMessages).CountDocuments(ctx, filter)
}

func (r messageRepository) ListHeld(ctx context.Context, mongoURL, sessionID, agentID string) ([]mongodb.Message, error) {
	db, err := r.pool.Database(mongoURL)
	if err != nil {
		return nil, err
	}
	filter := bson.M{"session_id": sessionID, "to_agent_id": agentID, "state": mongodb.MessageStateHeld}
	opts := options.Find().SetSort(bson.D{{Key: "created_at", Value: 1}})
	cur, err := db.Collection(mongodb.CollectionMessages).Find(ctx, filter, opts)
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	var held []mongodb.Message
	if err := cur.All(ctx, &held); err != nil {
		return nil, err
	}
	return held, nil
}

func (r messageRepository) ExpireDue(ctx context.Context, mongoURL string, now time.Time) ([]mongodb.Message, error) {
	db, err := r.pool.Database(mongoURL)
	if err != nil {
		return nil, err
	}
	filter := bson.M{
		"state":      bson.M{"$nin": terminalStates},
		"expires_at": bson.M{"$lte": now},
	}
	return r.findAndTransition(ctx, db, filter, mongodb.MessageStateExpired, now)
}

func (r messageRepository) FailPendingFor(ctx context.Context, mongoURL, agentID string) ([]mongodb.Message, error) {
	db, err := r.pool.Database(mongoURL)
	if err != nil {
		return nil, err
	}
	filter := bson.M{"to_agent_id": agentID, "state": bson.M{"$nin": terminalStates}}
	return r.findAndTransition(ctx, db, filter, mongodb.MessageStateUndeliverable, time.Now().UTC())
}

// findAndTransition finds every message matching filter, bulk-transitions
// them all to state, and returns the pre-transition documents so the caller
// can build sender notifications from them. The two-step find-then-update
// (rather than a single atomic op) accepts a small race at this codebase's
// scale: a message that changes state in the moment between the two steps
// is simply picked up by the next sweep instead.
func (r messageRepository) findAndTransition(ctx context.Context, db *mongo.Database, filter bson.M, state string, now time.Time) ([]mongodb.Message, error) {
	col := db.Collection(mongodb.CollectionMessages)
	cur, err := col.Find(ctx, filter)
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	var due []mongodb.Message
	if err := cur.All(ctx, &due); err != nil {
		return nil, err
	}
	if len(due) == 0 {
		return nil, nil
	}
	ids := make([]string, len(due))
	for i, m := range due {
		ids[i] = m.ID
	}
	update := bson.M{"$set": bson.M{"state": state, "updated_at": now}}
	if _, err := col.UpdateMany(ctx, bson.M{"_id": bson.M{"$in": ids}}, update); err != nil {
		return nil, err
	}
	return due, nil
}
