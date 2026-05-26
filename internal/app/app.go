// Package app is the composition root: it constructs every dependency, wires
// the modules together and owns the application lifecycle.
package app

import (
	"context"
	"fmt"

	"github.com/jmoiron/sqlx"
	"github.com/rs/zerolog"

	accountmod "github.com/monorepo/internal/modules/account"
	paymentmod "github.com/monorepo/internal/modules/payment"
	"github.com/monorepo/internal/shared/config"
	"github.com/monorepo/internal/shared/httpserver"
	"github.com/monorepo/internal/shared/messaging"
	"github.com/monorepo/internal/shared/module"
	"github.com/monorepo/internal/shared/postgres"
)

type App struct {
	cfg       *config.Config
	log       zerolog.Logger
	db        *sqlx.DB
	server    *httpserver.Server
	messaging *messaging.Engine
	modules   []module.Module
}

// New builds the application. This is the single place where modules are
// instantiated and their dependencies (DB, bus, ports) are injected by hand.
func New(ctx context.Context, cfg *config.Config, log zerolog.Logger) (*App, error) {
	db, err := postgres.Connect(ctx, postgres.Config{
		DSN:             cfg.DB.DSN,
		MaxOpenConns:    cfg.DB.MaxOpenConns,
		MaxIdleConns:    cfg.DB.MaxIdleConns,
		ConnMaxLifetime: cfg.DB.ConnMaxLifetime,
	})
	if err != nil {
		return nil, err
	}

	msg := messaging.NewEngine(db, cfg.DB.DSN, log, messaging.Config{
		Interval:    cfg.Outbox.PollInterval,
		BatchSize:   cfg.Outbox.BatchSize,
		MaxAttempts: cfg.Outbox.MaxAttempts,
		BaseBackoff: cfg.Outbox.BaseBackoff,
		MaxBackoff:  cfg.Outbox.MaxBackoff,
	})

	// --- wire modules ---
	// account is built first because payment depends on its public port.
	account := accountmod.New(db, msg, log)
	payment := paymentmod.New(db, log, account.Provider(), msg)
	modules := []module.Module{account, payment}

	server := httpserver.New(cfg.HTTP.Port, log)
	for _, m := range modules {
		m.RegisterRoutes(server.API())
	}

	return &App{
		cfg:       cfg,
		log:       log,
		db:        db,
		server:    server,
		messaging: msg,
		modules:   modules,
	}, nil
}

// Run applies migrations, starts the outbox relay and HTTP server, and blocks
// until ctx is cancelled, then shuts everything down gracefully.
func (a *App) Run(ctx context.Context) error {
	if a.cfg.DB.AutoMigrate {
		if err := a.migrate(ctx); err != nil {
			return fmt.Errorf("migrate: %w", err)
		}
	}

	go a.messaging.Run(ctx)

	errCh := make(chan error, 1)
	go func() {
		a.log.Info().Int("port", a.cfg.HTTP.Port).Msg("http server starting")
		errCh <- a.server.Start()
	}()

	select {
	case <-ctx.Done():
		a.log.Info().Msg("shutdown signal received")
	case err := <-errCh:
		return fmt.Errorf("http server: %w", err)
	}

	shutdownCtx, cancel := context.WithTimeout(context.Background(), a.cfg.HTTP.ShutdownTimeout)
	defer cancel()
	if err := a.server.Shutdown(shutdownCtx); err != nil {
		a.log.Error().Err(err).Msg("server shutdown error")
	}
	if err := a.db.Close(); err != nil {
		a.log.Error().Err(err).Msg("db close error")
	}
	a.log.Info().Msg("shutdown complete")
	return nil
}
