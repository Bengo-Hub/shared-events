package events

import (
	"context"
	"sync"
	"time"

	"go.uber.org/zap"
)

// EventPublisher is the transport-layer interface for publishing events to a message broker.
// Implement this with NATSAdapter (plain NATS core) or JetStreamAdapter (NATS JetStream).
type EventPublisher interface {
	Publish(ctx context.Context, event *Event) error
}

// PollerConfig holds configuration for OutboxPoller.
type PollerConfig struct {
	BatchSize  int
	PollPeriod time.Duration
}

// OutboxPoller polls the outbox repository and dispatches pending events via an EventPublisher.
// Wire in a NATSAdapter or JetStreamAdapter as the publisher to keep transport concerns out of
// the polling loop. Services that previously duplicated this struct can replace it with this.
type OutboxPoller struct {
	repo       OutboxRepository
	publisher  EventPublisher
	logger     *zap.Logger
	batchSize  int
	pollPeriod time.Duration
	wg         sync.WaitGroup
	stopCh     chan struct{}
}

// NewOutboxPoller creates an OutboxPoller with the given transport adapter.
func NewOutboxPoller(repo OutboxRepository, pub EventPublisher, logger *zap.Logger, cfg PollerConfig) *OutboxPoller {
	if cfg.BatchSize <= 0 {
		cfg.BatchSize = 100
	}
	if cfg.PollPeriod <= 0 {
		cfg.PollPeriod = 5 * time.Second
	}
	return &OutboxPoller{
		repo:       repo,
		publisher:  pub,
		logger:     logger.Named("outbox.poller"),
		batchSize:  cfg.BatchSize,
		pollPeriod: cfg.PollPeriod,
		stopCh:     make(chan struct{}),
	}
}

// Start begins the background polling loop in a goroutine.
func (p *OutboxPoller) Start(ctx context.Context) {
	p.wg.Add(1)
	go p.run(ctx)
	p.logger.Info("outbox poller started",
		zap.Int("batch_size", p.batchSize),
		zap.Duration("poll_period", p.pollPeriod),
	)
}

// Stop gracefully shuts down the polling loop and waits for it to exit.
func (p *OutboxPoller) Stop() {
	close(p.stopCh)
	p.wg.Wait()
	p.logger.Info("outbox poller stopped")
}

func (p *OutboxPoller) run(ctx context.Context) {
	defer p.wg.Done()

	ticker := time.NewTicker(p.pollPeriod)
	defer ticker.Stop()

	p.poll(ctx)

	for {
		select {
		case <-ctx.Done():
			return
		case <-p.stopCh:
			return
		case <-ticker.C:
			p.poll(ctx)
		}
	}
}

func (p *OutboxPoller) poll(ctx context.Context) {
	records, err := p.repo.GetPendingRecords(ctx, p.batchSize)
	if err != nil {
		p.logger.Error("failed to get pending records", zap.Error(err))
		return
	}

	for _, record := range records {
		event, err := FromJSON(record.Payload)
		if err != nil {
			p.logger.Warn("invalid outbox payload, marking failed",
				zap.String("id", record.ID.String()), zap.Error(err))
			_ = p.repo.MarkAsFailed(ctx, record.ID, err.Error(), time.Now())
			continue
		}

		if err := p.publisher.Publish(ctx, event); err != nil {
			p.logger.Warn("failed to publish record",
				zap.String("id", record.ID.String()),
				zap.String("event_type", record.EventType),
				zap.Error(err),
			)
			_ = p.repo.MarkAsFailed(ctx, record.ID, err.Error(), time.Now())
			continue
		}

		if err := p.repo.MarkAsPublished(ctx, record.ID, time.Now()); err != nil {
			p.logger.Error("failed to mark record as published",
				zap.String("id", record.ID.String()), zap.Error(err))
		}
	}
}
