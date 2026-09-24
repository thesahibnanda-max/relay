package repository

import (
	"context"
	"errors"

	"go.mongodb.org/mongo-driver/v2/bson"
	"go.mongodb.org/mongo-driver/v2/mongo"
)

// namespaceExistsCode is MongoDB's command-error code for "this collection
// already exists" - the only error ensureCollection treats as success.
const namespaceExistsCode = 48

// ensureCollection creates a collection if it doesn't already exist, so a
// missing table gets made automatically rather than relying on the implicit
// (and easy to overlook) auto-creation MongoDB does on first insert.
func ensureCollection(ctx context.Context, db *mongo.Database, name string) error {
	err := db.CreateCollection(ctx, name)
	if err == nil {
		return nil
	}
	var cmdErr mongo.CommandError
	if errors.As(err, &cmdErr) && cmdErr.Code == namespaceExistsCode {
		return nil
	}
	return err
}

// byID is the {_id: id} filter every repository in this package looks
// documents up with.
func byID(id string) bson.M {
	return bson.M{"_id": id}
}
