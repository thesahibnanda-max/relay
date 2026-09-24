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
	// UpdatePolicy refreshes an agent's role-derived policy fields (checked
	// again on every resume, in case the role changed between runs) and its
	// status, in one write.
	UpdatePolicy(ctx context.Context, mongoURL, agentID, status string, approveInbound, canInterrupt, canBroadcast bool) error
	// MarkAllDisconnected force-marks every currently-"connected" agent on
	// this shard as "disconnected" - called once per shard on server
	// startup, mirroring the local daemon's own rule: a previous process
	// owned any connection that was live before restart, so it's definitely
	// gone now.
	MarkAllDisconnected(ctx context.Context, mongoURL string) error
	// ReapGone marks every agent disconnected at or before cutoff as
	// "exited" (gone for good, distinct from a merely offline agent that
	// might still reconnect) and returns the reaped agents, so the caller
	// can fail their pending mail.
	ReapGone(ctx context.Context, mongoURL string, cutoff time.Time) ([]mongodb.Agent, error)
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
	col := db.Collection(mongodb.CollectionAgents)
	_, err = col.Indexes().CreateMany(ctx, []mongo.IndexModel{
		{Keys: bson.D{{Key: "session_id", Value: 1}, {Key: "name", Value: 1}}, Options: options.Index().SetUnique(true)},
		{Keys: bson.D{{Key: "status", Value: 1}, {Key: "updated_at", Value: 1}}}, // the disconnect-reaper sweep's scan
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

func (r agentRepository) MarkAllDisconnected(ctx context.Context, mongoURL string) error {
	db, err := r.pool.Database(mongoURL)
	if err != nil {
		return err
	}
	filter := bson.M{"status": "connected"}
	update := bson.M{"$set": bson.M{"status": "disconnected", "updated_at": time.Now().UTC()}}
	_, err = db.Collection(mongodb.CollectionAgents).UpdateMany(ctx, filter, update)
	return err
}

func (r agentRepository) UpdatePolicy(ctx context.Context, mongoURL, agentID, status string, approveInbound, canInterrupt, canBroadcast bool) error {
	db, err := r.pool.Database(mongoURL)
	if err != nil {
		return err
	}
	update := bson.M{"$set": bson.M{
		"status": status, "approve_inbound": approveInbound, "can_interrupt": canInterrupt,
		"can_broadcast": canBroadcast, "updated_at": time.Now().UTC(),
	}}
	_, err = db.Collection(mongodb.CollectionAgents).UpdateOne(ctx, byID(agentID), update)
	return err
}

func (r agentRepository) ReapGone(ctx context.Context, mongoURL string, cutoff time.Time) ([]mongodb.Agent, error) {
	db, err := r.pool.Database(mongoURL)
	if err != nil {
		return nil, err
	}
	col := db.Collection(mongodb.CollectionAgents)
	filter := bson.M{"status": "disconnected", "updated_at": bson.M{"$lte": cutoff}}
	cur, err := col.Find(ctx, filter)
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	var gone []mongodb.Agent
	if err := cur.All(ctx, &gone); err != nil {
		return nil, err
	}
	if len(gone) == 0 {
		return nil, nil
	}
	ids := make([]string, len(gone))
	for i, a := range gone {
		ids[i] = a.ID
	}
	update := bson.M{"$set": bson.M{"status": "exited", "updated_at": time.Now().UTC()}}
	if _, err := col.UpdateMany(ctx, bson.M{"_id": bson.M{"$in": ids}}, update); err != nil {
		return nil, err
	}
	return gone, nil
}
