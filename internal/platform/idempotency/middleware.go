package idempotency

import (
	"errors"
	"fmt"
	"net/http"

	"github.com/gofiber/fiber/v2"
	"github.com/rs/zerolog"
)

// HeaderName is the request header carrying the client-chosen idempotency
// key. Stripe uses the same name and most SDKs respect it.
const HeaderName = "Idempotency-Key"

// Middleware enforces idempotency for mutating methods. GET/HEAD pass
// through untouched. Requests without an Idempotency-Key also pass through
// — sending one is opt-in for the client.
//
// On any Storage error the middleware fails open (logs + passthrough): a
// broken Redis must not stop business traffic.
func Middleware(storage Storage, log zerolog.Logger) fiber.Handler {
	l := log.With().Str("component", "idempotency").Logger()
	return func(c *fiber.Ctx) error {
		if !isMutating(c.Method()) {
			return c.Next()
		}
		header := c.Get(HeaderName)
		if header == "" {
			return c.Next()
		}

		key := buildKey(c.Method(), c.Route().Path, header)
		ctx := c.UserContext()

		cached, err := storage.Claim(ctx, key)
		switch {
		case errors.Is(err, ErrInFlight):
			return c.Status(fiber.StatusConflict).JSON(fiber.Map{
				"error": "request with this Idempotency-Key is already in progress",
			})
		case err != nil:
			l.Warn().Err(err).Str("key", key).Msg("claim failed; failing open")
			return c.Next()
		case cached != nil:
			c.Set(fiber.HeaderContentType, cached.ContentType)
			return c.Status(cached.Status).Send(cached.Body)
		}

		// Claim succeeded — process the request. After the handler we
		// either Store the response (2xx-4xx) or Release the sentinel
		// (5xx) so the client can retry without waiting for the TTL.
		nextErr := c.Next()

		status := c.Response().StatusCode()
		if status >= http.StatusInternalServerError {
			if rErr := storage.Release(ctx, key); rErr != nil {
				l.Warn().Err(rErr).Str("key", key).Msg("release failed")
			}
			return nextErr
		}

		// Body bytes may live in a Fiber-owned buffer that gets reused
		// after the response is flushed. Copy to a fresh slice.
		body := append([]byte(nil), c.Response().Body()...)
		resp := &CachedResponse{
			Status:      status,
			ContentType: string(c.Response().Header.ContentType()),
			Body:        body,
		}
		if sErr := storage.Store(ctx, key, resp, DefaultCacheTTL); sErr != nil {
			l.Warn().Err(sErr).Str("key", key).Msg("store failed; response sent but not cached")
		}
		return nextErr
	}
}

func isMutating(method string) bool {
	switch method {
	case fiber.MethodPost, fiber.MethodPut, fiber.MethodPatch, fiber.MethodDelete:
		return true
	}
	return false
}

func buildKey(method, route, header string) string {
	if route == "" {
		route = "unknown"
	}
	return fmt.Sprintf("idempotency:%s:%s:%s", method, route, header)
}
