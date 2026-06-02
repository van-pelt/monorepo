// Package crons schedules recurring background jobs and serializes their
// execution across pods via Postgres advisory locks.
//
// Leadership model: every tick a job tries pg_try_advisory_lock(<key>) on a
// dedicated connection. Whichever pod gets the lock runs the job; the
// others skip the tick and the next opportunity comes on the next
// schedule fire. On crash the connection closes and Postgres releases the
// lock automatically — there are no stale locks to clean up.
//
// The framework is wired up in cmd/api/main.go but no concrete jobs are
// registered yet. Add a Register(...) call where a recurring task is
// needed (typically a module's New()).
package crons

import (
	"context"
	"errors"
	"fmt"
	"hash/fnv"
	"time"

	"github.com/jmoiron/sqlx"
	"github.com/robfig/cron/v3"
	"github.com/rs/zerolog"
)

// jobTimeout caps a single execution. A long-running job holds the
// Postgres advisory lock (and therefore one DB connection) for its full
// duration — keep jobs bounded or split them.
const jobTimeout = 5 * time.Minute

// shutdownTimeout is how long Run waits for in-flight jobs to finish
// after ctx is cancelled.
const shutdownTimeout = 30 * time.Second

// Job is the user-supplied work. It receives a derived ctx with the
// per-execution timeout already applied; respect it for graceful
// cancellation.
type Job func(ctx context.Context) error

// Scheduler is the registry + driver. Construct once via NewScheduler,
// Register jobs at wiring time, then run via go Scheduler.Run(ctx).
type Scheduler struct {
	db   *sqlx.DB
	log  zerolog.Logger
	cron *cron.Cron
}

func NewScheduler(db *sqlx.DB, log zerolog.Logger) *Scheduler {
	l := log.With().Str("component", "crons").Logger()
	// SkipIfStillRunning: if a previous tick's run hasn't finished by the
	// next fire, drop the new run (better than queueing and getting
	// behind). Recover wraps job invocations so a panic in one job does
	// not crash the cron goroutine.
	c := cron.New(cron.WithChain(
		cron.Recover(cronLogger{log: l}),
		cron.SkipIfStillRunning(cronLogger{log: l}),
	))
	return &Scheduler{db: db, log: l, cron: c}
}

// Register adds a job to run on schedule (standard 5-field cron, e.g.
// "0 3 * * *" for daily at 03:00). Returns an error if the schedule is
// malformed; safe to call only before Run.
func (s *Scheduler) Register(name, schedule string, job Job) error {
	if name == "" {
		return errors.New("crons: empty job name")
	}
	if _, err := s.cron.AddFunc(schedule, func() { s.runWithLock(name, job) }); err != nil {
		return fmt.Errorf("crons: schedule %q for %s: %w", schedule, name, err)
	}
	s.log.Info().Str("job", name).Str("schedule", schedule).Msg("job registered")
	return nil
}

// Run starts the cron loop and blocks until ctx is cancelled. On shutdown
// it waits up to shutdownTimeout for in-flight jobs to finish before
// returning.
func (s *Scheduler) Run(ctx context.Context) error {
	s.cron.Start()
	<-ctx.Done()

	stopCtx := s.cron.Stop()
	select {
	case <-stopCtx.Done():
		return nil
	case <-time.After(shutdownTimeout):
		s.log.Warn().Dur("waited", shutdownTimeout).Msg("crons shutdown timed out")
		return errors.New("crons: shutdown timed out")
	}
}

// runWithLock is the per-tick wrapper around a Job: acquires a Postgres
// advisory lock on a dedicated connection so only one pod runs the job,
// invokes the Job, releases the lock and the connection.
func (s *Scheduler) runWithLock(name string, job Job) {
	ctx, cancel := context.WithTimeout(context.Background(), jobTimeout)
	defer cancel()

	conn, err := s.db.Connx(ctx)
	if err != nil {
		s.log.Error().Err(err).Str("job", name).Msg("acquire connection")
		return
	}
	defer func() { _ = conn.Close() }()

	key := jobKey(name)
	var locked bool
	if err := conn.QueryRowxContext(ctx, "SELECT pg_try_advisory_lock($1)", key).Scan(&locked); err != nil {
		s.log.Error().Err(err).Str("job", name).Msg("advisory lock query")
		return
	}
	if !locked {
		s.log.Debug().Str("job", name).Msg("skipped: another pod holds the lock")
		return
	}
	defer func() {
		// Release on the same connection. If the conn dropped already
		// Postgres will reap the lock on session close anyway, so we
		// log + ignore.
		releaseCtx, releaseCancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer releaseCancel()
		if _, err := conn.ExecContext(releaseCtx, "SELECT pg_advisory_unlock($1)", key); err != nil {
			s.log.Warn().Err(err).Str("job", name).Msg("advisory unlock failed (will auto-release on conn close)")
		}
	}()

	s.log.Info().Str("job", name).Msg("job started")
	start := time.Now()
	if err := job(ctx); err != nil {
		s.log.Error().Err(err).Str("job", name).Dur("duration", time.Since(start)).Msg("job failed")
		return
	}
	s.log.Info().Str("job", name).Dur("duration", time.Since(start)).Msg("job done")
}

// jobKey hashes the name into the int64 keyspace pg_try_advisory_lock
// expects. FNV-64a is fast and good enough — collisions across distinct
// job names are vanishingly unlikely in practice.
func jobKey(name string) int64 {
	h := fnv.New64a()
	_, _ = h.Write([]byte(name))
	return int64(h.Sum64())
}

// cronLogger adapts our zerolog to robfig/cron's logger interface — used
// by the Recover/SkipIfStillRunning middleware to report panics and
// skipped runs.
type cronLogger struct{ log zerolog.Logger }

func (l cronLogger) Info(msg string, keysAndValues ...any) {
	l.log.Info().Fields(toFields(keysAndValues)).Msg(msg)
}

func (l cronLogger) Error(err error, msg string, keysAndValues ...any) {
	l.log.Error().Err(err).Fields(toFields(keysAndValues)).Msg(msg)
}

func toFields(kv []any) map[string]any {
	out := make(map[string]any, len(kv)/2)
	for i := 0; i+1 < len(kv); i += 2 {
		key, ok := kv[i].(string)
		if !ok {
			continue
		}
		out[key] = kv[i+1]
	}
	return out
}
