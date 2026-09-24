package postgres

import (
	"time"

	"gorm.io/datatypes"
)

// MongoDBURL is one configured MongoDB shard. Every table in this system
// carries the same three bookkeeping columns: a NOT NULL, default-{}
// metadata blob, and created/updated timestamps.
type MongoDBURL struct {
	ID        uint           `gorm:"primaryKey;autoIncrement"`
	MongoURL  string         `gorm:"column:mongo_url;not null;uniqueIndex"`
	Metadata  datatypes.JSON `gorm:"not null;default:'{}'"`
	CreatedAt time.Time      `gorm:"not null"`
	UpdatedAt time.Time      `gorm:"not null"`
}

func (MongoDBURL) TableName() string { return "mongo_db_urls" }

// SessionShardMap records which MongoDBURL a given global session's data
// lives on. One row per session, written once at session-creation time and
// never changed afterward - see package sharding for how a shard is picked.
type SessionShardMap struct {
	ID         uint           `gorm:"primaryKey;autoIncrement"`
	SessionID  string         `gorm:"column:session_id;not null;uniqueIndex"`
	MongoURLID uint           `gorm:"column:mongo_url_id;not null;index"`
	MongoDBURL MongoDBURL     `gorm:"foreignKey:MongoURLID"`
	Metadata   datatypes.JSON `gorm:"not null;default:'{}'"`
	CreatedAt  time.Time      `gorm:"not null"`
	UpdatedAt  time.Time      `gorm:"not null"`
}

func (SessionShardMap) TableName() string { return "session_shard_map" }
