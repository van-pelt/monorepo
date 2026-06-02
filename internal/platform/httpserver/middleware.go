package httpserver

import (
	"context"
	"fmt"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel/trace"
)

// timeoutMiddleware wraps c.UserContext() with context.WithTimeout, so any
// handler / service / repo call that respects ctx will be cancelled after
// `timeout`. The middleware does not write a response itself — handlers
// that respect ctx return their normal error path (typically apperror
// from the underlying cancelled DB call) and the error handler maps it.
//
// Pair with db.statement_timeout: even if a code path ignores ctx, the
// query will still be killed server-side.
func timeoutMiddleware(timeout time.Duration) fiber.Handler {
	return func(c *fiber.Ctx) error {
		ctx, cancel := context.WithTimeout(c.UserContext(), timeout)
		defer cancel()
		c.SetUserContext(ctx)
		return c.Next()
	}
}

// requestLogger logs one structured line per HTTP request. If a trace
// context is present on the request ctx (set by tracing.Middleware), the
// trace_id is added to the log line so a tail can be filtered down to a
// single request across all middleware/handler logs.
func requestLogger(log zerolog.Logger) fiber.Handler {
	return func(c *fiber.Ctx) error {
		start := time.Now()
		err := c.Next()

		evt := log.Info().
			Str("method", c.Method()).
			Str("path", c.Path()).
			Int("status", c.Response().StatusCode()).
			Str("request_id", fmt.Sprint(c.Locals("requestid"))).
			Dur("latency", time.Since(start))
		if sc := trace.SpanContextFromContext(c.UserContext()); sc.IsValid() {
			evt = evt.Str("trace_id", sc.TraceID().String())
		}
		evt.Msg("http request")
		return err
	}
}
