package events

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// OutboxRecord represents a record in the outbox table.
type OutboxRecord struct {
	ID            uuid.UUID
	TenantID      uuid.UUID
	AggregateType string
	AggregateID   uuid.UUID
	EventType     string
	Payload       []byte
	Status        string // PENDING, PUBLISHED, FAILED
	Attempts      int
	LastAttemptAt *time.Time
	PublishedAt   *time.Time
	ErrorMessage  *string
	CreatedAt     time.Time
}

const (
	StatusPending   = "PENDING"
	StatusPublished = "PUBLISHED"
	StatusFailed    = "FAILED"
)

// OutboxRepository defines the interface for outbox persistence.
type OutboxRepository interface {
	// CreateOutboxRecord stores an event in the outbox within a transaction.
	CreateOutboxRecord(ctx context.Context, tx *sql.Tx, record *OutboxRecord) error
	
	// GetPendingRecords fetches pending events for publishing.
	GetPendingRecords(ctx context.Context, limit int) ([]*OutboxRecord, error)
	
	// MarkAsPublished marks an event as successfully published.
	MarkAsPublished(ctx context.Context, id uuid.UUID, publishedAt time.Time) error
	
	// MarkAsFailed marks an event as failed and increments attempts.
	MarkAsFailed(ctx context.Context, id uuid.UUID, errorMessage string, lastAttemptAt time.Time) error
	
	// BeginTx starts a database transaction.
	BeginTx(ctx context.Context) (*sql.Tx, error)
}

// CreateOutboxRecordInTx creates an outbox record within an existing transaction.
// This ensures atomicity: if the domain operation fails, the event is not stored.
func CreateOutboxRecordInTx(ctx context.Context, tx *sql.Tx, repo OutboxRepository, event *Event) error {
	payload, err := event.ToJSON()
	if err != nil {
		return fmt.Errorf("marshal event payload: %w", err)
	}

	record := &OutboxRecord{
		ID:            event.ID,
		TenantID:      event.TenantID,
		AggregateType: event.AggregateType,
		AggregateID:   event.AggregateID,
		EventType:     event.EventType,
		Payload:       payload,
		Status:        StatusPending,
		Attempts:      0,
		CreatedAt:     time.Now().UTC(),
	}

	return repo.CreateOutboxRecord(ctx, tx, record)
}

// PublishWithOutbox publishes an event using the transactional outbox pattern.
// It stores the event in the outbox within the same transaction as the domain operation,
// ensuring eventual consistency.
//
// Usage:
//   tx, _ := repo.BeginTx(ctx)
//   defer tx.Rollback()
//   
//   // Perform domain operation (e.g., create subscription)
//   subscription, err := subscriptionRepo.Create(ctx, tx, ...)
//   if err != nil {
//       return err
//   }
//   
//   // Store event in outbox (same transaction)
//   event := events.NewEvent("subscription.created", "subscription", subscription.ID, tenantID, payload)
//   if err := events.CreateOutboxRecordInTx(ctx, tx, outboxRepo, event); err != nil {
//       return err
//   }
//   
//   // Commit transaction
//   return tx.Commit()
func PublishWithOutbox(ctx context.Context, tx *sql.Tx, repo OutboxRepository, event *Event) error {
	return CreateOutboxRecordInTx(ctx, tx, repo, event)
}

