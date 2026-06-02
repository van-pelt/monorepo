package postgres

import (
	"context"
	"errors"
	"fmt"

	"github.com/jmoiron/sqlx"
)

// TxFunc runs inside a transaction. Every statement that must be part of the
// transaction has to use the provided Querier.
type TxFunc func(ctx context.Context, q Querier) error

// UnitOfWork executes work within a single DB transaction.
type UnitOfWork struct {
	db *sqlx.DB
}

func NewUnitOfWork(db *sqlx.DB) *UnitOfWork { return &UnitOfWork{db: db} }

// Do runs fn inside a transaction, committing on success and rolling back on
// error or panic. Use InTx instead when the work produces a value.
func (u *UnitOfWork) Do(ctx context.Context, fn TxFunc) error {
	_, err := InTx(ctx, u, func(ctx context.Context, q Querier) (struct{}, error) {
		return struct{}{}, fn(ctx, q)
	})
	return err
}

// InTx runs fn inside a transaction and returns its typed result. It is the
// generic counterpart of UnitOfWork.Do, needed because Go methods cannot have
// their own type parameters.
//
// On a business error the transaction is rolled back. On a failed Commit no
// Rollback is attempted: a failed Commit already terminates the transaction in
// database/sql, so a follow-up Rollback would only return sql.ErrTxDone and
// add noise over the real cause.
func InTx[T any](ctx context.Context, uow *UnitOfWork, fn func(ctx context.Context, q Querier) (T, error)) (T, error) {
	var zero T

	tx, err := uow.db.BeginTxx(ctx, nil)
	if err != nil {
		return zero, fmt.Errorf("begin tx: %w", err)
	}

	// Roll back only on panic; the error and commit paths below are explicit.
	defer func() {
		if p := recover(); p != nil {
			_ = tx.Rollback()
			panic(p)
		}
	}()

	result, err := fn(ctx, tx)
	if err != nil {
		if rbErr := tx.Rollback(); rbErr != nil {
			return zero, errors.Join(err, fmt.Errorf("rollback: %w", rbErr))
		}
		return zero, err
	}

	if err := tx.Commit(); err != nil {
		return zero, fmt.Errorf("commit tx: %w", err)
	}
	return result, nil
}
