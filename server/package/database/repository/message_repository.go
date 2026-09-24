package repository

import (
	"context"
	"errors"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"

	"github.com/thesahibnanda-max/relay/server/package/database/mongodb"
)

// MessageRepository is CRUD over the messages collection - shard-aware, same
// convention as SessionRepository/AgentRepository. This is the bare v1
// shape (a single Delivered flag): the local daemon's full delivery state
// machine is explicitly deferred to a follow-up plan.
type MessageRepository interface {
	EnsureCollection(ctx context.Context, mongoURL string) error
	Create(ctx context.Context, mongoURL string, m mongodb.Message) (mongodb.Message, error)
	MarkDelivered(ctx context.Context, mongoURL, messageID string) error
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
	return ensureCollection(ctx, db, mongodb.CollectionMessages)
}

func (r messageRepository) Create(ctx context.Context, mongoURL string, m mongodb.Message) (mongodb.Message, error) {
	db, err := r.pool.Database(mongoURL)
	if err != nil {
		return mongodb.Message{}, err
	}
	if m.Metadata == nil {
		m.Metadata = map[string]any{}
	}
	now := time.Now().UTC()
	m.CreatedAt, m.UpdatedAt = now, now
	if _, err := db.Collection(mongodb.CollectionMessages).InsertOne(ctx, m); err != nil {
		return mongodb.Message{}, err
	}
	return m, nil
}

func (r messageRepository) MarkDelivered(ctx context.Context, mongoURL, messageID string) error {
	db, err := r.pool.Database(mongoURL)
	if err != nil {
		return err
	}
	update := bson.M{"$set": bson.M{"delivered": true, "updated_at": time.Now().UTC()}}
	_, err = db.Collection(mongodb.CollectionMessages).UpdateOne(ctx, byID(messageID), update)
	return err
}
