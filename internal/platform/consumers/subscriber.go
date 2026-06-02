// Package consumers binds module event handlers to RabbitMQ queues. One
// Subscriber instance corresponds to one consumer name — typically a module.
//
// Topology per Subscribe(topic, handler):
//
//	exchange:   <events>            (topic, declared by rabbitmq.Client)
//	main queue: <consumer>.<topic>  (durable, routed via x-dead-letter-exchange to DLX)
//	main bind:  exchange  --topic-->  main queue
//	dlq queue:  <consumer>.<topic>.dlq
//	dlq bind:   DLX  --topic-->  dlq queue
//
// On handler success → ack. On handler error → nack(requeue=false), which
// routes the message to the DLX and ultimately into the .dlq queue for
// operator inspection (analogous to the old outbox_dead table).
//
// Bulkhead: each subscription processes deliveries in parallel up to its
// concurrency limit (Config.DefaultConcurrency or Config.TopicConcurrency
// override). AMQP prefetch matches concurrency so the broker stops pushing
// once the bulkhead is full; one slow topic cannot starve the others.
package consumers

import (
	"context"
	"fmt"
	"math/rand/v2"
	"sync"
	"time"

	"github.com/google/uuid"
	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/rs/zerolog"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"

	"github.com/monorepo/internal/platform/messaging"
	"github.com/monorepo/internal/platform/observability/metrics"
	"github.com/monorepo/internal/platform/rabbitmq"
)

// Reconnect backoff: when a consumer's channel dies (broker restart,
// network blip, dead conn), we wait before retrying. Exponential growth
// up to a cap, with ±25% jitter so N consumers across pods don't all hit
// the broker at the same instant.
const (
	reconnectBase   = 1 * time.Second
	reconnectMax    = 30 * time.Second
	reconnectJitter = 0.25
)

// Defaults applied when Config leaves fields zero. DefaultConcurrency=4 is
// a conservative starting point — bump per-topic via TopicConcurrency for
// hot streams. QueueDepthPollInterval=30s is cheap (one QueueInspect per
// queue + dlq per interval).
const (
	defaultConcurrency       = 4
	defaultQueueDepthPollInt = 30 * time.Second
	defaultHandlerTimeout    = 30 * time.Second
)

// Config tunes per-Subscriber behaviour. All fields are optional; leaving
// the struct zero yields safe defaults.
type Config struct {
	// DefaultConcurrency caps in-flight handler executions per subscription.
	// Acts as both the bulkhead semaphore size and the AMQP prefetch count.
	DefaultConcurrency int
	// TopicConcurrency overrides DefaultConcurrency for specific topics —
	// keys are routing keys passed to Subscribe.
	TopicConcurrency map[string]int
	// QueueDepthPollInterval controls how often the depth poller calls
	// QueueInspect to refresh the consumer_queue_depth gauge.
	QueueDepthPollInterval time.Duration
	// HandlerTimeout bounds a single handler invocation. The handler ctx
	// is cancelled at this deadline so handlers respecting ctx unwind;
	// they still acquire a bulkhead slot for the full duration, so a
	// runaway handler can occupy a slot up to this long.
	HandlerTimeout time.Duration
}

// Subscriber registers handlers at wiring-time and runs one consumer
// goroutine per subscription on Run.
type Subscriber struct {
	client       *rabbitmq.Client
	consumerName string
	cfg          Config
	log          zerolog.Logger

	mu   sync.Mutex
	subs []subscription
}

type subscription struct {
	topic       string
	handler     messaging.Handler
	queue       string
	concurrency int
}

func New(client *rabbitmq.Client, consumerName string, cfg Config, log zerolog.Logger) *Subscriber {
	if cfg.DefaultConcurrency <= 0 {
		cfg.DefaultConcurrency = defaultConcurrency
	}
	if cfg.QueueDepthPollInterval <= 0 {
		cfg.QueueDepthPollInterval = defaultQueueDepthPollInt
	}
	if cfg.HandlerTimeout <= 0 {
		cfg.HandlerTimeout = defaultHandlerTimeout
	}
	return &Subscriber{
		client:       client,
		consumerName: consumerName,
		cfg:          cfg,
		log:          log.With().Str("component", "consumer").Str("name", consumerName).Logger(),
	}
}

// Subscribe registers a handler for a topic. Wiring-time only — not safe to
// call after Run. Concurrency is resolved from Config: TopicConcurrency[topic]
// if set, else DefaultConcurrency.
func (s *Subscriber) Subscribe(topic string, h messaging.Handler) {
	s.mu.Lock()
	defer s.mu.Unlock()
	c := s.cfg.DefaultConcurrency
	if override, ok := s.cfg.TopicConcurrency[topic]; ok && override > 0 {
		c = override
	}
	s.subs = append(s.subs, subscription{
		topic:       topic,
		handler:     h,
		queue:       s.consumerName + "." + topic,
		concurrency: c,
	})
}

// Run starts a consumer goroutine per registered subscription plus a single
// queue-depth poller. It returns when ctx is cancelled or when any consumer
// fails fatally. Each consumer owns its AMQP channel for the lifetime of
// the run.
func (s *Subscriber) Run(ctx context.Context) error {
	if len(s.subs) == 0 {
		<-ctx.Done()
		return ctx.Err()
	}

	var wg sync.WaitGroup
	for _, sub := range s.subs {
		wg.Add(1)
		go func(sub subscription) {
			defer wg.Done()
			if err := s.consume(ctx, sub); err != nil && ctx.Err() == nil {
				s.log.Error().Err(err).Str("queue", sub.queue).Msg("consumer stopped")
			}
		}(sub)
	}

	wg.Add(1)
	go func() {
		defer wg.Done()
		s.pollQueueDepths(ctx)
	}()

	wg.Wait()
	return nil
}

// consume drives a single subscription forever. consumeOnce holds a live
// AMQP channel and drains its deliveries; when the channel/connection
// dies (broker restart, network blip), consumeOnce returns and consume
// backs off before reconnecting via the next consumeOnce — rabbitmq.Client
// transparently re-dials the connection inside Channel().
//
// Each subscription has its own attempt counter, so a queue-specific
// problem doesn't slow recovery for healthy queues.
func (s *Subscriber) consume(ctx context.Context, sub subscription) error {
	attempt := 0
	for ctx.Err() == nil {
		err := s.consumeOnce(ctx, sub)
		if err == nil || ctx.Err() != nil {
			return ctx.Err()
		}
		attempt++
		wait := reconnectBackoff(attempt)
		s.log.Warn().Err(err).
			Str("queue", sub.queue).Int("attempt", attempt).
			Dur("retry_in", wait).Msg("consumer disconnected, reconnecting")
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(wait):
		}
	}
	return ctx.Err()
}

// consumeOnce holds a single AMQP channel for the lifetime of one
// healthy consume session. It returns nil only on ctx cancellation; any
// other return means the channel or connection failed and the outer loop
// should reconnect.
//
// Queue declaration is idempotent in RabbitMQ — repeated declares of the
// same durable queue with identical args succeed, so re-running it on
// every reconnect costs nothing and recovers from broker-side state loss.
//
// Bulkhead: prefetch is set to the subscription's concurrency and a sized
// semaphore caps in-flight handler goroutines at the same number. On a
// healthy reconnect the in-flight workers from the previous session are
// allowed to finish (their goroutines own their amqp.Delivery and ack via
// the dead channel will simply fail — message is redelivered, which is
// fine since handlers must be idempotent).
//
// Graceful shutdown: the accept loop watches `ctx` (cancelled on SIGTERM)
// and stops pulling new deliveries the moment it fires. In-flight handler
// goroutines, however, run with `handlerCtx` derived via
// context.WithoutCancel — they keep their own per-message HandlerTimeout
// and get to finish naturally rather than being torn down mid-tx. The
// outer `defer workers.Wait()` blocks the function return until they're
// done, so the caller (Run) sees the goroutine exit only once every
// in-flight handler has either ack'd, nack'd, or hit its handler timeout.
func (s *Subscriber) consumeOnce(ctx context.Context, sub subscription) error {
	ch, err := s.client.Channel()
	if err != nil {
		return fmt.Errorf("open channel: %w", err)
	}
	defer ch.Close()

	if err := s.declareQueueAndBind(ch, sub); err != nil {
		return err
	}

	if err := ch.Qos(sub.concurrency, 0, false); err != nil {
		return fmt.Errorf("qos: %w", err)
	}

	deliveries, err := ch.Consume(sub.queue, "", false, false, false, false, nil)
	if err != nil {
		return fmt.Errorf("consume %s: %w", sub.queue, err)
	}
	s.log.Info().Str("queue", sub.queue).Str("topic", sub.topic).
		Int("concurrency", sub.concurrency).Msg("consumer started")

	// Handlers run under a ctx that does NOT inherit cancellation from `ctx`
	// — so SIGTERM stops new pulls but in-flight handlers keep their
	// per-message HandlerTimeout and complete naturally. Trace/span context
	// is preserved (only cancellation is severed).
	handlerCtx := context.WithoutCancel(ctx)

	sem := make(chan struct{}, sub.concurrency)
	var workers sync.WaitGroup
	defer workers.Wait()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case d, ok := <-deliveries:
			if !ok {
				return fmt.Errorf("delivery channel for %s closed", sub.queue)
			}
			select {
			case sem <- struct{}{}:
			case <-ctx.Done():
				return ctx.Err()
			}
			workers.Add(1)
			go func(d amqp.Delivery) {
				defer workers.Done()
				defer func() { <-sem }()
				s.handle(handlerCtx, sub, d)
			}(d)
		}
	}
}

// reconnectBackoff returns reconnectBase * 2^(attempt-1), capped at
// reconnectMax, with ±reconnectJitter randomness. Same shape as the
// outbox relay's backoff so behaviour is uniform across the platform.
func reconnectBackoff(attempt int) time.Duration {
	d := reconnectBase << (attempt - 1)
	if d <= 0 || d > reconnectMax {
		d = reconnectMax
	}
	jitter := 1 + reconnectJitter*(2*rand.Float64()-1)
	return time.Duration(float64(d) * jitter)
}

func (s *Subscriber) declareQueueAndBind(ch *amqp.Channel, sub subscription) error {
	cfg := s.client.Config()
	dlqQueue := sub.queue + ".dlq"

	// DLQ first — plain durable queue bound to the DLX with the same
	// routing key as the main subscription, so rejected messages land here.
	if _, err := ch.QueueDeclare(dlqQueue, true, false, false, false, nil); err != nil {
		return fmt.Errorf("declare dlq %s: %w", dlqQueue, err)
	}
	if err := ch.QueueBind(dlqQueue, sub.topic, cfg.DLX, false, nil); err != nil {
		return fmt.Errorf("bind dlq %s to %s: %w", dlqQueue, cfg.DLX, err)
	}

	// Main queue: durable, dead-letters to DLX on nack(requeue=false).
	args := amqp.Table{
		"x-dead-letter-exchange": cfg.DLX,
	}
	if _, err := ch.QueueDeclare(sub.queue, true, false, false, false, args); err != nil {
		return fmt.Errorf("declare queue %s: %w", sub.queue, err)
	}
	if err := ch.QueueBind(sub.queue, sub.topic, cfg.Exchange, false, nil); err != nil {
		return fmt.Errorf("bind %s to %s: %w", sub.queue, cfg.Exchange, err)
	}
	return nil
}

func (s *Subscriber) handle(ctx context.Context, sub subscription, d amqp.Delivery) {
	// Restore the producer-side trace context from AMQP headers so the
	// handler's span is a child of the original producing request.
	if len(d.Headers) > 0 {
		carrier := propagation.MapCarrier{}
		for k, v := range d.Headers {
			if str, ok := v.(string); ok {
				carrier[k] = str
			}
		}
		ctx = otel.GetTextMapPropagator().Extract(ctx, carrier)
	}

	// MessageId carries the producer's outbox row id; pass it through on
	// the Event so handlers can dedup against <schema>.processed_events.
	// uuid.Nil when a message arrives without one — handlers using
	// consumers.Dedup will refuse it explicitly.
	var eventID uuid.UUID
	if d.MessageId != "" {
		if parsed, err := uuid.Parse(d.MessageId); err == nil {
			eventID = parsed
		} else {
			s.log.Warn().Str("message_id", d.MessageId).Err(err).Msg("invalid message id; event.ID will be nil")
		}
	}
	ev := messaging.Event{
		ID:      eventID,
		Topic:   d.RoutingKey,
		Payload: d.Body,
	}

	ctx, cancel := context.WithTimeout(ctx, s.cfg.HandlerTimeout)
	defer cancel()

	start := time.Now()
	status := "ack"
	defer func() {
		metrics.ConsumerHandleDuration.WithLabelValues(s.consumerName, sub.topic).
			Observe(time.Since(start).Seconds())
		metrics.ConsumerMessagesTotal.WithLabelValues(s.consumerName, sub.topic, status).Inc()
	}()

	defer func() {
		if r := recover(); r != nil {
			status = "panic"
			s.log.Error().Interface("panic", r).
				Str("queue", sub.queue).Str("topic", sub.topic).
				Msg("handler panicked; routing to DLQ")
			if nackErr := d.Nack(false, false); nackErr != nil {
				s.log.Error().Err(nackErr).Msg("nack failed")
			}
		}
	}()

	if err := sub.handler(ctx, ev); err != nil {
		status = "nack"
		s.log.Error().Err(err).
			Str("queue", sub.queue).Str("topic", sub.topic).
			Msg("handler failed; routing to DLQ")
		if nackErr := d.Nack(false, false); nackErr != nil {
			s.log.Error().Err(nackErr).Msg("nack failed")
		}
		return
	}
	if ackErr := d.Ack(false); ackErr != nil {
		s.log.Error().Err(ackErr).Msg("ack failed")
	}
}

// pollQueueDepths periodically refreshes the consumer_queue_depth gauge for
// every subscription's main queue and DLQ. Uses a dedicated channel and
// re-acquires it on broker hiccups; depth gauge is best-effort observability,
// not a control plane signal, so transient failures just skip a tick.
func (s *Subscriber) pollQueueDepths(ctx context.Context) {
	t := time.NewTicker(s.cfg.QueueDepthPollInterval)
	defer t.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			s.refreshQueueDepths(ctx)
		}
	}
}

func (s *Subscriber) refreshQueueDepths(ctx context.Context) {
	for _, sub := range s.subs {
		if ctx.Err() != nil {
			return
		}
		s.inspectAndRecord(sub.queue, sub.topic, "main")
		s.inspectAndRecord(sub.queue+".dlq", sub.topic, "dlq")
	}
}

// inspectAndRecord uses a one-shot channel per query because a failed
// QueueDeclarePassive (queue missing) closes the AMQP channel — sharing
// one across all queries would lose the rest of the tick on the first
// error. The cost is one extra round-trip per (queue, tick).
func (s *Subscriber) inspectAndRecord(queue, topic, kind string) {
	ch, err := s.client.Channel()
	if err != nil {
		s.log.Debug().Err(err).Msg("queue depth: channel unavailable")
		return
	}
	defer ch.Close()

	// Passive declare returns the queue's current stats without modifying
	// server state. Errors if the queue doesn't exist (consumer hasn't run
	// yet on this broker, queue was deleted) — debug-log + skip.
	q, err := ch.QueueDeclarePassive(queue, true, false, false, false, nil)
	if err != nil {
		s.log.Debug().Err(err).Str("queue", queue).Msg("queue inspect failed")
		return
	}
	metrics.ConsumerQueueDepth.WithLabelValues(s.consumerName, topic, kind).Set(float64(q.Messages))
}
