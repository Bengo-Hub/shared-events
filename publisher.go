package events

import (
	"context"
	"fmt"
	"time"

	"github.com/nats-io/nats.go"
	"go.uber.org/zap"
)

// Publisher publishes events from the outbox to NATS.
type Publisher struct {
	js     nats.JetStreamContext
	repo   OutboxRepository
	logger *zap.Logger

	// Configuration
	maxRetries   int
	retryDelay   time.Duration
	batchSize    int
	pollInterval time.Duration
	maxAttempts  int
}

// PublisherConfig holds configuration for the publisher.
type PublisherConfig struct {
	JetStream    nats.JetStreamContext
	Repository   OutboxRepository
	Logger       *zap.Logger
	MaxRetries   int           // Max retries for failed events
	RetryDelay   time.Duration // Delay between retries
	BatchSize    int           // Number of events to process per batch
	PollInterval time.Duration // How often to poll for pending events
	MaxAttempts  int           // Max attempts before marking as FAILED
}

// DefaultPublisherConfig returns a default configuration.
func DefaultPublisherConfig(js nats.JetStreamContext, repo OutboxRepository, logger *zap.Logger) PublisherConfig {
	return PublisherConfig{
		JetStream:    js,
		Repository:   repo,
		Logger:       logger,
		MaxRetries:   5,
		RetryDelay:   5 * time.Second,
		BatchSize:    100,
		PollInterval: 2 * time.Second,
		MaxAttempts:  10,
	}
}

// NewPublisher creates a new outbox publisher.
func NewPublisher(cfg PublisherConfig) *Publisher {
	return &Publisher{
		js:           cfg.JetStream,
		repo:         cfg.Repository,
		logger:       cfg.Logger.Named("outbox-publisher"),
		maxRetries:   cfg.MaxRetries,
		retryDelay:   cfg.RetryDelay,
		batchSize:    cfg.BatchSize,
		pollInterval: cfg.PollInterval,
		maxAttempts:  cfg.MaxAttempts,
	}
}

// Start begins publishing events from the outbox in the background.
// This should be called as a goroutine.
func (p *Publisher) Start(ctx context.Context) error {
	ticker := time.NewTicker(p.pollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			p.logger.Info("outbox publisher stopped")
			return nil
		case <-ticker.C:
			if err := p.processBatch(ctx); err != nil {
				p.logger.Error("failed to process batch", zap.Error(err))
			}
		}
	}
}

// processBatch processes a batch of pending events.
func (p *Publisher) processBatch(ctx context.Context) error {
	records, err := p.repo.GetPendingRecords(ctx, p.batchSize)
	if err != nil {
		return fmt.Errorf("get pending records: %w", err)
	}

	if len(records) == 0 {
		return nil // No pending events
	}

	p.logger.Info("processing batch", zap.Int("count", len(records)))

	for _, record := range records {
		if err := p.publishRecord(ctx, record); err != nil {
			p.logger.Error("failed to publish event",
				zap.String("event_id", record.ID.String()),
				zap.String("event_type", record.EventType),
				zap.Error(err),
			)
			// Continue processing other events
		}
	}

	return nil
}

// publishRecord publishes a single event record.
func (p *Publisher) publishRecord(ctx context.Context, record *OutboxRecord) error {
	// Deserialize event
	event, err := FromJSON(record.Payload)
	if err != nil {
		// Invalid payload, mark as failed
		errorMsg := fmt.Sprintf("invalid event payload: %v", err)
		return p.repo.MarkAsFailed(ctx, record.ID, errorMsg, time.Now().UTC())
	}

	// Check max attempts
	if record.Attempts >= p.maxAttempts {
		errorMsg := fmt.Sprintf("max attempts (%d) exceeded", p.maxAttempts)
		return p.repo.MarkAsFailed(ctx, record.ID, errorMsg, time.Now().UTC())
	}

	// Publish to NATS
	subject := event.Subject()
	data, err := event.ToJSON()
	if err != nil {
		errorMsg := fmt.Sprintf("marshal event: %v", err)
		return p.repo.MarkAsFailed(ctx, record.ID, errorMsg, time.Now().UTC())
	}

	// Add headers for event metadata
	headers := make(nats.Header)
	headers.Set("event-id", event.ID.String())
	headers.Set("event-type", event.EventType)
	headers.Set("aggregate-type", event.AggregateType)
	headers.Set("aggregate-id", event.AggregateID.String())
	headers.Set("tenant-id", event.TenantID.String())
	headers.Set("event-version", event.Version)

	msg := nats.NewMsg(subject)
	msg.Data = data
	msg.Header = headers

	_, err = p.js.PublishMsg(msg)
	if err != nil {
		// Retry later
		errorMsg := fmt.Sprintf("publish to NATS: %v", err)
		return p.repo.MarkAsFailed(ctx, record.ID, errorMsg, time.Now().UTC())
	}

	// Mark as published
	publishedAt := time.Now().UTC()
	if err := p.repo.MarkAsPublished(ctx, record.ID, publishedAt); err != nil {
		p.logger.Error("failed to mark event as published",
			zap.String("event_id", record.ID.String()),
			zap.Error(err),
		)
		return err
	}

	p.logger.Info("event published",
		zap.String("event_id", record.ID.String()),
		zap.String("event_type", event.EventType),
		zap.String("subject", subject),
	)

	return nil
}
