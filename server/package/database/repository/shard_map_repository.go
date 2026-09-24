package repository

import (
	"context"
	"errors"

	"gorm.io/gorm"

	"github.com/thesahibnanda-max/relay/server/package/database/postgres"
)

// ShardMapRepository is CRUD over the session_shard_map control-plane table
// - the record of which MongoDB shard each global session's data lives on.
type ShardMapRepository interface {
	// Count returns how many sessions have been assigned a shard so far -
	// the round-robin selector's only piece of state, derived rather than
	// tracked separately, so it survives a restart with no extra bookkeeping.
	Count(ctx context.Context) (int64, error)
	// GetBySessionID returns the existing mapping for sessionID, if any.
	// found is false (with a zero-value SessionShardMap and nil error) when
	// no row exists yet - that is the normal "not yet assigned" case, not
	// an error.
	GetBySessionID(ctx context.Context, sessionID string) (row postgres.SessionShardMap, found bool, err error)
	// Create records a brand-new session -> shard assignment. Called at
	// most once per session (enforced by the table's unique index on
	// session_id), right after PickShardFor decides where a new session goes.
	Create(ctx context.Context, sessionID string, mongoURLID uint) (postgres.SessionShardMap, error)
}

type shardMapRepository struct {
	db postgres.Interface
}

func NewShardMapRepository(db postgres.Interface) (ShardMapRepository, error) {
	if db == nil {
		return nil, errors.New("repository: db is nil")
	}
	return shardMapRepository{db: db}, nil
}

func (r shardMapRepository) Count(ctx context.Context) (int64, error) {
	var count int64
	if err := r.db.DB().WithContext(ctx).Model(&postgres.SessionShardMap{}).Count(&count).Error; err != nil {
		return 0, err
	}
	return count, nil
}

func (r shardMapRepository) GetBySessionID(ctx context.Context, sessionID string) (postgres.SessionShardMap, bool, error) {
	var row postgres.SessionShardMap
	err := r.db.DB().WithContext(ctx).Where("session_id = ?", sessionID).First(&row).Error
	if errors.Is(err, gorm.ErrRecordNotFound) {
		return postgres.SessionShardMap{}, false, nil
	}
	if err != nil {
		return postgres.SessionShardMap{}, false, err
	}
	return row, true, nil
}

func (r shardMapRepository) Create(ctx context.Context, sessionID string, mongoURLID uint) (postgres.SessionShardMap, error) {
	row := postgres.SessionShardMap{SessionID: sessionID, MongoURLID: mongoURLID}
	if err := r.db.DB().WithContext(ctx).Create(&row).Error; err != nil {
		return postgres.SessionShardMap{}, err
	}
	return row, nil
}
