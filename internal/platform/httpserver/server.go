// Package httpserver wraps the Fiber HTTP server: shared middleware, a single
// error handler and a versioned router group modules attach their routes to.
package httpserver

import (
	"context"
	"fmt"
	"time"

	"github.com/gofiber/fiber/v2"
	recovermw "github.com/gofiber/fiber/v2/middleware/recover"
	"github.com/gofiber/fiber/v2/middleware/requestid"
	"github.com/rs/zerolog"

	"github.com/monorepo/internal/platform/observability/metrics"
	"github.com/monorepo/internal/platform/observability/tracing"
)

// Config controls the HTTP server. Port is required; RequestTimeout, when
// > 0, installs the timeout middleware that wraps c.UserContext() with a
// per-request deadline.
type Config struct {
	Port           int
	RequestTimeout time.Duration
}

type Server struct {
	app  *fiber.App
	port int
	log  zerolog.Logger
}

func New(cfg Config, log zerolog.Logger) *Server {
	app := fiber.New(fiber.Config{
		AppName:      "monorepo",
		ErrorHandler: errorHandler(log),
	})
	// Order matters: recover wraps everything for panic safety; metrics +
	// tracing run early so spans/timers cover the whole chain. Timeout is
	// placed after tracing so the deadline-augmented ctx still carries the
	// span. requestLogger sits last because it logs after c.Next returns.
	app.Use(recovermw.New())
	app.Use(requestid.New())
	app.Use(metrics.Middleware())
	app.Use(tracing.Middleware())
	if cfg.RequestTimeout > 0 {
		app.Use(timeoutMiddleware(cfg.RequestTimeout))
	}
	app.Use(requestLogger(log))

	return &Server{app: app, port: cfg.Port, log: log}
}

// API returns the versioned router group that modules register their routes on.
func (s *Server) API() fiber.Router {
	return s.app.Group("/api/v1")
}

// Root returns the unversioned router. Use for ops endpoints (/healthz,
// /readyz, /metrics) that k8s and Prometheus expect at fixed paths.
func (s *Server) Root() fiber.Router {
	return s.app
}

func (s *Server) Start() error {
	return s.app.Listen(fmt.Sprintf(":%d", s.port))
}

func (s *Server) Shutdown(ctx context.Context) error {
	return s.app.ShutdownWithContext(ctx)
}
