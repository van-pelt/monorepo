// Package metrics defines Prometheus metrics and the HTTP middleware /
// handler that expose them. Metrics are registered against the default
// Prometheus registry at package init via promauto, so importing this
// package is enough to make them available — no Register() call needed.
//
// To add a metric: declare it as a package-level var below and reference
// it from the code that updates it. Keep label sets small and bounded —
// metrics are stored in memory per unique label combination.
package metrics

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

var (
	// HTTPRequestDuration tracks per-request latency. The "route" label uses
	// Fiber's route pattern (e.g. "/api/v1/accounts/:id"), not the literal
	// path, so cardinality is bounded by the number of declared routes.
	HTTPRequestDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "http_request_duration_seconds",
		Help:    "Duration of HTTP requests in seconds.",
		Buckets: prometheus.DefBuckets,
	}, []string{"method", "route", "status"})

	// HTTPRequestsTotal counts requests by method/route/status.
	HTTPRequestsTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "http_requests_total",
		Help: "Total HTTP requests.",
	}, []string{"method", "route", "status"})

	// OutboxBacklog is the number of rows currently sitting in each
	// module's outbox table. Refreshed periodically by the relay; a
	// sustained non-zero value points to a stuck dispatcher.
	OutboxBacklog = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "outbox_backlog",
		Help: "Number of pending rows in each module's outbox table.",
	}, []string{"schema"})

	// OutboxDeadTotal counts events that exhausted retries and were moved
	// to <schema>.outbox_dead. Alert on increase.
	OutboxDeadTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "outbox_dead_total",
		Help: "Total events moved to dead-letter.",
	}, []string{"schema", "topic"})

	// OutboxDispatchDuration tracks the time the relay spent dispatching
	// a single event (Dispatcher.Dispatch call).
	OutboxDispatchDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "outbox_dispatch_duration_seconds",
		Help:    "Duration of a single outbox event dispatch.",
		Buckets: prometheus.DefBuckets,
	}, []string{"schema", "topic"})

	// PanicsTotal counts panics recovered by the Fiber recover middleware.
	PanicsTotal = promauto.NewCounter(prometheus.CounterOpts{
		Name: "panics_total",
		Help: "Total recovered panics.",
	})

	// ConsumerQueueDepth is the AMQP queue depth refreshed periodically by
	// each consumers.Subscriber via QueueInspect. The "kind" label is
	// "main" or "dlq" so backlog and dead-letter rates are visible side by
	// side. Alert on dlq>0 and on sustained main growth.
	ConsumerQueueDepth = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "consumer_queue_depth",
		Help: "Current AMQP queue depth per consumer/topic.",
	}, []string{"consumer", "topic", "kind"})

	// ConsumerMessagesTotal counts processed deliveries by outcome. status
	// is one of "ack", "nack" (handler returned error → DLQ), "panic" (handler
	// panicked → DLQ).
	ConsumerMessagesTotal = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "consumer_messages_total",
		Help: "Total messages processed by consumers.",
	}, []string{"consumer", "topic", "status"})

	// ConsumerHandleDuration tracks per-message handler latency, including
	// the ack/nack call but excluding the time messages sit in the bulkhead
	// semaphore before a worker picks them up.
	ConsumerHandleDuration = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "consumer_handle_duration_seconds",
		Help:    "Duration of consumer handler invocations.",
		Buckets: prometheus.DefBuckets,
	}, []string{"consumer", "topic"})
)

// Handler returns the net/http handler that serves Prometheus scrape
// requests. Mount it on a non-versioned route (e.g. "/metrics") via
// fiber's adaptor.
func Handler() http.Handler { return promhttp.Handler() }
