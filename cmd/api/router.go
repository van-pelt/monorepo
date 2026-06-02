package main

import (
	"github.com/gofiber/fiber/v2"
	"github.com/rs/zerolog"

	"github.com/monorepo/cmd/api/handlers"
	accountapi "github.com/monorepo/internal/modules/account/api"
	paymentapi "github.com/monorepo/internal/modules/payment/api"
	"github.com/monorepo/internal/platform/idempotency"
)

// registerRoutes attaches every module's HTTP handlers to the API router
// group. Handlers depend only on each module's public api.Service.
//
// idemStorage may be nil when Redis is disabled (cfg.Redis.DSN empty) —
// the idempotency middleware is then simply not mounted, so mutating
// endpoints behave as before.
func registerRoutes(
	api fiber.Router,
	idemStorage idempotency.Storage,
	log zerolog.Logger,
	account accountapi.Service,
	payment paymentapi.Service,
) {
	if idemStorage != nil {
		api.Use(idempotency.Middleware(idemStorage, log))
	}
	handlers.NewAccountHandler(account).Register(api)
	handlers.NewPaymentHandler(payment).Register(api)
}
