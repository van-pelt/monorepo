// Package httpserver wraps the Fiber HTTP server: shared middleware, a single
// error handler and a versioned router group modules attach their routes to.
package httpserver

import (
	"context"
	"fmt"

	"github.com/gofiber/fiber/v2"
	recovermw "github.com/gofiber/fiber/v2/middleware/recover"
	"github.com/gofiber/fiber/v2/middleware/requestid"
	"github.com/rs/zerolog"
)

type Server struct {
	app  *fiber.App
	port int
	log  zerolog.Logger
}

func New(port int, log zerolog.Logger) *Server {
	app := fiber.New(fiber.Config{
		AppName:      "monorepo",
		ErrorHandler: errorHandler(log),
	})
	app.Use(recovermw.New())
	app.Use(requestid.New())
	app.Use(requestLogger(log))

	return &Server{app: app, port: port, log: log}
}

// API returns the versioned router group that modules register their routes on.
func (s *Server) API() fiber.Router {
	return s.app.Group("/api/v1")
}

func (s *Server) Start() error {
	return s.app.Listen(fmt.Sprintf(":%d", s.port))
}

func (s *Server) Shutdown(ctx context.Context) error {
	return s.app.ShutdownWithContext(ctx)
}
