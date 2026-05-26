// Package account is the account module's wiring layer: it builds the module's
// layers, exposes its public port and subscribes to events from other modules.
package account

import (
	"context"
	"embed"
	"encoding/json"
	"errors"
	"io/fs"

	"github.com/gofiber/fiber/v2"
	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"github.com/rs/zerolog"

	"github.com/monorepo/internal/modules/account/accountport"
	"github.com/monorepo/internal/modules/account/domain"
	"github.com/monorepo/internal/modules/account/repository"
	"github.com/monorepo/internal/modules/account/service"
	"github.com/monorepo/internal/modules/account/transport"
	"github.com/monorepo/internal/modules/payment/paymentport"
	"github.com/monorepo/internal/shared/messaging"
	"github.com/monorepo/internal/shared/postgres"
)

//go:embed migrations/*.sql
var migrationsFS embed.FS

// Module is the account vertical slice. It implements module.Module.
type Module struct {
	svc     service.Service
	handler *transport.Handler
	log     zerolog.Logger
}

// New builds the account module and wires its repository, service and
// transport layers. It also subscribes to payment events.
func New(db *sqlx.DB, subscriber messaging.Subscriber, log zerolog.Logger) *Module {
	l := log.With().Str("module", "account").Logger()

	repo := repository.NewAccountRepository()
	uow := postgres.NewUnitOfWork(db)
	svc := service.New(db, uow, repo, l)

	m := &Module{
		svc:     svc,
		handler: transport.NewHandler(svc),
		log:     l,
	}

	// The account module reacts to payments asynchronously (eventual
	// consistency): when a payment is created, it settles the funds transfer.
	subscriber.Subscribe(paymentport.TopicPaymentCreated, m.onPaymentCreated)
	return m
}

func (m *Module) Name() string { return "account" }

func (m *Module) RegisterRoutes(r fiber.Router) { m.handler.Register(r) }

func (m *Module) Migrations() fs.FS { return migrationsFS }

// Provider exposes the account module's synchronous public port, which other
// modules receive at the composition root.
func (m *Module) Provider() accountport.AccountProvider {
	return providerAdapter{svc: m.svc}
}

// onPaymentCreated settles a payment by transferring funds between accounts.
// It must be idempotent: the outbox guarantees at-least-once delivery.
func (m *Module) onPaymentCreated(ctx context.Context, e messaging.Event) error {
	var evt paymentport.PaymentCreated
	if err := json.Unmarshal(e.Payload, &evt); err != nil {
		return err
	}
	m.log.Info().
		Str("payment_id", evt.PaymentID.String()).
		Int64("amount", evt.Amount).
		Msg("settling payment")
	return m.svc.Transfer(ctx, evt.FromAccountID, evt.ToAccountID, evt.Amount)
}

// providerAdapter adapts the internal service to the public AccountProvider
// port, translating domain errors into the port's error contract.
type providerAdapter struct {
	svc service.Service
}

func (p providerAdapter) GetByID(ctx context.Context, id uuid.UUID) (accountport.AccountInfo, error) {
	acc, err := p.svc.GetAccount(ctx, id)
	if err != nil {
		if errors.Is(err, domain.ErrAccountNotFound) {
			return accountport.AccountInfo{}, accountport.ErrAccountNotFound
		}
		return accountport.AccountInfo{}, err
	}
	return accountport.AccountInfo{
		ID:       acc.ID,
		OwnerID:  acc.OwnerID,
		Currency: acc.Currency,
	}, nil
}
