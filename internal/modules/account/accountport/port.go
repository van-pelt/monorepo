// Package accountport is the PUBLIC contract of the account module.
//
// Other modules may import ONLY this package — never account's domain,
// service or repository. It is a dependency-free leaf package, which keeps the
// module graph acyclic even when two modules reference each other's ports.
package accountport

import (
	"context"

	"github.com/google/uuid"

	"github.com/monorepo/internal/shared/apperror"
)

// ErrAccountNotFound is returned by AccountProvider when the account is absent.
var ErrAccountNotFound = apperror.NotFound("account not found")

// AccountInfo is the read-only projection the account module exposes to others.
type AccountInfo struct {
	ID       uuid.UUID
	OwnerID  uuid.UUID
	Currency string
}

// AccountProvider is the synchronous port other modules call to query accounts.
type AccountProvider interface {
	GetByID(ctx context.Context, id uuid.UUID) (AccountInfo, error)
}
