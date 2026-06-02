// Package postgres provides the shared database connection pool, a Querier
// abstraction and a Unit of Work for transactional consistency.
package postgres

import (
	"context"
	"fmt"
	"net/url"
	"strconv"
	"strings"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib" // registers the "pgx" database/sql driver
	"github.com/jmoiron/sqlx"
)

type Config struct {
	DSN             string
	MaxOpenConns    int
	MaxIdleConns    int
	ConnMaxLifetime time.Duration
	// StatementTimeout, when > 0, is appended to the DSN as the postgres
	// `statement_timeout` parameter. The driver applies it on every new
	// connection, so any query (inside the app) is killed server-side after
	// this duration. cmd/migrate uses sql.Open directly with the raw DSN and
	// is therefore unaffected — migrations can run as long as they need.
	// If the DSN already contains statement_timeout, it is left untouched.
	StatementTimeout time.Duration
}

// Connect opens a pooled connection to Postgres via the pgx stdlib driver and
// verifies it with a ping.
func Connect(ctx context.Context, cfg Config) (*sqlx.DB, error) {
	dsn, err := applyStatementTimeout(cfg.DSN, cfg.StatementTimeout)
	if err != nil {
		return nil, fmt.Errorf("apply statement_timeout to dsn: %w", err)
	}

	db, err := sqlx.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("open db: %w", err)
	}
	db.SetMaxOpenConns(cfg.MaxOpenConns)
	db.SetMaxIdleConns(cfg.MaxIdleConns)
	db.SetConnMaxLifetime(cfg.ConnMaxLifetime)

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := db.PingContext(pingCtx); err != nil {
		return nil, fmt.Errorf("ping db: %w", err)
	}
	return db, nil
}

// applyStatementTimeout appends `statement_timeout=<ms>` to the DSN query
// parameters when the caller asked for one and the DSN does not already
// specify it. Pgx accepts statement_timeout as a runtime parameter in the
// URL — it sets it on every new connection's session.
func applyStatementTimeout(dsn string, timeout time.Duration) (string, error) {
	if timeout <= 0 {
		return dsn, nil
	}
	if strings.Contains(dsn, "statement_timeout") {
		// User has set it explicitly — respect their value.
		return dsn, nil
	}
	u, err := url.Parse(dsn)
	if err != nil {
		return "", err
	}
	q := u.Query()
	q.Set("statement_timeout", strconv.Itoa(int(timeout.Milliseconds())))
	u.RawQuery = q.Encode()
	return u.String(), nil
}
