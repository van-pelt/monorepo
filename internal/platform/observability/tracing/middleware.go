package tracing

import (
	"strings"

	"github.com/gofiber/fiber/v2"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/codes"
	"go.opentelemetry.io/otel/propagation"
)

const tracerName = "github.com/monorepo/cmd/api"

// Middleware extracts the incoming W3C trace context from request headers,
// starts a span named "METHOD route" and propagates the updated context to
// subsequent middleware and the handler via c.SetUserContext. Records
// status code and any returned error on the span.
//
// Note: c.Method() and c.Route().Path are zero-copy strings backed by
// Fiber's pooled request buffer. Spans live past the request lifecycle
// (the exporter batches them), so any raw Fiber string we put into an
// attribute would silently mutate when the Ctx is recycled. strings.Clone
// forces a heap-backed copy.
func Middleware() fiber.Handler {
	tracer := otel.Tracer(tracerName)
	return func(c *fiber.Ctx) error {
		carrier := propagation.MapCarrier{}
		c.Request().Header.VisitAll(func(k, v []byte) {
			// string([]byte) below already copies — safe to put in carrier
			// which is consumed synchronously by Extract right after.
			carrier.Set(string(k), string(v))
		})
		parent := otel.GetTextMapPropagator().Extract(c.UserContext(), carrier)

		route := c.Route().Path
		if route == "" {
			route = "unknown"
		}
		method := strings.Clone(c.Method())
		route = strings.Clone(route)

		ctx, span := tracer.Start(parent, method+" "+route)
		defer span.End()

		span.SetAttributes(
			attribute.String("http.method", method),
			attribute.String("http.route", route),
		)
		c.SetUserContext(ctx)

		err := c.Next()

		span.SetAttributes(attribute.Int("http.status_code", c.Response().StatusCode()))
		if err != nil {
			span.RecordError(err)
			span.SetStatus(codes.Error, err.Error())
		}
		return err
	}
}
