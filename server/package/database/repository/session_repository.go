package repository

import (
	"context"
	"errors"
	"time"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"

	"github.com/thesahibnanda-max/relay/server/package/database/mongodb"
)

// SessionRepository is CRUD over the sessions collection. Every method is
// shard-aware: mongoURL names which configured MongoDB shard to operate on,
// resolved by the caller (package sharding) before reaching here - this
// repository never decides which shard anything belongs to.
type SessionRepository interface {
	// EnsureCollection creates the sessions collection on the named shard if
	// it doesn't already exist - "tables get made on their own if not exists",
	// done explicitly rather than left as an implicit side effect of the
	// first insert.
	EnsureCollection(ctx context.Context, mongoURL string) error
	Create(ctx context.Context, mongoURL string, s mongodb.Session) (mongodb.Session, error)
	Get(ctx context.Context, mongoURL, sessionID string) (session mongodb.Session, found bool, err error)
	// Delete removes the session document itself. Not atomic on its own with
	// AgentRepository.DeleteBySession/MessageRepository.DeleteBySession - the
	// caller wraps all three in one mongodb.Interface.WithTransaction call
	// (see session.Interface's DeleteSessionsOlderThan).
	Delete(ctx context.Context, mongoURL, sessionID string) error
	// ListOlderThan returns every session on mongoURL whose own UpdatedAt is
	// before cutoff - a cheap first filter, not the authoritative idle check
	// on its own: nothing currently refreshes a session document's UpdatedAt
	// after creation (see Create), so a long-lived-but-active session would
	// look "old" by this alone. A caller MUST also check each candidate's own
	// agents (AgentRepository.ListBySession) before deleting anything - see
	// session.Interface's DeleteSessionsOlderThan for the two-step contract
	// this feeds.
	ListOlderThan(ctx context.Context, mongoURL string, cutoff time.Time) ([]mongodb.Session, error)
}

type sessionRepository struct {
	pool mongodb.Interface
}

func NewSessionRepository(pool mongodb.Interface) (SessionRepository, error) {
	if pool == nil {
		return nil, errors.New("repository: pool is nil")
	}
	return sessionRepository{pool: pool}, nil
}

func (r sessionRepository) EnsureCollection(ctx context.Context, mongoURL string) error {
	db, err := r.pool.Database(mongoURL)
	if err != nil {
		return err
	}
	return ensureCollection(ctx, db, mongodb.CollectionSessions)
}

func (r sessionRepository) Create(ctx context.Context, mongoURL string, s mongodb.Session) (mongodb.Session, error) {
	db, err := r.pool.Database(mongoURL)
	if err != nil {
		return mongodb.Session{}, err
	}
	if s.Metadata == nil {
		s.Metadata = map[string]any{}
	}
	now := time.Now().UTC()
	s.CreatedAt, s.UpdatedAt = now, now
	if _, err := db.Collection(mongodb.CollectionSessions).InsertOne(ctx, s); err != nil {
		return mongodb.Session{}, err
	}
	return s, nil
}

func (r sessionRepository) Get(ctx context.Context, mongoURL, sessionID string) (mongodb.Session, bool, error) {
	db, err := r.pool.Database(mongoURL)
	if err != nil {
		return mongodb.Session{}, false, err
	}
	var s mongodb.Session
	err = db.Collection(mongodb.CollectionSessions).FindOne(ctx, byID(sessionID)).Decode(&s)
	if errors.Is(err, mongo.ErrNoDocuments) {
		return mongodb.Session{}, false, nil
	}
	if err != nil {
		return mongodb.Session{}, false, err
	}
	return s, true, nil
}

func (r sessionRepository) Delete(ctx context.Context, mongoURL, sessionID string) error {
	db, err := r.pool.Database(mongoURL)
	if err != nil {
		return err
	}
	_, err = db.Collection(mongodb.CollectionSessions).DeleteOne(ctx, byID(sessionID))
	return err
}

func (r sessionRepository) ListOlderThan(ctx context.Context, mongoURL string, cutoff time.Time) ([]mongodb.Session, error) {
	db, err := r.pool.Database(mongoURL)
	if err != nil {
		return nil, err
	}
	cur, err := db.Collection(mongodb.CollectionSessions).Find(ctx, bson.M{"updated_at": bson.M{"$lt": cutoff}})
	if err != nil {
		return nil, err
	}
	defer cur.Close(ctx)
	var out []mongodb.Session
	if err := cur.All(ctx, &out); err != nil {
		return nil, err
	}
	return out, nil
}
