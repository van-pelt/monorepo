package postgres

import "github.com/jmoiron/sqlx"

// Querier is satisfied by both *sqlx.DB and *sqlx.Tx. Repositories accept a
// Querier so they don't need to know whether they run inside a transaction —
// the UnitOfWork decides what to pass in.
type Querier = sqlx.ExtContext
