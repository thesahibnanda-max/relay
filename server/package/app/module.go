// Package app wires every package's constructor into one Uber Fx graph and
// starts serving. This is the only package that knows about every other
// package in the module - everything else only knows its own direct
// dependencies via the interfaces it declares.
package app

import (
	"go.uber.org/fx"

	"github.com/thesahibnanda-max/relay/server/package/config"
	"github.com/thesahibnanda-max/relay/server/package/database/mongodb"
	"github.com/thesahibnanda-max/relay/server/package/database/postgres"
	"github.com/thesahibnanda-max/relay/server/package/database/repository"
	"github.com/thesahibnanda-max/relay/server/package/session"
	"github.com/thesahibnanda-max/relay/server/package/sharding"
	"github.com/thesahibnanda-max/relay/server/package/ws"
)

// Module provides every constructor in the module. Fx resolves the graph
// from the interfaces each New(...) function asks for - nothing here needs
// fx.Annotate or named instances, because every constructor follows the
// same New(deps...) (Interface, error) shape.
var Module = fx.Module("server",
	fx.Provide(
		config.New,
		postgres.New,
		mongodb.New,
		repository.NewMongoURLRepository,
		repository.NewShardMapRepository,
		repository.NewSessionRepository,
		repository.NewAgentRepository,
		repository.NewMessageRepository,
		sharding.New,
		session.New,
		ws.New,
	),
)
