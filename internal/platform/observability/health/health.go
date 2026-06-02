// Package health defines liveness and readiness probes for the HTTP server.
//
//   - /healthz: always 200 — process is alive. Used by k8s livenessProbe
//     to detect a hung process (and trigger a pod kill).
//   - /readyz: runs every registered Probe with a short timeout. 200 means
//     the app can serve traffic; 503 means a dependency is unavailable
//     (DB ping fails, broker connection dropped, …). Used by k8s
//     readinessProbe to gate traffic.
//
// Probes should be cheap (single ping or in-memory check) — they may be
// called every second.
package health

import (
	"context"
	"net/http"
	"time"

	"github.com/gofiber/fiber/v2"
)

// Probe returns nil if the dependency is healthy. Each probe is given a
// shared deadline by Readiness; implementations may still set their own
// internal timeout if a probe primitive lacks ctx support.
type Probe func(ctx context.Context) error

// readinessTimeout caps how long a single /readyz call can spend running
// probes in total. Each probe must respect ctx.
const readinessTimeout = 3 * time.Second

// Liveness handler always returns 200. Mount on /healthz.
func Liveness(c *fiber.Ctx) error {
	return c.SendStatus(http.StatusOK)
}

// Readiness returns a handler that runs every probe sequentially under a
// shared timeout. On any probe failure it returns 503 with the first
// failing probe's error; this is enough for k8s to gate traffic and for an
// operator to find the broken dependency in logs.
func Readiness(probes ...Probe) fiber.Handler {
	return func(c *fiber.Ctx) error {
		ctx, cancel := context.WithTimeout(c.UserContext(), readinessTimeout)
		defer cancel()
		for _, p := range probes {
			if err := p(ctx); err != nil {
				return c.Status(http.StatusServiceUnavailable).JSON(fiber.Map{"error": err.Error()})
			}
		}
		return c.SendStatus(http.StatusOK)
	}
}
