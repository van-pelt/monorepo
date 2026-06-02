package idempotency

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	goredis "github.com/redis/go-redis/v9"

	"github.com/monorepo/internal/platform/redis"
)

// inFlightSentinel marks a key as "claimed but not yet completed". Stored
// as a plain string distinguishable from any valid JSON CachedResponse.
const inFlightSentinel = "__in_flight__"

// RedisStorage is the Redis-backed Storage.
type RedisStorage struct {
	rdb *goredis.Client
}

func NewRedisStorage(client *redis.Client) *RedisStorage {
	return &RedisStorage{rdb: client.Raw()}
}

func (s *RedisStorage) Claim(ctx context.Context, key string) (*CachedResponse, error) {
	// SetNX: write sentinel only if key is absent. Distinguishes
	// first-mover (claim succeeded) from contention (claim missed).
	ok, err := s.rdb.SetNX(ctx, key, inFlightSentinel, InFlightTTL).Result()
	if err != nil {
		return nil, fmt.Errorf("setnx %s: %w", key, err)
	}
	if ok {
		return nil, nil
	}

	// Key existed — read what's there. Race window: it may have just
	// expired between SetNX and Get; treat that as "no prior result"
	// and bubble up ErrInFlight conservatively (next attempt will
	// succeed).
	val, err := s.rdb.Get(ctx, key).Result()
	if errors.Is(err, goredis.Nil) {
		return nil, ErrInFlight
	}
	if err != nil {
		return nil, fmt.Errorf("get %s: %w", key, err)
	}
	if val == inFlightSentinel {
		return nil, ErrInFlight
	}

	var resp CachedResponse
	if err := json.Unmarshal([]byte(val), &resp); err != nil {
		return nil, fmt.Errorf("decode cached response: %w", err)
	}
	return &resp, nil
}

func (s *RedisStorage) Store(ctx context.Context, key string, resp *CachedResponse, ttl time.Duration) error {
	data, err := json.Marshal(resp)
	if err != nil {
		return fmt.Errorf("encode response: %w", err)
	}
	if err := s.rdb.Set(ctx, key, data, ttl).Err(); err != nil {
		return fmt.Errorf("set %s: %w", key, err)
	}
	return nil
}

func (s *RedisStorage) Release(ctx context.Context, key string) error {
	if err := s.rdb.Del(ctx, key).Err(); err != nil {
		return fmt.Errorf("del %s: %w", key, err)
	}
	return nil
}
