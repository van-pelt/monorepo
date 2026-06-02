// Package repository implements the payment domain's persistence port on
// Postgres. All tables live in the dedicated "payment" schema.
package repository

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jmoiron/sqlx"

	"github.com/monorepo/internal/modules/payment/internal/domain"
	"github.com/monorepo/internal/platform/apperror"
	"github.com/monorepo/internal/platform/postgres"
)

type PaymentRepository struct{}

func NewPaymentRepository() *PaymentRepository { return &PaymentRepository{} }

type paymentRow struct {
	ID            uuid.UUID `db:"id"`
	FromAccountID uuid.UUID `db:"from_account_id"`
	ToAccountID   uuid.UUID `db:"to_account_id"`
	Amount        int64     `db:"amount"`
	Currency      string    `db:"currency"`
	Status        string    `db:"status"`
	CreatedAt     time.Time `db:"created_at"`
}

func (r paymentRow) toDomain() *domain.Payment {
	return &domain.Payment{
		ID:            r.ID,
		FromAccountID: r.FromAccountID,
		ToAccountID:   r.ToAccountID,
		Amount:        r.Amount,
		Currency:      r.Currency,
		Status:        domain.Status(r.Status),
		CreatedAt:     r.CreatedAt,
	}
}

func (r *PaymentRepository) Create(ctx context.Context, q postgres.Querier, p *domain.Payment) error {
	const query = `
		INSERT INTO payment.payments
			(id, from_account_id, to_account_id, amount, currency, status, created_at)
		VALUES ($1, $2, $3, $4, $5, $6, $7)`
	if _, err := q.ExecContext(ctx, query,
		p.ID, p.FromAccountID, p.ToAccountID, p.Amount, p.Currency, string(p.Status), p.CreatedAt); err != nil {
		return apperror.Wrap("insert payment", err)
	}
	return nil
}

func (r *PaymentRepository) GetByID(ctx context.Context, q postgres.Querier, id uuid.UUID) (*domain.Payment, error) {
	const query = `
		SELECT id, from_account_id, to_account_id, amount, currency, status, created_at
		FROM payment.payments
		WHERE id = $1`
	var row paymentRow
	if err := sqlx.GetContext(ctx, q, &row, query, id); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return nil, domain.ErrPaymentNotFound
		}
		return nil, apperror.Wrap("select payment", err)
	}
	return row.toDomain(), nil
}
