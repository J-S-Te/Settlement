package main

import (
	"context"
	"log/slog"
	"os"

	"github.com/j-s-te/settlement/internal/config"
	"github.com/j-s-te/settlement/internal/migration"
	"github.com/j-s-te/settlement/migrations"
)

func main() {
	dsn, err := config.LoadDatabase()
	if err != nil {
		slog.Error("configuration failed", "error", err)
		os.Exit(1)
	}
	if err := migration.Run(context.Background(), dsn, migrations.Files); err != nil {
		slog.Error("migration failed", "error", err)
		os.Exit(1)
	}
	slog.Info("settlement migrations complete")
}
