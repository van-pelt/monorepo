package httpserver

import (
	"fmt"
	"time"

	"github.com/gofiber/fiber/v2"
	"github.com/rs/zerolog"
)

// requestLogger logs one structured line per HTTP request.
func requestLogger(log zerolog.Logger) fiber.Handler {
	return func(c *fiber.Ctx) error {
		start := time.Now()
		err := c.Next()
		log.Info().
			Str("method", c.Method()).
			Str("path", c.Path()).
			Int("status", c.Response().StatusCode()).
			Str("request_id", fmt.Sprint(c.Locals("requestid"))).
			Dur("latency", time.Since(start)).
			Msg("http request")
		return err
	}
}
