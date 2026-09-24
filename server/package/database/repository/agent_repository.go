package repository

import (
	"context"
	"errors"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
	"go.mongodb.org/mongo-driver/v2/mongo/options"

	"github.com/thesahibnanda-max/relay/server/package/database/mongodb"
)

// AgentRepository is CRUD over the agents collection - shard-aware, same
// convention as SessionRepository.
type AgentRepository interface {
	// EnsureCollection creates the agents collection (and its
	// session_id+name uniqueness index) on the named shard if either is
	// missing.
	EnsureCollection(ctx context.Context, mongoURL string) error
	Create(ctx context.Context, mongoURL string, a mongodb.Agent) (mongodb.Agent, error)
	Get(ctx context.Context, mongoURL, agentID string) (agent mongodb.Agent, found bool, err error)
	GetByName(ctx context.Context, mongoURL, sessionID, name string) (agent mongodb.Agent, found bool, err error)
	ListBySession(ctx context.Context, mongoURL, sessionID string) ([]mongodb.Agent, error)
	SetStatus(ctx context.Context, mongoURL, agentID, status string) error
}

type agentRepository struct {
	pool mongodb.Interface
}

func NewAgentRepository(pool mongodb.Interface) (AgentRepository, error) {
	if pool == nil {
		return nil, errors.New("repository: pool is nil")
	}
	return agentRepository{pool: pool}, nil
}

func (r agentRepository) EnsureCollection(ctx context.Context, mongoURL string) error {
	db, err := r.pool.Database(mongoURL)
	if err != nil {
		return err
	}
	if err := ensureCollection(ctx, db, mongodb.CollectionAgents); err != nil {
		return err
	}
	_, err = db.Collection(mongodb.CollectionAgents).Indexes().CreateOne(ctx, mongo.IndexModel{
		Keys:    bson.D{{Key: "session_id", Value: 1}, {Key: "name", Value: 1}},
		Options: options.Index().SetUnique(true),
	})
	return err
}

func (r agentRepository) Create(ctx context.Context, mongoURL string, a mongodb.Agent) (mongodb.Agent, error) {
	db, err := r.pool.Database(mongoURL)
	if err != nil {
		return mongodb.Agent{}, err
	}
	if a.Metadata == nil {
		a.Metadata = map[string]any{}
	}
	now := time.Now().UTC()
	a.CreatedAt, a.UpdatedAt = now, now
	if _, err := db.Collection(mongodb.CollectionAgents).InsertOne(ctx, a); err != nil {
		return mongodb.Agent{}, err
	}
	return a, nil
}

func (r agentRepository) Get(ctx context.Context, mongoURL, agentID string) (mongodb.Agent, bool, error) {
	db, err := r.pool.Database(mongoURL)
	if err != nil {
		return mongodb.Agent{}, false, err
	}
	var a mongodb.Agent
	err = db.Collection(mongodb.CollectionAgents).FindOne(ctx, byID(agentID)).Decode(&a)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return mongodb.Agent{}, false, nil
	}
	if err != nil {
		return mongodb.Agent{}, false, err
	}
	return a, true, nil
}

func (r agentRepository) GetByName(ctx context.Context, mongoURL, sessionID, name string) (mongodb.Agent, bool, error) {
	db, err := r.pool.Database(mongoURL)
	if err != nil {
		return mongodb.Agent{}, false, err
	}
	var a mongodb.Agent
	filter := bson.M{"session_id": sessionID, "name": name}
	err = db.Collection(mongodb.CollectionAgents).FindOne(ctx, filter).Decode(&a)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return mongodb.Agent{}, false, nil
	}
	if err != nil {
		return mongodb.Agent{}, false, err
	}
	return a, true, nil
}

func (r agentRepository) ListBySession(ctx context.Context, mongoURL, sessionID string) ([]mongodb.Agent, error) {
	db, err := r.pool.Database(mongoURL)
	if err != nil {
		return nil, err
	}
	cur, err := db.Collection(mongodb.CollectionAgents).Find(ctx, bson.M{"session_id": sessionID})
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	var agents []mongodb.Agent
	if err := cur.All(ctx, &agents); err != nil {
		return nil, err
	}
	return agents, nil
}

func (r agentRepository) SetStatus(ctx context.Context, mongoURL, agentID, status string) error {
	db, err := r.pool.Database(mongoURL)
	if err != nil {
		return err
	}
	update := bson.M{"$set": bson.M{"status": status, "updated_at": time.Now().UTC()}}
	_, err = db.Collection(mongodb.CollectionAgents).UpdateOne(ctx, byID(agentID), update)
	return err
}
