package domain

import (
	"context"

	"github.com/google/uuid"

	"github.com/monorepo/internal/platform/postgres"
)

// Repository is the persistence port. It is defined here (owned by the domain),
// implemented in the repository layer and consumed by the service layer.
//
// Methods take a postgres.Querier so the service's UnitOfWork decides whether a
// call runs inside a transaction.
type Repository interface {
	Create(ctx context.Context, q postgres.Querier, a *Account) error
	GetByID(ctx context.Context, q postgres.Querier, id uuid.UUID) (*Account, error)
	UpdateBalance(ctx context.Context, q postgres.Querier, id uuid.UUID, newBalance int64) error
}
