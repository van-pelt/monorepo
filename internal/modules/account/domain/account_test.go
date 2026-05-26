package domain_test

import (
	"testing"

	"github.com/google/uuid"

	"github.com/monorepo/internal/modules/account/domain"
)

func TestNewAccount(t *testing.T) {
	t.Run("rejects nil owner", func(t *testing.T) {
		if _, err := domain.NewAccount(uuid.Nil, "USD"); err == nil {
			t.Fatal("expected error for nil owner")
		}
	})

	t.Run("rejects invalid currency", func(t *testing.T) {
		if _, err := domain.NewAccount(uuid.New(), "DOLLAR"); err == nil {
			t.Fatal("expected error for invalid currency")
		}
	})

	t.Run("creates a valid account with zero balance", func(t *testing.T) {
		acc, err := domain.NewAccount(uuid.New(), "USD")
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if acc.Balance != 0 {
			t.Fatalf("expected zero balance, got %d", acc.Balance)
		}
	})
}

func TestAccountCanDebit(t *testing.T) {
	acc := &domain.Account{Balance: 100}
	if !acc.CanDebit(100) {
		t.Fatal("expected debit of full balance to be allowed")
	}
	if acc.CanDebit(101) {
		t.Fatal("expected debit above balance to be rejected")
	}
}
