// Package redis is a thin wrapper around go-redis. Other platform packages
// (currently only idempotency) consume *Client; the wrapper exists so we
// own the lifecycle (Connect/Close), the readiness probe, and the
// structured logger setup in one place.
//
// Redis is optional: when cfg.Redis.DSN is empty the composition root
// skips Connect entirely and any feature that needs Redis is wired to a
// nil storage (degrades to no-op).
package redis

import (
	"context"
	"fmt"
	"time"

	goredis "github.com/redis/go-redis/v9"
	"github.com/rs/zerolog"
)

// Config carries the DSN. Empty DSN means Redis is disabled — call sites
// should not call Connect in that case.
type Config struct {
	DSN string
}

// Client wraps a *redis.Client.
type Client struct {
	rdb *goredis.Client
	log zerolog.Logger
}

// Connect parses the DSN, opens the pool and pings to verify connectivity.
// Fast-fails if Redis is unreachable, matching how postgres + rabbitmq
// behave at startup.
func Connect(ctx context.Context, cfg Config, log zerolog.Logger) (*Client, error) {
	opts, err := goredis.ParseURL(cfg.DSN)
	if err != nil {
		return nil, fmt.Errorf("parse redis dsn: %w", err)
	}
	rdb := goredis.NewClient(opts)

	pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	if err := rdb.Ping(pingCtx).Err(); err != nil {
		_ = rdb.Close()
		return nil, fmt.Errorf("ping redis: %w", err)
	}
	return &Client{
		rdb: rdb,
		log: log.With().Str("component", "redis").Logger(),
	}, nil
}

// Raw returns the underlying *redis.Client for callers that need the full
// API (e.g. idempotency.RedisStorage uses SetNX, Get, Del).
func (c *Client) Raw() *goredis.Client { return c.rdb }

// HealthCheck pings Redis. Used by /readyz; returns the underlying error
// so the operator sees why traffic was gated.
func (c *Client) HealthCheck(ctx context.Context) error {
	return c.rdb.Ping(ctx).Err()
}

func (c *Client) Close() error { return c.rdb.Close() }
