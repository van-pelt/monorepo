// Package domain holds the account module's entities and ports. It has no
// dependency on transport or any concrete infrastructure.
package domain

import (
	"time"

	"github.com/google/uuid"
)

// Account is the aggregate root. Balance is stored in minor units (e.g. cents)
// as an integer to avoid floating-point errors in monetary calculations.
type Account struct {
	ID        uuid.UUID
	OwnerID   uuid.UUID
	Currency  string
	Balance   int64
	CreatedAt time.Time
	UpdatedAt time.Time
}

// NewAccount creates a valid account, enforcing domain invariants.
func NewAccount(ownerID uuid.UUID, currency string) (*Account, error) {
	if ownerID == uuid.Nil {
		return nil, ErrInvalidOwner
	}
	if len(currency) != 3 {
		return nil, ErrInvalidCurrency
	}
	now := time.Now().UTC()
	return &Account{
		ID:        uuid.New(),
		OwnerID:   ownerID,
		Currency:  currency,
		Balance:   0,
		CreatedAt: now,
		UpdatedAt: now,
	}, nil
}

// CanDebit reports whether the account holds enough funds for the amount.
func (a *Account) CanDebit(amount int64) bool {
	return a.Balance >= amount
}
