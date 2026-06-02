package rabbitmq

import (
	"context"
	"fmt"

	"github.com/google/uuid"
	amqp "github.com/rabbitmq/amqp091-go"
	"go.opentelemetry.io/otel"
	"go.opentelemetry.io/otel/propagation"
)

// Publisher implements outbox.Dispatcher: each Dispatch forwards one outbox
// row to the topic exchange with routing key = topic. It opens a channel per
// call because the outbox relay is single-threaded — there is no contention
// to amortise across, and channel-per-publish keeps a transient failure
// scoped to a single message (the next call gets a fresh channel).
type Publisher struct {
	client *Client
}

func NewPublisher(client *Client) *Publisher {
	return &Publisher{client: client}
}

// Dispatch publishes payload to the exchange with topic as routing key.
// Returns nil only when amqp091 has accepted the publish; the outbox relay
// then deletes the row.
//
// eventID is the producer's outbox row id; sent as amqp.Publishing.MessageId
// so consumers can use it for dedup against <schema>.processed_events.
func (p *Publisher) Dispatch(ctx context.Context, eventID uuid.UUID, topic string, payload []byte) error {
	ch, err := p.client.Channel()
	if err != nil {
		return fmt.Errorf("open channel: %w", err)
	}
	defer ch.Close()

	// Propagate the trace context (restored by the outbox relay) into AMQP
	// headers so the consumer's span is a child of the original producer's.
	carrier := propagation.MapCarrier{}
	otel.GetTextMapPropagator().Inject(ctx, carrier)
	var headers amqp.Table
	if len(carrier) > 0 {
		headers = amqp.Table{}
		for k, v := range carrier {
			headers[k] = v
		}
	}

	return ch.PublishWithContext(ctx,
		p.client.cfg.Exchange,
		topic,
		false, // mandatory
		false, // immediate
		amqp.Publishing{
			MessageId:    eventID.String(),
			ContentType:  "application/json",
			Body:         payload,
			DeliveryMode: amqp.Persistent,
			Headers:      headers,
		},
	)
}
