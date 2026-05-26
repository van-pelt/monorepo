package app

import (
	"context"
	"fmt"

	"github.com/pressly/goose/v3"

	"github.com/monorepo/migrations"
)

// migrate applies the base migrations (schemas + outbox) and then each
// module's migrations. Every module tracks its migration version in its own
// schema, so modules can evolve independently.
func (a *App) migrate(ctx context.Context) error {
	if err := goose.SetDialect("postgres"); err != nil {
		return fmt.Errorf("set dialect: %w", err)
	}
	sqlDB := a.db.DB

	// 1. Base migrations: per-module schemas and the shared outbox table.
	goose.SetBaseFS(migrations.FS)
	goose.SetTableName("public.goose_db_version")
	if err := goose.UpContext(ctx, sqlDB, "."); err != nil {
		return fmt.Errorf("base migrations: %w", err)
	}

	// 2. Per-module migrations, each tracked inside the module's own schema.
	for _, m := range a.modules {
		goose.SetBaseFS(m.Migrations())
		goose.SetTableName(m.Name() + ".goose_db_version")
		if err := goose.UpContext(ctx, sqlDB, "migrations"); err != nil {
			return fmt.Errorf("%s migrations: %w", m.Name(), err)
		}
		a.log.Info().Str("module", m.Name()).Msg("migrations applied")
	}
	return nil
}
