package domain

import "github.com/monorepo/internal/shared/apperror"

var (
	ErrPaymentNotFound  = apperror.NotFound("payment not found")
	ErrInvalidAccount   = apperror.Invalid("from and to accounts are required")
	ErrSameAccount      = apperror.Invalid("cannot transfer to the same account")
	ErrInvalidAmount    = apperror.Invalid("amount must be positive")
	ErrCurrencyMismatch = apperror.Conflict("account currencies do not match")
)
