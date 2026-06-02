// Package migrations embeds the SQL migrations for the platform base layer
// (schemas + outbox) and each business module. cmd/migrate reads from FS and
// applies them per-schema, tracking each module's version in its own schema.
//
// When you add a new module, add a //go:embed line for its subdirectory below.
package migrations

import "embed"

// FS holds every module's migrations, keyed by top-level subdirectory name:
//
//	base    — platform-level schema creation + shared outbox tables
//	account — account schema migrations
//	payment — payment schema migrations
//
//go:embed all:base
//go:embed all:account
//go:embed all:payment
var FS embed.FS
