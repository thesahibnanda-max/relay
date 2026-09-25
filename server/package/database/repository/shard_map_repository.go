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
	// DeleteBySessionID removes sessionID's shard assignment row. Called
	// AFTER its Mongo data is already fully deleted (see session.Interface's
	// DeleteSessionsOlderThan) - Postgres and MongoDB are two separate
	// databases with no shared transaction, so this is a deliberate
	// best-effort second step. If it fails after the Mongo delete already
	// committed, the result is a harmless orphaned shard-map row pointing at
	// nothing - cheap to notice and clean up later. The reverse ordering is
	// never acceptable: a missing shard-map row must never be allowed to
	// strand real, undeleted session data with no record of where it lives.
	DeleteBySessionID(ctx context.Context, sessionID string) error
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

func (r shardMapRepository) DeleteBySessionID(ctx context.Context, sessionID string) error {
	return r.db.DB().WithContext(ctx).Where("session_id = ?", sessionID).Delete(&postgres.SessionShardMap{}).Error
}
