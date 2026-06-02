package metrics

import (
	"strconv"
	"strings"
	"time"

	"github.com/gofiber/fiber/v2"
)

// Middleware records HTTPRequestDuration and HTTPRequestsTotal for every
// request. Mount as early as possible (after recover) so it sees latency
// of subsequent middleware plus the handler.
//
// Note: c.Method() and c.Route().Path return strings backed by Fiber's
// pooled request buffer (zero-copy via unsafe). Once the Ctx is recycled
// for the next request, the underlying bytes are overwritten, which
// silently corrupts any string we handed off to Prometheus — labels like
// "POST" mutate into "GETT" mid-flight. strings.Clone forces a fresh
// heap-backed copy so the label is stable for the lifetime of the
// metric series.
func Middleware() fiber.Handler {
	return func(c *fiber.Ctx) error {
		start := time.Now()
		err := c.Next()

		route := c.Route().Path
		if route == "" {
			// Unmatched routes (404) have no Route().Path; bucket them under
			// "unknown" so they don't blow up cardinality with raw paths.
			route = "unknown"
		}
		method := strings.Clone(c.Method())
		route = strings.Clone(route)
		status := strconv.Itoa(c.Response().StatusCode())

		HTTPRequestDuration.WithLabelValues(method, route, status).Observe(time.Since(start).Seconds())
		HTTPRequestsTotal.WithLabelValues(method, route, status).Inc()
		return err
	}
}
