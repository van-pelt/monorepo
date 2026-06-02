// Package idempotency provides safe-retry semantics for mutating HTTP
// endpoints via the `Idempotency-Key` header.
//
// Flow per request (POST/PUT/PATCH/DELETE with non-empty key):
//
//  1. Compute key = idempotency:<method>:<route>:<header>.
//  2. Claim: try SETNX with an "in_flight" sentinel + 60s TTL.
//     - Success → process the request; on completion Store(response, 24h)
//     overwrites the sentinel. On 5xx → Release deletes the sentinel
//     so the client can retry immediately.
//     - Already in flight (sentinel still present) → respond 409.
//     - Already cached → return the cached response without invoking
//     the handler.
//
// Failure mode is fail-open: any Storage error degrades the middleware
// to a passthrough — Redis being down must not block business traffic.
package idempotency

import (
	"context"
	"errors"
	"time"
)

// DefaultCacheTTL is how long a finished response is kept. Matches Stripe's
// 24h convention — long enough for client retry strategies, short enough
// to bound Redis footprint.
const DefaultCacheTTL = 24 * time.Hour

// InFlightTTL is the safety expiry on the in-flight sentinel. If the
// server crashes between Claim and Store, the key auto-expires after
// this duration and the next attempt can retry. Should comfortably
// exceed the typical request latency.
const InFlightTTL = 60 * time.Second

// ErrInFlight is returned by Storage.Claim when another request is
// currently processing the same key. The middleware translates it into
// HTTP 409 Conflict.
var ErrInFlight = errors.New("idempotency: request already in progress")

// CachedResponse is what we store and replay. Headers beyond Content-Type
// are intentionally not preserved — keep cardinality low and serialization
// straightforward.
type CachedResponse struct {
	Status      int    `json:"status"`
	ContentType string `json:"content_type"`
	Body        []byte `json:"body"`
}

// Storage is the persistence contract. RedisStorage is the production
// implementation; tests can supply an in-memory fake.
type Storage interface {
	// Claim attempts to mark key as in-flight.
	//   (nil, nil)        — claim succeeded; caller processes the request.
	//   (*CachedResponse) — claim missed because the request already
	//                       completed previously; caller returns this.
	//   (_, ErrInFlight)  — another request is processing right now.
	//   (_, other error)  — storage failure; caller should fail open.
	Claim(ctx context.Context, key string) (*CachedResponse, error)

	// Store overwrites the sentinel with the actual response and resets
	// the TTL to DefaultCacheTTL.
	Store(ctx context.Context, key string, resp *CachedResponse, ttl time.Duration) error

	// Release deletes the key entirely — used on 5xx responses so the
	// client can retry without waiting for the InFlightTTL to expire.
	Release(ctx context.Context, key string) error
}
