// Command migrate applies all SQL migrations: the platform base layer first
// (schemas + outbox), then each business module against its own schema. Each
// module tracks its goose version in its own goose_db_version table inside
// the module's schema, so modules evolve independently.
//
// Run before starting cmd/api. In production this is a one-shot k8s Job per
// release; in local dev: `make migrate`.
//
// Usage: migrate [-config path] [up|down|status]
package main

import (
	"context"
	"database/sql"
	"flag"
	"fmt"
	"io/fs"
	"os"
	"sort"
	"time"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/pressly/goose/v3"

	"github.com/monorepo/internal/platform/config"
	"github.com/monorepo/internal/platform/observability/logger"
	"github.com/monorepo/internal/platform/security"
	"github.com/monorepo/migrations"
)

// baseDir is the subdirectory holding platform-level migrations (schemas +
// shared outbox). It is applied first; its goose version table lives in the
// public schema.
const baseDir = "base"

func main() {
	configPath := flag.String("config", "config/config.yaml", "path to config")
	flag.Parse()

	command := flag.Arg(0)
	if command == "" {
		command = "up"
	}

	cfg, err := config.Load(*configPath)
	if err != nil {
		exitf("load config: %v", err)
	}
	if err := security.ResolveSecrets(context.Background(), cfg, &security.EnvSecretsProvider{}); err != nil {
		exitf("resolve secrets: %v", err)
	}
	log := logger.New(cfg.Log.Level, cfg.Env)

	db, err := sql.Open("pgx", cfg.DB.DSN)
	if err != nil {
		exitf("open db: %v", err)
	}
	defer db.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	if err := db.PingContext(ctx); err != nil {
		exitf("ping db: %v", err)
	}

	if err := goose.SetDialect("postgres"); err != nil {
		exitf("set dialect: %v", err)
	}

	modules, err := listModules(migrations.FS)
	if err != nil {
		exitf("list modules: %v", err)
	}

	for _, name := range modules {
		schema := name
		versionTable := schema + ".goose_db_version"
		if name == baseDir {
			versionTable = "public.goose_db_version"
		}
		sub, err := fs.Sub(migrations.FS, name)
		if err != nil {
			exitf("sub fs %s: %v", name, err)
		}
		goose.SetBaseFS(sub)
		goose.SetTableName(versionTable)
		if err := goose.RunContext(ctx, command, db, "."); err != nil {
			exitf("%s %s: %v", name, command, err)
		}
		log.Info().Str("module", name).Str("command", command).Msg("migrations applied")
	}
}

// listModules returns the top-level subdirectories of fsys in the order the
// migrator should apply them: base first, then the rest alphabetically.
func listModules(fsys fs.FS) ([]string, error) {
	entries, err := fs.ReadDir(fsys, ".")
	if err != nil {
		return nil, err
	}
	var others []string
	hasBase := false
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		if e.Name() == baseDir {
			hasBase = true
			continue
		}
		others = append(others, e.Name())
	}
	sort.Strings(others)
	if hasBase {
		return append([]string{baseDir}, others...), nil
	}
	return others, nil
}

func exitf(format string, a ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", a...)
	os.Exit(1)
}
