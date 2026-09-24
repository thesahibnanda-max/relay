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

// messageStateOrder is the forward-only progression SetState enforces -
// mirrors the local daemon's own compare-and-set Advance() semantics on a
// smaller state set (see mongodb.Message's doc comment for what's deferred).
var messageStateOrder = []string{
	mongodb.MessageStateQueued,
	mongodb.MessageStateDispatched,
	mongodb.MessageStateAcknowledged,
}

// statesAtOrBelow returns every state at or before state in
// messageStateOrder (inclusive, so re-asserting the current state is a safe
// no-op), or nil if state isn't a recognized state.
func statesAtOrBelow(state string) []string {
	for i, s := range messageStateOrder {
		if s == state {
			return messageStateOrder[:i+1]
		}
	}
	return nil
}

// MessageRepository is CRUD over the messages collection - shard-aware, same
// convention as SessionRepository/AgentRepository. Phase 1's delivery model
// is the 3-state queued/dispatched/acknowledged progression (see
// mongodb.Message's doc comment) - not the local daemon's full 9-state
// machine, which is explicitly deferred.
type MessageRepository interface {
	// EnsureCollection creates the messages collection (and its
	// {session_id,to_agent_id,state} / {session_id,reply_to} indexes) on the
	// named shard if either is missing.
	EnsureCollection(ctx context.Context, mongoURL string) error
	Create(ctx context.Context, mongoURL string, m mongodb.Message) (mongodb.Message, error)
	Get(ctx context.Context, mongoURL, messageID string) (message mongodb.Message, found bool, err error)
	// SetState advances a message's delivery state - callers are responsible
	// for only ever moving it forward (queued -> dispatched -> acknowledged);
	// this method itself is a plain unconditional set, matching the
	// package's "server decides, never trusts the client" convention while
	// keeping Phase 1's model simple.
	SetState(ctx context.Context, mongoURL, messageID, state string) error
	// ListPending returns every message addressed to toAgentID that hasn't
	// reached mongodb.MessageStateAcknowledged yet, oldest first - this is
	// what a (re)connecting agent gets replayed, making delivery at-least-once
	// across a crash/reconnect instead of a fire-and-forget push.
	ListPending(ctx context.Context, mongoURL, sessionID, toAgentID string) ([]mongodb.Message, error)
	// FindReply returns the oldest message that replied to replyToID, if any.
	FindReply(ctx context.Context, mongoURL, sessionID, replyToID string) (message mongodb.Message, found bool, err error)
	// ListForAgent returns the most recent messages (up to limit) either
	// to or from agentID, in chronological order - the data behind the
	// global session's relay_get_context equivalent.
	ListForAgent(ctx context.Context, mongoURL, sessionID, agentID string, limit int) ([]mongodb.Message, error)
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

func (r messageRepository) SetState(ctx context.Context, mongoURL, messageID, state string) error {
	db, err := r.pool.Database(mongoURL)
	if err != nil {
		return err
	}
	allowed := statesAtOrBelow(state)
	if allowed == nil {
		return fmt.Errorf("repository: unknown message state %q", state)
	}
	// Forward-only, idempotent compare-and-set: a message never moves
	// backward (the filter only matches a document whose current state is
	// at or before the target), and re-asserting the current state is a
	// safe no-op.
	filter := bson.M{"_id": messageID, "state": bson.M{"$in": allowed}}
	update := bson.M{"$set": bson.M{"state": state, "updated_at": time.Now().UTC()}}
	_, err = db.Collection(mongodb.CollectionMessages).UpdateOne(ctx, filter, update)
	return err
}

func (r messageRepository) ListPending(ctx context.Context, mongoURL, sessionID, toAgentID string) ([]mongodb.Message, error) {
	db, err := r.pool.Database(mongoURL)
	if err != nil {
		return nil, err
	}
	filter := bson.M{
		"session_id":  sessionID,
		"to_agent_id": toAgentID,
		"state":       bson.M{"$ne": mongodb.MessageStateAcknowledged},
	}
	cur, err := db.Collection(mongodb.CollectionMessages).Find(ctx, filter, options.Find().SetSort(bson.D{{Key: "created_at", Value: 1}}))
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
