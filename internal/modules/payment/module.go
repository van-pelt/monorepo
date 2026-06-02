// Package payment is the payment module's wiring layer.
package payment

import (
	"context"
	"errors"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"github.com/rs/zerolog"

	accountapi "github.com/monorepo/internal/modules/account/api"
	paymentapi "github.com/monorepo/internal/modules/payment/api"
	"github.com/monorepo/internal/modules/payment/internal/domain"
	"github.com/monorepo/internal/modules/payment/internal/repository"
	"github.com/monorepo/internal/modules/payment/internal/service"
	"github.com/monorepo/internal/platform/outbox"
	"github.com/monorepo/internal/platform/postgres"
)

// Module is the payment vertical slice.
type Module struct {
	svc service.Service
	log zerolog.Logger
}

// New builds the payment module. It depends on the account module's public
// API (accountapi.Service) and an outbox.Publisher scoped to payment's
// schema.
func New(db *sqlx.DB, log zerolog.Logger, accounts accountapi.Service, publisher outbox.Publisher) *Module {
	l := log.With().Str("module", "payment").Logger()

	repo := repository.NewPaymentRepository()
	uow := postgres.NewUnitOfWork(db)
	svc := service.New(db, uow, repo, accounts, publisher, l)

	return &Module{svc: svc, log: l}
}

func (m *Module) Name() string { return "payment" }

// API exposes the payment module's synchronous public surface.
func (m *Module) API() paymentapi.Service { return apiAdapter{svc: m.svc} }

// apiAdapter implements paymentapi.Service against the in-process service.
type apiAdapter struct {
	svc service.Service
}

func (a apiAdapter) CreatePayment(ctx context.Context, in paymentapi.CreatePaymentInput) (paymentapi.Payment, error) {
	p, err := a.svc.CreatePayment(ctx, service.CreatePaymentInput{
		FromAccountID: in.FromAccountID,
		ToAccountID:   in.ToAccountID,
		Amount:        in.Amount,
	})
	if err != nil {
		return paymentapi.Payment{}, err
	}
	return toAPI(p), nil
}

func (a apiAdapter) GetByID(ctx context.Context, id uuid.UUID) (paymentapi.Payment, error) {
	p, err := a.svc.GetPayment(ctx, id)
	if err != nil {
		if errors.Is(err, domain.ErrPaymentNotFound) {
			return paymentapi.Payment{}, paymentapi.ErrPaymentNotFound
		}
		return paymentapi.Payment{}, err
	}
	return toAPI(p), nil
}

func toAPI(p *domain.Payment) paymentapi.Payment {
	return paymentapi.Payment{
		ID:            p.ID,
		FromAccountID: p.FromAccountID,
		ToAccountID:   p.ToAccountID,
		Amount:        p.Amount,
		Currency:      p.Currency,
		Status:        string(p.Status),
		CreatedAt:     p.CreatedAt,
	}
}
