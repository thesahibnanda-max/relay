package main

import (
	"go.uber.org/fx"

	"github.com/thesahibnanda-max/relay/server/package/app"
)

func main() {
	fx.New(
		app.Module,
		fx.Invoke(app.Serve),
	).Run()
}
