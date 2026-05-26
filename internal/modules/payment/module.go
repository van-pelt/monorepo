// Package payment is the payment module's wiring layer.
package payment

import (
	"embed"
	"io/fs"

	"github.com/gofiber/fiber/v2"
	"github.com/jmoiron/sqlx"
	"github.com/rs/zerolog"

	"github.com/monorepo/internal/modules/account/accountport"
	"github.com/monorepo/internal/modules/payment/repository"
	"github.com/monorepo/internal/modules/payment/service"
	"github.com/monorepo/internal/modules/payment/transport"
	"github.com/monorepo/internal/shared/messaging"
	"github.com/monorepo/internal/shared/postgres"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Module is the payment vertical slice. It implements module.Module.
type Module struct {
	handler *transport.Handler
}

// New builds the payment module. It depends on the account module's public
// port (accountport.AccountProvider) and the messaging.Publisher, both
// injected at the composition root.
func New(db *sqlx.DB, log zerolog.Logger, accounts accountport.AccountProvider, publisher messaging.Publisher) *Module {
	l := log.With().Str("module", "payment").Logger()

	repo := repository.NewPaymentRepository()
	uow := postgres.NewUnitOfWork(db)
	svc := service.New(db, uow, repo, accounts, publisher, l)

	return &Module{handler: transport.NewHandler(svc)}
}

func (m *Module) Name() string { return "payment" }

func (m *Module) RegisterRoutes(r fiber.Router) { m.handler.Register(r) }

func (m *Module) Migrations() fs.FS { return migrationsFS }
