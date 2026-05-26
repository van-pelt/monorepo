package postgres_test

import (
	"context"
	"errors"
	"testing"

	"github.com/DATA-DOG/go-sqlmock"
	"github.com/jmoiron/sqlx"

	"github.com/monorepo/internal/shared/postgres"
)

func newMockUoW(t *testing.T) (*postgres.UnitOfWork, sqlmock.Sqlmock) {
	t.Helper()
	db, mock, err := sqlmock.New()
	if err != nil {
		t.Fatalf("sqlmock.New: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return postgres.NewUnitOfWork(sqlx.NewDb(db, "sqlmock")), mock
}

func TestUnitOfWork_Do_Commit(t *testing.T) {
	uow, mock := newMockUoW(t)
	mock.ExpectBegin()
	mock.ExpectCommit()

	err := uow.Do(context.Background(), func(context.Context, postgres.Querier) error {
		return nil
	})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestUnitOfWork_Do_RollbackOnError(t *testing.T) {
	uow, mock := newMockUoW(t)
	mock.ExpectBegin()
	mock.ExpectRollback()

	wantErr := errors.New("business failure")
	err := uow.Do(context.Background(), func(context.Context, postgres.Querier) error {
		return wantErr
	})
	if !errors.Is(err, wantErr) {
		t.Fatalf("expected %v, got %v", wantErr, err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestUnitOfWork_Do_NoRollbackAfterFailedCommit(t *testing.T) {
	// A failed Commit already terminates the transaction; a follow-up Rollback
	// would only return sql.ErrTxDone. This test pins that behaviour: only
	// ExpectCommit (with error) is set and ExpectRollback is NOT, so if the
	// code wrongly called Rollback, ExpectationsWereMet would fail.
	uow, mock := newMockUoW(t)
	commitErr := errors.New("commit boom")
	mock.ExpectBegin()
	mock.ExpectCommit().WillReturnError(commitErr)

	err := uow.Do(context.Background(), func(context.Context, postgres.Querier) error {
		return nil
	})
	if !errors.Is(err, commitErr) {
		t.Fatalf("expected commit error, got %v", err)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestInTx_ReturnsValue(t *testing.T) {
	uow, mock := newMockUoW(t)
	mock.ExpectBegin()
	mock.ExpectCommit()

	got, err := postgres.InTx(context.Background(), uow,
		func(context.Context, postgres.Querier) (int, error) {
			return 42, nil
		})
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if got != 42 {
		t.Fatalf("expected 42, got %d", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}

func TestInTx_RollbackOnError_ReturnsZeroValue(t *testing.T) {
	uow, mock := newMockUoW(t)
	mock.ExpectBegin()
	mock.ExpectRollback()

	wantErr := errors.New("boom")
	got, err := postgres.InTx(context.Background(), uow,
		func(context.Context, postgres.Querier) (int, error) {
			return 7, wantErr // non-zero value must not leak out with an error
		})
	if !errors.Is(err, wantErr) {
		t.Fatalf("expected %v, got %v", wantErr, err)
	}
	if got != 0 {
		t.Fatalf("expected zero value on error, got %d", got)
	}
	if err := mock.ExpectationsWereMet(); err != nil {
		t.Fatalf("unmet expectations: %v", err)
	}
}
