// Package api is the PUBLIC contract of the payment module.
//
// Like account/api, this is a dependency-free leaf package. Event types that
// cross module boundaries live here so consumers can decode them without
// importing the payment module's internals.
package api

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/monorepo/internal/platform/apperror"
)

// TopicPaymentCreated is the event topic emitted when a payment is registered.
const TopicPaymentCreated = "payment.created"

// ErrPaymentNotFound is returned by Service when the payment does not exist.
var ErrPaymentNotFound = apperror.NotFound("payment not found")

// Payment is the public projection of a payment.
type Payment struct {
	ID            uuid.UUID
	FromAccountID uuid.UUID
	ToAccountID   uuid.UUID
	Amount        int64
	Currency      string
	Status        string
	CreatedAt     time.Time
}

// CreatePaymentInput is the vocabulary of a create-payment request.
type CreatePaymentInput struct {
	FromAccountID uuid.UUID
	ToAccountID   uuid.UUID
	Amount        int64
}

// Service is the payment module's synchronous public API.
type Service interface {
	CreatePayment(ctx context.Context, in CreatePaymentInput) (Payment, error)
	GetByID(ctx context.Context, id uuid.UUID) (Payment, error)
}

// PaymentCreated is the public event contract. It is serialized into the
// outbox and consumed by other modules (the account module settles the
// funds transfer when it receives this event).
type PaymentCreated struct {
	PaymentID     uuid.UUID `json:"payment_id"`
	FromAccountID uuid.UUID `json:"from_account_id"`
	ToAccountID   uuid.UUID `json:"to_account_id"`
	Amount        int64     `json:"amount"`
	Currency      string    `json:"currency"`
}
