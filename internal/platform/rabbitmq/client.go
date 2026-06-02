// Package rabbitmq is the AMQP client used by the outbox relay (as its
// Dispatcher) and by platform/consumers (to receive events). Topology is
// fixed: one topic exchange for events, one DLX for messages that consumers
// nack(requeue=false). Per-subscription queues are declared lazily by
// platform/consumers.
//
// Reconnect is lazy: Channel() checks the cached connection and re-dials
// (+ redeclares exchanges) if it has been closed. Callers don't manage
// reconnect state — they retry their use of Channel() on error, with
// their own backoff (consumers.Subscriber.consume for consumers, the
// outbox relay's per-row retry for publishers).
package rabbitmq

import (
	"context"
	"errors"
	"fmt"
	"sync"

	amqp "github.com/rabbitmq/amqp091-go"
	"github.com/rs/zerolog"
)

// Config tunes the RabbitMQ connection and topology names. DSN follows the
// amqp:// scheme. Exchange/DLX names are configurable so a single broker can
// host multiple apps without collisions.
type Config struct {
	DSN      string
	Exchange string // main topic exchange producers publish to
	DLX      string // dead-letter exchange consumer queues route to on nack
}

// Client owns the AMQP connection. Channels are short-lived and obtained
// via Channel(); amqp091's connection is safe for concurrent channel
// creation, but channels themselves are not safe for concurrent use.
//
// Connection state is guarded by mu. On the first call after the
// connection drops, Channel re-dials under the lock — concurrent callers
// see the new conn after the lock is released.
type Client struct {
	cfg Config
	log zerolog.Logger

	mu     sync.Mutex
	conn   *amqp.Connection
	closed bool
}

// Connect opens the AMQP connection, declares the topic exchange and DLX,
// and returns a Client ready for Publisher/Subscriber.
func Connect(cfg Config, log zerolog.Logger) (*Client, error) {
	c := &Client{
		cfg: cfg,
		log: log.With().Str("component", "rabbitmq").Logger(),
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if err := c.dialLocked(); err != nil {
		return nil, err
	}
	return c, nil
}

// dialLocked opens a new connection and declares exchanges. Caller holds mu.
func (c *Client) dialLocked() error {
	conn, err := amqp.Dial(c.cfg.DSN)
	if err != nil {
		return fmt.Errorf("dial rabbitmq: %w", err)
	}
	if err := declareExchanges(conn, c.cfg); err != nil {
		_ = conn.Close()
		return err
	}
	c.conn = conn
	c.log.Info().Msg("rabbitmq connected")
	return nil
}

func declareExchanges(conn *amqp.Connection, cfg Config) error {
	ch, err := conn.Channel()
	if err != nil {
		return fmt.Errorf("open channel: %w", err)
	}
	defer ch.Close()

	if err := ch.ExchangeDeclare(cfg.Exchange, "topic", true, false, false, false, nil); err != nil {
		return fmt.Errorf("declare exchange %s: %w", cfg.Exchange, err)
	}
	if err := ch.ExchangeDeclare(cfg.DLX, "topic", true, false, false, false, nil); err != nil {
		return fmt.Errorf("declare exchange %s: %w", cfg.DLX, err)
	}
	return nil
}

// Channel returns a new AMQP channel, transparently re-dialing the
// connection if the previous one has been closed by the broker. Callers
// retain ownership and must Close the channel when done.
//
// Errors from Channel propagate up to the caller's own retry loop:
// consumer goroutines back off and call again, the outbox relay schedules
// the row for retry per its outbox.Config.
func (c *Client) Channel() (*amqp.Channel, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return nil, errors.New("rabbitmq client closed")
	}
	if c.conn == nil || c.conn.IsClosed() {
		c.conn = nil
		if err := c.dialLocked(); err != nil {
			return nil, err
		}
	}
	return c.conn.Channel()
}

// Config returns the topology configuration (read-only).
func (c *Client) Config() Config { return c.cfg }

// HealthCheck reports whether the AMQP connection is currently open. Used
// by /readyz so traffic stops being routed to a pod whose broker link
// has dropped. The next Channel() call will lazily redial.
func (c *Client) HealthCheck(_ context.Context) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.closed {
		return errors.New("rabbitmq client closed")
	}
	if c.conn == nil || c.conn.IsClosed() {
		return errors.New("rabbitmq connection closed")
	}
	return nil
}

// Close shuts the client down for good — subsequent Channel calls return
// an error and the reconnect loop in callers will exit on ctx cancellation.
func (c *Client) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closed = true
	if c.conn != nil {
		err := c.conn.Close()
		c.conn = nil
		return err
	}
	return nil
}
