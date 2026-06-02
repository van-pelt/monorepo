// Package tracing wires the OpenTelemetry SDK and exposes helpers for
// propagating trace context across async boundaries (HTTP, AMQP, outbox).
//
// Phase 4.2 wires the SDK with no exporter configured — spans are sampled
// and dropped after recording. Adding an OTLP exporter later is a matter of
// passing `sdktrace.WithBatcher(otlpExporter)` to Init; instrumentation
// already lives at all the right places (HTTP middleware, outbox column,
// AMQP headers).
package tracing

import (
	"context"
	"encoding/json"

	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
	sdktrace "go.opentelemetry.io/otel/sdk/trace"
	"go.opentelemetry.io/otel/trace"
)

// Init installs the global TracerProvider and TextMapPropagator. The
// returned shutdown function flushes pending spans on app exit. With no
// SpanProcessor wired, spans are created and discarded — the SDK is ready
// to receive an OTLP exporter without further code changes.
func Init() func(context.Context) error {
	tp := sdktrace.NewTracerProvider()
	otel.SetTracerProvider(tp)

	// W3C TraceContext: defines what we read from incoming HTTP headers,
	// inject into AMQP headers and serialize into the outbox row.
	otel.SetTextMapPropagator(propagation.NewCompositeTextMapPropagator(
		propagation.TraceContext{},
		propagation.Baggage{},
	))
	return tp.Shutdown
}

// Tracer returns a tracer scoped to a package-stable name. Use the
// importing package's import path so spans can be filtered by source.
func Tracer(name string) trace.Tracer { return otel.Tracer(name) }

// MarshalContext serializes the current trace context (traceparent +
// tracestate) into a JSON blob suitable for storing in the outbox
// trace_context column. Returns nil when ctx carries no span.
func MarshalContext(ctx context.Context) ([]byte, error) {
	carrier := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, carrier)
	if len(carrier) == 0 {
		return nil, nil
	}
	return json.Marshal(carrier)
}

// UnmarshalContext restores the trace context from the outbox column into
// the parent ctx. Empty/nil input is a no-op — returns the original ctx.
func UnmarshalContext(ctx context.Context, raw []byte) context.Context {
	if len(raw) == 0 {
		return ctx
	}
	var carrier propagation.MapCarrier
	if err := json.Unmarshal(raw, &carrier); err != nil {
		return ctx
	}
	return otel.GetTextMapPropagator().Extract(ctx, carrier)
}
