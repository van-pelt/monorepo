// Package paymentport is the PUBLIC contract of the payment module.
//
// Like accountport, this is a dependency-free leaf package. Event types that
// cross module boundaries live here so consumers can decode them without
// importing the payment module's internals (which would create an import
// cycle, since payment imports accountport).
package paymentport

import "github.com/google/uuid"

// TopicPaymentCreated is the event topic emitted when a payment is registered.
const TopicPaymentCreated = "payment.created"

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
