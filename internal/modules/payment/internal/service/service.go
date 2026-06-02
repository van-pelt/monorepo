// Package service holds the payment module's use cases.
package service

import (
	"context"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"github.com/rs/zerolog"

	accountapi "github.com/monorepo/internal/modules/account/api"
	paymentapi "github.com/monorepo/internal/modules/payment/api"
	"github.com/monorepo/internal/modules/payment/internal/domain"
	"github.com/monorepo/internal/platform/outbox"
	"github.com/monorepo/internal/platform/postgres"
)

// Service is the payment module's use-case contract. Consumers depend on this
// interface; the concrete implementation is reached only through New.
type Service interface {
	CreatePayment(ctx context.Context, in CreatePaymentInput) (*domain.Payment, error)
	GetPayment(ctx context.Context, id uuid.UUID) (*domain.Payment, error)
}

// CreatePaymentInput is the service's input vocabulary for a new payment.
type CreatePaymentInput struct {
	FromAccountID uuid.UUID
	ToAccountID   uuid.UUID
	Amount        int64
}

type service struct {
	db        *sqlx.DB
	uow       *postgres.UnitOfWork
	repo      domain.Repository
	accounts  accountapi.Service // synchronous API into the account module
	publisher outbox.Publisher
	log       zerolog.Logger
}

func New(
	db *sqlx.DB,
	uow *postgres.UnitOfWork,
	repo domain.Repository,
	accounts accountapi.Service,
	publisher outbox.Publisher,
	log zerolog.Logger,
) Service {
	return &service{db: db, uow: uow, repo: repo, accounts: accounts, publisher: publisher, log: log}
}

// CreatePayment validates the accounts via the account module's API, then
// persists the payment together with a payment.created event in one
// transaction (transactional outbox). The outbox relay publishes the event
// afterwards; the account module consumes it to settle the funds transfer.
func (s *service) CreatePayment(ctx context.Context, in CreatePaymentInput) (*domain.Payment, error) {
	// --- synchronous cross-module call through the public API ---
	from, err := s.accounts.GetByID(ctx, in.FromAccountID)
	if err != nil {
		return nil, err
	}
	to, err := s.accounts.GetByID(ctx, in.ToAccountID)
	if err != nil {
		return nil, err
	}
	if from.Currency != to.Currency {
		return nil, domain.ErrCurrencyMismatch
	}

	payment, err := domain.NewPayment(in.FromAccountID, in.ToAccountID, in.Amount, from.Currency)
	if err != nil {
		return nil, err
	}

	// --- persist payment + outbox event atomically ---
	if err := s.uow.Do(ctx, func(ctx context.Context, q postgres.Querier) error {
		if err := s.repo.Create(ctx, q, payment); err != nil {
			return err
		}
		return s.publisher.Publish(ctx, q, paymentapi.TopicPaymentCreated, paymentapi.PaymentCreated{
			PaymentID:     payment.ID,
			FromAccountID: payment.FromAccountID,
			ToAccountID:   payment.ToAccountID,
			Amount:        payment.Amount,
			Currency:      payment.Currency,
		})
	}); err != nil {
		return nil, err
	}
	return payment, nil
}

// GetPayment is a read; it runs outside a transaction.
func (s *service) GetPayment(ctx context.Context, id uuid.UUID) (*domain.Payment, error) {
	return s.repo.GetByID(ctx, s.db, id)
}
