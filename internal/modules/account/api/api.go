// Package api is the PUBLIC contract of the account module.
//
// Other modules may import ONLY this package — never account's internal
// domain, service or repository (the Go internal/ mechanism enforces this).
// It is a dependency-free leaf package: HTTP handlers in cmd/api, other
// modules (through the in-process adapter) and any future gRPC client all
// depend on the same Service interface and DTOs declared here.
package api

import (
	"context"
	"time"

	"github.com/google/uuid"

	"github.com/monorepo/internal/platform/apperror"
)

// ErrAccountNotFound is returned by Service when the account does not exist.
// HTTP layer maps it to 404; future gRPC adapters map to codes.NotFound.
var ErrAccountNotFound = apperror.NotFound("account not found")

// Account is the public projection of an account. The same shape is returned
// to HTTP clients (as JSON) and to other modules through the in-process port.
type Account struct {
	ID        uuid.UUID
	OwnerID   uuid.UUID
	Currency  string
	Balance   int64
	CreatedAt time.Time
	UpdatedAt time.Time
}

// CreateAccountInput is the vocabulary of a create-account request, used by
// callers (HTTP handlers, other modules) instead of positional arguments.
type CreateAccountInput struct {
	OwnerID  uuid.UUID
	Currency string
}

// Service is the account module's synchronous public API. All consumers
// depend on this interface; the in-process implementation lives in
// module.go's adapter, a future gRPC client would implement the same
// interface against a remote service.
type Service interface {
	CreateAccount(ctx context.Context, in CreateAccountInput) (Account, error)
	GetByID(ctx context.Context, id uuid.UUID) (Account, error)
}
