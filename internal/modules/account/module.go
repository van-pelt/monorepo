// Package account is the account module's wiring layer: it builds the module's
// layers, exposes its public API and subscribes to events from other modules.
// HTTP handlers live in cmd/api/handlers and consume only the api package.
package account

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"github.com/rs/zerolog"

	accountapi "github.com/monorepo/internal/modules/account/api"
	"github.com/monorepo/internal/modules/account/internal/domain"
	"github.com/monorepo/internal/modules/account/internal/repository"
	"github.com/monorepo/internal/modules/account/internal/service"
	paymentapi "github.com/monorepo/internal/modules/payment/api"
	"github.com/monorepo/internal/platform/consumers"
	"github.com/monorepo/internal/platform/messaging"
	"github.com/monorepo/internal/platform/postgres"
)

// Module is the account vertical slice. cmd/api wires it explicitly — there
// is no Module interface.
type Module struct {
	svc service.Service
	log zerolog.Logger
}

// New builds the account module and wires its repository and service layers.
// It also subscribes to payment events.
func New(db *sqlx.DB, subscriber messaging.Subscriber, log zerolog.Logger) *Module {
	l := log.With().Str("module", "account").Logger()

	repo := repository.NewAccountRepository()
	uow := postgres.NewUnitOfWork(db)
	svc := service.New(db, uow, repo, l)

	m := &Module{svc: svc, log: l}

	// payment.created drives an async funds transfer. consumers.Dedup
	// wraps the handler so the processed_events mark and the transfer
	// commit in the same tx — exactly-once-effect under at-least-once
	// broker delivery.
	subscriber.Subscribe(
		paymentapi.TopicPaymentCreated,
		consumers.Dedup(uow, "account", m.onPaymentCreatedTx),
	)
	return m
}

func (m *Module) Name() string { return "account" }

// API exposes the account module's synchronous public surface. cmd/api uses
// it both for HTTP handlers and to inject into other modules that need a
// synchronous account dependency.
func (m *Module) API() accountapi.Service {
	return apiAdapter{svc: m.svc}
}

// onPaymentCreatedTx settles a payment by transferring funds between accounts
// inside the tx provided by consumers.Dedup. Redeliveries are short-circuited
// upstream of this method.
func (m *Module) onPaymentCreatedTx(ctx context.Context, q postgres.Querier, e messaging.Event) error {
	var evt paymentapi.PaymentCreated
	if err := json.Unmarshal(e.Payload, &evt); err != nil {
		return err
	}
	m.log.Info().
		Str("event_id", e.ID.String()).
		Str("payment_id", evt.PaymentID.String()).
		Int64("amount", evt.Amount).
		Msg("settling payment")
	return m.svc.TransferTx(ctx, q, evt.FromAccountID, evt.ToAccountID, evt.Amount)
}

// apiAdapter implements accountapi.Service against the in-process service.
// A future gRPC client would implement the same interface against a remote
// process — handlers in cmd/api wouldn't change.
type apiAdapter struct {
	svc service.Service
}

func (a apiAdapter) CreateAccount(ctx context.Context, in accountapi.CreateAccountInput) (accountapi.Account, error) {
	acc, err := a.svc.CreateAccount(ctx, in.OwnerID, in.Currency)
	if err != nil {
		return accountapi.Account{}, err
	}
	return toAPI(acc), nil
}

func (a apiAdapter) GetByID(ctx context.Context, id uuid.UUID) (accountapi.Account, error) {
	acc, err := a.svc.GetAccount(ctx, id)
	if err != nil {
		if errors.Is(err, domain.ErrAccountNotFound) {
			return accountapi.Account{}, accountapi.ErrAccountNotFound
		}
		return accountapi.Account{}, err
	}
	return toAPI(acc), nil
}

func toAPI(a *domain.Account) accountapi.Account {
	return accountapi.Account{
		ID:        a.ID,
		OwnerID:   a.OwnerID,
		Currency:  a.Currency,
		Balance:   a.Balance,
		CreatedAt: a.CreatedAt,
		UpdatedAt: a.UpdatedAt,
	}
}
