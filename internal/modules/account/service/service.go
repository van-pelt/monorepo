// Package service holds the account module's use cases (business logic).
package service

import (
	"context"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"
	"github.com/rs/zerolog"

	"github.com/monorepo/internal/modules/account/domain"
	"github.com/monorepo/internal/shared/postgres"
)

// Service is the account module's use-case contract. All consumers (HTTP
// handler, event handler, port adapter) depend on this interface and can swap
// in a mock for tests; the concrete implementation lives in this package and
// is reached only through New.
type Service interface {
	CreateAccount(ctx context.Context, ownerID uuid.UUID, currency string) (*domain.Account, error)
	GetAccount(ctx context.Context, id uuid.UUID) (*domain.Account, error)
	Transfer(ctx context.Context, fromID, toID uuid.UUID, amount int64) error
}

type service struct {
	db   *sqlx.DB
	uow  *postgres.UnitOfWork
	repo domain.Repository
	log  zerolog.Logger
}

func New(db *sqlx.DB, uow *postgres.UnitOfWork, repo domain.Repository, log zerolog.Logger) Service {
	return &service{db: db, uow: uow, repo: repo, log: log}
}

// CreateAccount validates and persists a new account.
func (s *service) CreateAccount(ctx context.Context, ownerID uuid.UUID, currency string) (*domain.Account, error) {
	acc, err := domain.NewAccount(ownerID, currency)
	if err != nil {
		return nil, err
	}
	if err := s.uow.Do(ctx, func(ctx context.Context, q postgres.Querier) error {
		return s.repo.Create(ctx, q, acc)
	}); err != nil {
		return nil, err
	}
	return acc, nil
}

// GetAccount is a read; it runs outside a transaction on the pool directly.
func (s *service) GetAccount(ctx context.Context, id uuid.UUID) (*domain.Account, error) {
	return s.repo.GetByID(ctx, s.db, id)
}

// Transfer moves funds between two accounts atomically. It is invoked
// asynchronously by the module's payment.created event handler.
func (s *service) Transfer(ctx context.Context, fromID, toID uuid.UUID, amount int64) error {
	return s.uow.Do(ctx, func(ctx context.Context, q postgres.Querier) error {
		from, err := s.repo.GetByID(ctx, q, fromID)
		if err != nil {
			return err
		}
		to, err := s.repo.GetByID(ctx, q, toID)
		if err != nil {
			return err
		}
		if !from.CanDebit(amount) {
			return domain.ErrInsufficient
		}
		if err := s.repo.UpdateBalance(ctx, q, from.ID, from.Balance-amount); err != nil {
			return err
		}
		return s.repo.UpdateBalance(ctx, q, to.ID, to.Balance+amount)
	})
}
