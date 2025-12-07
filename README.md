# Shared Events Library

**Repository:** `github.com/Bengo-Hub/shared-events`

Standardized event publishing library for BengoBox microservices, implementing the transactional outbox pattern for reliable event delivery.

## Features

- ✅ **Transactional Outbox Pattern** - Guaranteed event delivery with database transactions
- ✅ **NATS JetStream Integration** - Reliable event publishing
- ✅ **Background Publisher Worker** - Automatic event processing
- ✅ **Event Versioning** - Schema version support
- ✅ **Retry Logic** - Configurable retry attempts and delays
- ✅ **Dead Letter Handling** - Failed events after max attempts

## Installation

```bash
go get github.com/Bengo-Hub/shared-events@v0.1.0
```

## Usage

### 1. Define Outbox Table

Each service needs an `outbox_events` table. If using Ent, add to your schema:

```go
// internal/ent/schema/outboxevent.go
package schema

import (
	"time"
	"entgo.io/ent"
	"entgo.io/ent/schema/field"
	"github.com/google/uuid"
)

type OutboxEvent struct {
	ent.Schema
}

func (OutboxEvent) Fields() []ent.Field {
	return []ent.Field{
		field.UUID("id", uuid.UUID{}).Default(uuid.New).Immutable(),
		field.UUID("tenant_id", uuid.UUID{}),
		field.String("aggregate_type").NotEmpty(),
		field.UUID("aggregate_id", uuid.UUID{}),
		field.String("event_type").NotEmpty(),
		field.JSON("payload", map[string]any{}),
		field.String("status").Default("PENDING"),
		field.Int("attempts").Default(0),
		field.Time("last_attempt_at").Optional(),
		field.Time("published_at").Optional(),
		field.String("error_message").Optional(),
		field.Time("created_at").Default(time.Now).Immutable(),
	}
}
```

### 2. Implement OutboxRepository

Either use the provided `SQLOutboxRepository` or implement your own:

```go
import (
	"github.com/Bengo-Hub/shared-events"
	"database/sql"
)

// Using SQL repository
outboxRepo := events.NewSQLOutboxRepository(db)

// Or implement custom repository
type CustomOutboxRepository struct {
	entClient *ent.Client
}

func (r *CustomOutboxRepository) CreateOutboxRecord(ctx context.Context, tx *sql.Tx, record *events.OutboxRecord) error {
	// Implementation using Ent or your ORM
}
```

### 3. Publish Events with Outbox

```go
import (
	"context"
	"database/sql"
	events "github.com/Bengo-Hub/shared-events"
	"github.com/google/uuid"
)

// In your domain service method
func (s *SubscriptionService) CreateSubscription(ctx context.Context, tenantID uuid.UUID, planID uuid.UUID) (*Subscription, error) {
	// Begin transaction
	tx, err := outboxRepo.BeginTx(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()

	// Perform domain operation
	subscription, err := s.repo.Create(ctx, tx, &Subscription{
		TenantID: tenantID,
		PlanID:   planID,
		Status:   "active",
	})
	if err != nil {
		return nil, err
	}

	// Create event
	event := events.NewEvent(
		"subscription.created",
		"subscription",
		subscription.ID,
		tenantID,
		map[string]interface{}{
			"subscription_id": subscription.ID,
			"plan_id":         planID,
			"tenant_id":       tenantID,
			"status":          subscription.Status,
		},
	)

	// Store event in outbox (same transaction)
	if err := events.PublishWithOutbox(ctx, tx, outboxRepo, event); err != nil {
		return nil, err
	}

	// Commit transaction
	if err := tx.Commit(); err != nil {
		return nil, err
	}

	return subscription, nil
}
```

### 4. Start Background Publisher

```go
import (
	"context"
	"github.com/Bengo-Hub/shared-events"
	"github.com/nats-io/nats.go"
	"go.uber.org/zap"
)

// In app initialization
func (a *App) StartOutboxPublisher(ctx context.Context) error {
	// Create NATS JetStream context
	js, err := natsConn.JetStream()
	if err != nil {
		return err
	}

	// Create publisher
	cfg := events.DefaultPublisherConfig(js, outboxRepo, logger)
	publisher := events.NewPublisher(cfg)

	// Start in background
	go func() {
		if err := publisher.Start(ctx); err != nil {
			logger.Error("outbox publisher failed", zap.Error(err))
		}
	}()

	return nil
}
```

## Event Structure

Events follow a standard structure:

```json
{
  "id": "uuid",
  "event_type": "subscription.created",
  "aggregate_type": "subscription",
  "aggregate_id": "uuid",
  "tenant_id": "uuid",
  "payload": {
    "subscription_id": "uuid",
    "plan_id": "uuid",
    "status": "active"
  },
  "metadata": {
    "source": "subscription-service",
    "correlation_id": "uuid"
  },
  "timestamp": "2024-12-05T10:30:00Z",
  "version": "1.0"
}
```

## NATS Subject Format

Events are published to NATS subjects following the pattern:
```
{aggregate_type}.{event_type}
```

Example:
- `subscription.created`
- `subscription.upgraded`
- `order.placed`
- `invoice.paid`

## Configuration

Customize publisher behavior:

```go
cfg := events.PublisherConfig{
	JetStream:    js,
	Repository:   outboxRepo,
	Logger:       logger,
	MaxRetries:   10,              // Max retries per event
	RetryDelay:   5 * time.Second, // Delay between retries
	BatchSize:    100,             // Events per batch
	PollInterval: 2 * time.Second, // Poll frequency
	MaxAttempts:  15,              // Max attempts before marking as FAILED
}
publisher := events.NewPublisher(cfg)
```

## Best Practices

1. **Always use transactions** - Store events in the same transaction as domain operations
2. **Idempotent consumers** - Design event handlers to be idempotent
3. **Event versioning** - Use version field for schema evolution
4. **Monitor failed events** - Alert on events that exceed max attempts
5. **Batch processing** - Adjust batch size based on event volume

## Testing

```go
// Mock repository for testing
type MockOutboxRepository struct {
	records []*events.OutboxRecord
}

func (m *MockOutboxRepository) CreateOutboxRecord(ctx context.Context, tx *sql.Tx, record *events.OutboxRecord) error {
	m.records = append(m.records, record)
	return nil
}

// ... implement other methods
```

## Migration from Direct Publishing

If you're currently publishing directly to NATS:

1. Add `outbox_events` table
2. Replace direct `js.Publish()` calls with `PublishWithOutbox()`
3. Start background publisher worker
4. Deploy and monitor

Events will now be reliably delivered even if NATS is temporarily unavailable.

