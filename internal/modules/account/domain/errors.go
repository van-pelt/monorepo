package domain

import "github.com/monorepo/internal/shared/apperror"

var (
	ErrAccountNotFound = apperror.NotFound("account not found")
	ErrInvalidOwner    = apperror.Invalid("owner id is required")
	ErrInvalidCurrency = apperror.Invalid("currency must be a 3-letter code")
	ErrInsufficient    = apperror.Conflict("insufficient funds")
)
