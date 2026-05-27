package events

import (
	"context"
	"fmt"

	"github.com/nats-io/nats.go"
	"go.uber.org/zap"
)

// NATSAdapter implements EventPublisher using plain NATS core.
// Use this when JetStream durability is not required.
type NATSAdapter struct {
	conn   *nats.Conn
	logger *zap.Logger
}

// NewNATSAdapter creates an EventPublisher backed by plain NATS core.
func NewNATSAdapter(conn *nats.Conn, logger *zap.Logger) *NATSAdapter {
	return &NATSAdapter{conn: conn, logger: logger.Named("outbox.nats")}
}

// Publish publishes an event to NATS on the event's derived subject.
func (a *NATSAdapter) Publish(_ context.Context, event *Event) error {
	if a.conn == nil {
		a.logger.Warn("NATS connection not available, skipping publish",
			zap.String("event_type", event.EventType))
		return nil
	}
	data, err := event.ToJSON()
	if err != nil {
		return fmt.Errorf("marshal event: %w", err)
	}
	if err := a.conn.Publish(event.Subject(), data); err != nil {
		return fmt.Errorf("nats publish: %w", err)
	}
	a.logger.Debug("event published via NATS",
		zap.String("subject", event.Subject()),
		zap.String("event_id", event.ID.String()))
	return nil
}

// JetStreamAdapter implements EventPublisher using NATS JetStream.
// Use this for durable, persisted event delivery with ack and replay support.
type JetStreamAdapter struct {
	js     nats.JetStreamContext
	logger *zap.Logger
}

// NewJetStreamAdapter creates an EventPublisher backed by NATS JetStream.
func NewJetStreamAdapter(js nats.JetStreamContext, logger *zap.Logger) *JetStreamAdapter {
	return &JetStreamAdapter{js: js, logger: logger.Named("outbox.jetstream")}
}

// Publish publishes an event to JetStream with standard metadata headers.
func (a *JetStreamAdapter) Publish(_ context.Context, event *Event) error {
	if a.js == nil {
		a.logger.Warn("JetStream not available, skipping publish",
			zap.String("event_type", event.EventType))
		return nil
	}
	data, err := event.ToJSON()
	if err != nil {
		return fmt.Errorf("marshal event: %w", err)
	}

	headers := make(nats.Header)
	headers.Set("event-id", event.ID.String())
	headers.Set("event-type", event.EventType)
	headers.Set("aggregate-type", event.AggregateType)
	headers.Set("aggregate-id", event.AggregateID.String())
	headers.Set("tenant-id", event.TenantID.String())
	headers.Set("event-version", event.Version)

	msg := nats.NewMsg(event.Subject())
	msg.Data = data
	msg.Header = headers

	if _, err := a.js.PublishMsg(msg); err != nil {
		return fmt.Errorf("jetstream publish: %w", err)
	}
	a.logger.Debug("event published via JetStream",
		zap.String("subject", event.Subject()),
		zap.String("event_id", event.ID.String()))
	return nil
}
