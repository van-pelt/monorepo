package domain

import (
	"context"

	"github.com/google/uuid"

	"github.com/monorepo/internal/platform/postgres"
)

// Repository is the persistence port for payments.
type Repository interface {
	Create(ctx context.Context, q postgres.Querier, p *Payment) error
	GetByID(ctx context.Context, q postgres.Querier, id uuid.UUID) (*Payment, error)
}
