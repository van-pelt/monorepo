// Package repository implements the account domain's persistence port on
// Postgres. All tables live in the dedicated "account" schema.
package repository

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"

	"github.com/monorepo/internal/modules/account/internal/domain"
	"github.com/monorepo/internal/platform/apperror"
	"github.com/monorepo/internal/platform/postgres"
)

type AccountRepository struct{}

func NewAccountRepository() *AccountRepository { return &AccountRepository{} }

// accountRow is the DB-shaped representation, mapped to/from the domain entity.
type accountRow struct {
	ID        uuid.UUID `db:"id"`
	OwnerID   uuid.UUID `db:"owner_id"`
	Currency  string    `db:"currency"`
	Balance   int64     `db:"balance"`
	CreatedAt time.Time `db:"created_at"`
	UpdatedAt time.Time `db:"updated_at"`
}

func (r accountRow) toDomain() *domain.Account {
	return &domain.Account{
		ID:        r.ID,
		OwnerID:   r.OwnerID,
		Currency:  r.Currency,
		Balance:   r.Balance,
		CreatedAt: r.CreatedAt,
		UpdatedAt: r.UpdatedAt,
	}
}

func (r *AccountRepository) Create(ctx context.Context, q postgres.Querier, a *domain.Account) error {
	const query = `
		INSERT INTO account.accounts (id, owner_id, currency, balance, created_at, updated_at)
		VALUES ($1, $2, $3, $4, $5, $6)`
	if _, err := q.ExecContext(ctx, query,
		a.ID, a.OwnerID, a.Currency, a.Balance, a.CreatedAt, a.UpdatedAt); err != nil {
		return apperror.Wrap("insert account", err)
	}
	return nil
}

func (r *AccountRepository) GetByID(ctx context.Context, q postgres.Querier, id uuid.UUID) (*domain.Account, error) {
	const query = `
		SELECT id, owner_id, currency, balance, created_at, updated_at
		FROM account.accounts
		WHERE id = $1`
	var row accountRow
	if err := sqlx.GetContext(ctx, q, &row, query, id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, domain.ErrAccountNotFound
		}
		return nil, apperror.Wrap("select account", err)
	}
	return row.toDomain(), nil
}

func (r *AccountRepository) UpdateBalance(ctx context.Context, q postgres.Querier, id uuid.UUID, newBalance int64) error {
	const query = `UPDATE account.accounts SET balance = $2, updated_at = now() WHERE id = $1`
	res, err := q.ExecContext(ctx, query, id, newBalance)
	if err != nil {
		return apperror.Wrap("update balance", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return domain.ErrAccountNotFound
	}
	return nil
}
