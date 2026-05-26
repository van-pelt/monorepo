// Package domain holds the payment module's entities and ports.
package domain

import (
	"time"

	"github.com/google/uuid"
)

// Status is the lifecycle state of a payment.
type Status string

const (
	StatusPending   Status = "pending"
	StatusCompleted Status = "completed"
	StatusFailed    Status = "failed"
)

// Payment is the aggregate root of the payment module. Amount is in minor
// units (e.g. cents).
type Payment struct {
	ID            uuid.UUID
	FromAccountID uuid.UUID
	ToAccountID   uuid.UUID
	Amount        int64
	Currency      string
	Status        Status
	CreatedAt     time.Time
}

// NewPayment creates a valid payment in the pending state.
func NewPayment(from, to uuid.UUID, amount int64, currency string) (*Payment, error) {
	if from == uuid.Nil || to == uuid.Nil {
		return nil, ErrInvalidAccount
	}
	if from == to {
		return nil, ErrSameAccount
	}
	if amount <= 0 {
		return nil, ErrInvalidAmount
	}
	return &Payment{
		ID:            uuid.New(),
		FromAccountID: from,
		ToAccountID:   to,
		Amount:        amount,
		Currency:      currency,
		Status:        StatusPending,
		CreatedAt:     time.Now().UTC(),
	}, nil
}
