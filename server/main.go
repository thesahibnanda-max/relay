package main

import (
	"log/slog"

	"go.uber.org/fx"

	"github.com/thesahibnanda-max/relay/server/package/app"

	"github.com/joho/godotenv"
)

func main() {
	if err := godotenv.Load(".env.stage"); err != nil {
		slog.Warn("error in setting env vars", slog.Any("error", err))
	}
	fx.New(
		app.Module,
		fx.Invoke(app.Serve),
	).Run()
}
