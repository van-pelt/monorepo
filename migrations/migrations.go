// Package migrations embeds the base SQL migrations that create the per-module
// schemas and the shared outbox table.
package migrations

import "embed"

// FS holds the base migrations, applied before any module's migrations.
//
//go:embed *.sql
var FS embed.FS
