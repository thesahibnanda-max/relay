// Package repository holds every data-access type in this module. Each file
// is one repository, all following the same New(deps...) (Interface, error)
// shape described in the project plan.
package repository

import (
	"context"
	"errors"

	"gorm.io/gorm"

	"github.com/thesahibnanda-max/relay/server/package/database/postgres"
)

// MongoURLRepository is CRUD over the mongo_db_urls control-plane table.
type MongoURLRepository interface {
	// ListOrderedByID returns every configured shard, ordered by id - the
	// order the round-robin selector assigns shards to new sessions in.
	ListOrderedByID(ctx context.Context) ([]postgres.MongoDBURL, error)
	// EnsureURL makes sure a row exists for url, inserting one if not -
	// this is how the set of known shards stays in sync with config.Config's
	// MongoURLs on every startup, without ever duplicating a row.
	EnsureURL(ctx context.Context, url string) (postgres.MongoDBURL, error)
	// GetByID resolves a shard id (as recorded in a SessionShardMap row)
	// back to its MongoDBURL.
	GetByID(ctx context.Context, id uint) (postgres.MongoDBURL, error)
}

type mongoURLRepository struct {
	db postgres.Interface
}

func NewMongoURLRepository(db postgres.Interface) (MongoURLRepository, error) {
	if db == nil {
		return nil, errors.New("repository: db is nil")
	}
	return mongoURLRepository{db: db}, nil
}

func (r mongoURLRepository) ListOrderedByID(ctx context.Context) ([]postgres.MongoDBURL, error) {
	var urls []postgres.MongoDBURL
	if err := r.db.DB().WithContext(ctx).Order("id ASC").Find(&urls).Error; err != nil {
		return nil, err
	}
	return urls, nil
}

func (r mongoURLRepository) EnsureURL(ctx context.Context, url string) (postgres.MongoDBURL, error) {
	var existing postgres.MongoDBURL
	err := r.db.DB().WithContext(ctx).Where("mongo_url = ?", url).First(&existing).Error
	if err == nil {
		return existing, nil
	}
	if !errors.Is(err, gorm.ErrRecordNotFound) {
		return postgres.MongoDBURL{}, err
	}
	row := postgres.MongoDBURL{MongoURL: url}
	if err := r.db.DB().WithContext(ctx).Create(&row).Error; err != nil {
		return postgres.MongoDBURL{}, err
	}
	return row, nil
}

func (r mongoURLRepository) GetByID(ctx context.Context, id uint) (postgres.MongoDBURL, error) {
	var row postgres.MongoDBURL
	if err := r.db.DB().WithContext(ctx).First(&row, id).Error; err != nil {
		return postgres.MongoDBURL{}, err
	}
	return row, nil
}
