package main

import (
	"context"
	"os"
	"os/signal"
	"syscall"

	"github.com/monorepo/internal/app"
	"github.com/monorepo/internal/shared/config"
	"github.com/monorepo/internal/shared/logger"
)

func main() {
	configPath := os.Getenv("APP_CONFIG")
	if configPath == "" {
		configPath = "config/config.yaml"
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		panic(err)
	}

	log := logger.New(cfg.Log.Level, cfg.Env)

	// ctx is cancelled on SIGINT/SIGTERM, triggering graceful shutdown.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	application, err := app.New(ctx, cfg, log)
	if err != nil {
		log.Fatal().Err(err).Msg("failed to build application")
	}

	if err := application.Run(ctx); err != nil {
		log.Fatal().Err(err).Msg("application stopped with error")
	}
}
