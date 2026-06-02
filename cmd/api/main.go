// Command api is the application entry point. It composes every module and
// platform service explicitly, then starts the HTTP server, outbox relay
// and RabbitMQ consumers. Migrations are out of scope — run cmd/migrate
// before starting the server.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/gofiber/fiber/v2/middleware/adaptor"
	"github.com/rs/zerolog"
	"golang.org/x/sync/errgroup"

	accountmod "github.com/monorepo/internal/modules/account"
	paymentmod "github.com/monorepo/internal/modules/payment"
	"github.com/monorepo/internal/platform/config"
	"github.com/monorepo/internal/platform/consumers"
	"github.com/monorepo/internal/platform/crons"
	"github.com/monorepo/internal/platform/featureflags"
	"github.com/monorepo/internal/platform/httpserver"
	"github.com/monorepo/internal/platform/idempotency"
	"github.com/monorepo/internal/platform/observability/health"
	"github.com/monorepo/internal/platform/observability/logger"
	"github.com/monorepo/internal/platform/observability/metrics"
	"github.com/monorepo/internal/platform/observability/tracing"
	"github.com/monorepo/internal/platform/outbox"
	"github.com/monorepo/internal/platform/postgres"
	"github.com/monorepo/internal/platform/rabbitmq"
	platformredis "github.com/monorepo/internal/platform/redis"
	"github.com/monorepo/internal/platform/security"
)

func main() {
	configPath := os.Getenv("APP_CONFIG")
	if configPath == "" {
		configPath = "config/config.yaml"
	}

	cfg, err := config.Load(configPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "load config: %v\n", err)
		os.Exit(1)
	}
	// Resolve any `secret:NAME` references in the config. Today only ENV
	// is wired (EnvSecretsProvider); swapping to a VaultSecretsProvider
	// later is a one-line change at the composition root.
	if err := security.ResolveSecrets(context.Background(), cfg, &security.EnvSecretsProvider{}); err != nil {
		fmt.Fprintf(os.Stderr, "resolve secrets: %v\n", err)
		os.Exit(1)
	}

	log := logger.New(cfg.Log.Level, cfg.Env)

	// ctx is cancelled on SIGINT/SIGTERM, triggering graceful shutdown.
	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := run(ctx, cfg, log); err != nil {
		log.Fatal().Err(err).Msg("application stopped with error")
	}
}

// run wires every dependency by hand and blocks until ctx is cancelled. Module
// graph reads top-to-bottom: account is built first because payment depends on
// its public api.Service. The outbox relay forwards events into RabbitMQ; the
// per-module consumer.Subscribers receive them and call module handlers.
func run(ctx context.Context, cfg *config.Config, log zerolog.Logger) error {
	// Set up OTel propagator + TracerProvider. No exporter is configured —
	// spans get sampled and dropped, but propagation through HTTP / outbox /
	// AMQP is fully wired. Add sdktrace.WithBatcher(otlpExporter) in
	// tracing.Init to start emitting.
	shutdownTracing := tracing.Init()
	defer func() {
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = shutdownTracing(shutdownCtx)
	}()

	db, err := postgres.Connect(ctx, postgres.Config{
		DSN:              cfg.DB.DSN,
		MaxOpenConns:     cfg.DB.MaxOpenConns,
		MaxIdleConns:     cfg.DB.MaxIdleConns,
		ConnMaxLifetime:  cfg.DB.ConnMaxLifetime,
		StatementTimeout: cfg.DB.StatementTimeout,
	})
	if err != nil {
		return fmt.Errorf("postgres: %w", err)
	}
	defer func() { _ = db.Close() }()

	// Feature flags: in-memory provider snapshotted from config. Modules
	// that need toggles take a featureflags.Provider in their New().
	// Replace with a remote-config Provider (LaunchDarkly etc.) when
	// runtime toggling is needed.
	flags := featureflags.NewInMemoryProvider(cfg.FeatureFlags)
	log.Info().Int("count", flags.Count()).Msg("feature flags loaded")
	_ = flags // no module consumes it yet — wire as parameter when needed

	rmq, err := rabbitmq.Connect(rabbitmq.Config{
		DSN:      cfg.RabbitMQ.DSN,
		Exchange: cfg.RabbitMQ.Exchange,
		DLX:      cfg.RabbitMQ.DLX,
	}, log)
	if err != nil {
		return fmt.Errorf("rabbitmq: %w", err)
	}
	defer func() { _ = rmq.Close() }()

	// Redis is optional: when cfg.Redis.DSN is empty, idemStorage stays nil
	// and the idempotency middleware is not mounted (mutating endpoints
	// behave as before).
	var (
		rdb         *platformredis.Client
		idemStorage idempotency.Storage
	)
	if cfg.Redis.DSN != "" {
		rdb, err = platformredis.Connect(ctx, platformredis.Config{DSN: cfg.Redis.DSN}, log)
		if err != nil {
			return fmt.Errorf("redis: %w", err)
		}
		defer func() { _ = rdb.Close() }()
		idemStorage = idempotency.NewRedisStorage(rdb)
	}

	// Outbox relay dispatches via rabbitmq.Publisher → AMQP exchange.
	// Per-module outbox.Publishers write into <schema>.outbox in the
	// caller's transaction.
	dispatcher := rabbitmq.NewPublisher(rmq)
	paymentPublisher := outbox.NewPublisher("payment")
	relay := outbox.NewRelay(db, cfg.DB.DSN, []string{"account", "payment"}, dispatcher, log, outbox.Config{
		Interval:    cfg.Outbox.PollInterval,
		BatchSize:   cfg.Outbox.BatchSize,
		MaxAttempts: cfg.Outbox.MaxAttempts,
		BaseBackoff: cfg.Outbox.BaseBackoff,
		MaxBackoff:  cfg.Outbox.MaxBackoff,
	})

	// One consumers.Subscriber per module: each owns its set of AMQP queues
	// named "<module>.<topic>". account is the only module with handlers
	// today; payment subscribes to nothing yet but the wiring is symmetric.
	// Bulkhead settings come from cfg.Consumers; for per-topic overrides
	// pass consumers.Config{TopicConcurrency: map[string]int{...}}.
	consumersCfg := consumers.Config{
		DefaultConcurrency:     cfg.Consumers.DefaultConcurrency,
		HandlerTimeout:         cfg.Consumers.HandlerTimeout,
		QueueDepthPollInterval: cfg.Consumers.QueueDepthPollInterval,
	}
	accountSubscriber := consumers.New(rmq, "account", consumersCfg, log)

	account := accountmod.New(db, accountSubscriber, log)
	payment := paymentmod.New(db, log, account.API(), paymentPublisher)

	// Cron framework: started below alongside other background services.
	// Modules can Register schedules in their New() when they grow
	// recurring work; no jobs today.
	scheduler := crons.NewScheduler(db, log)

	server := httpserver.New(httpserver.Config{
		Port:           cfg.HTTP.Port,
		RequestTimeout: cfg.HTTP.RequestTimeout,
	}, log)
	registerRoutes(server.API(), idemStorage, log, account.API(), payment.API())

	// Ops endpoints mounted on the unversioned root so k8s probes and the
	// Prometheus scraper hit fixed paths regardless of API versioning.
	probes := []health.Probe{
		func(ctx context.Context) error { return db.PingContext(ctx) },
		rmq.HealthCheck,
	}
	if rdb != nil {
		probes = append(probes, rdb.HealthCheck)
	}
	root := server.Root()
	root.Get("/healthz", health.Liveness)
	root.Get("/readyz", health.Readiness(probes...))
	root.Get("/metrics", adaptor.HTTPHandler(metrics.Handler()))

	// All background workers run under a single errgroup tied to gctx, so
	// that (1) cancelling ctx (SIGTERM) cancels them in one go, (2) any one
	// of them failing fatally tears down the rest, and (3) g.Wait() gives
	// us a single sync point we can block on during shutdown so resource
	// closers (db, rmq, redis) do not fire until workers have drained.
	g, gctx := errgroup.WithContext(ctx)

	g.Go(func() error {
		relay.Run(gctx)
		return nil
	})
	g.Go(func() error {
		if err := accountSubscriber.Run(gctx); err != nil && gctx.Err() == nil {
			return fmt.Errorf("account consumer: %w", err)
		}
		return nil
	})
	g.Go(func() error {
		if err := scheduler.Run(gctx); err != nil && gctx.Err() == nil {
			return fmt.Errorf("cron scheduler: %w", err)
		}
		return nil
	})

	httpErrCh := make(chan error, 1)
	go func() {
		log.Info().Int("port", cfg.HTTP.Port).Msg("http server starting")
		httpErrCh <- server.Start()
	}()

	select {
	case <-ctx.Done():
		log.Info().Msg("shutdown signal received")
	case err := <-httpErrCh:
		// HTTP died on its own — still try to drain background workers cleanly.
		log.Error().Err(err).Msg("http server stopped")
	}

	// Phase 1: drain HTTP. server.Shutdown blocks until in-flight requests
	// finish or ShutdownTimeout expires. Pair with request_timeout middleware
	// (10s) so handlers respecting ctx unwind well before the 15s wall.
	shutdownCtx, cancel := context.WithTimeout(context.Background(), cfg.HTTP.ShutdownTimeout)
	defer cancel()
	if err := server.Shutdown(shutdownCtx); err != nil {
		log.Error().Err(err).Msg("server shutdown error")
	}

	// Phase 2: wait for background workers. They observe gctx.Done (cancelled
	// when ctx is cancelled) and unwind via their internal WaitGroups —
	// consumer drains in-flight handlers, relay finishes its current
	// dispatchBatch, scheduler waits for in-flight jobs (up to its own
	// shutdownTimeout). Hard-cap so a stuck handler can't block exit forever.
	workersDone := make(chan error, 1)
	go func() { workersDone <- g.Wait() }()
	hardCap := cfg.HTTP.ShutdownTimeout + 30*time.Second
	select {
	case err := <-workersDone:
		if err != nil {
			log.Warn().Err(err).Msg("background workers exited with error")
		}
	case <-time.After(hardCap):
		log.Error().Dur("waited", hardCap).Msg("background workers did not finish in time; forcing exit")
	}

	// Phase 3: deferred closers (db, rmq, redis, tracing) now run on a
	// quiesced system — no pending tx/channel/publish in flight.
	log.Info().Msg("shutdown complete")
	return nil
}
