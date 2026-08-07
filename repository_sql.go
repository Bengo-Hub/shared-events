package events

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// SQLOutboxRepository implements OutboxRepository using standard SQL.
// Services can use this as a reference implementation or create their own.
type SQLOutboxRepository struct {
	db     *sql.DB
	logger interface {
		Error(string, ...interface{})
	}
}

// NewSQLOutboxRepository creates a new SQL-based outbox repository.
func NewSQLOutboxRepository(db *sql.DB) *SQLOutboxRepository {
	return &SQLOutboxRepository{db: db}
}

// BeginTx starts a database transaction.
func (r *SQLOutboxRepository) BeginTx(ctx context.Context) (*sql.Tx, error) {
	return r.db.BeginTx(ctx, &sql.TxOptions{
		Isolation: sql.LevelReadCommitted,
	})
}

// CreateOutboxRecord stores an event in the outbox within a transaction.
func (r *SQLOutboxRepository) CreateOutboxRecord(ctx context.Context, tx *sql.Tx, record *OutboxRecord) error {
	query := `
		INSERT INTO outbox_events (
			id, tenant_id, aggregate_type, aggregate_id, event_type,
			payload, status, attempts, created_at
		) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
	`

	_, err := tx.ExecContext(ctx, query,
		record.ID,
		record.TenantID,
		record.AggregateType,
		record.AggregateID,
		record.EventType,
		record.Payload,
		record.Status,
		record.Attempts,
		record.CreatedAt,
	)

	if err != nil {
		return fmt.Errorf("insert outbox record: %w", err)
	}

	return nil
}

// processingClaimStaleAfter bounds how long a claimed (PROCESSING) row is excluded from
// re-selection. The publisher's poll loop runs every few seconds and a batch publish
// completes well within this window, so a PROCESSING row still in that state after it has
// elapsed means the instance that claimed it died mid-publish (pod OOM/crash/reschedule) —
// it's safe, and necessary, to let another replica reclaim and retry it.
const processingClaimStaleAfter = 2 * time.Minute

// GetPendingRecords atomically claims up to `limit` publishable events for THIS instance
// and returns them.
//
// Multiple replicas of the same service all run a publisher polling this table
// concurrently. A plain "SELECT WHERE status = PENDING" here would let two+ replicas fetch
// the same rows in the same poll tick, each publish it to NATS (duplicate delivery to every
// subscriber), and then race on MarkAsPublished's DELETE — the loser logging a spurious
// "event not found". SELECT ... FOR UPDATE SKIP LOCKED inside the UPDATE's subquery makes
// the claim atomic and mutually exclusive across replicas: concurrent claims never intersect.
// The claimed rows are flagged StatusProcessing (not deleted or left PENDING) so a crashed
// claimer's rows are reclaimable after processingClaimStaleAfter instead of being lost.
func (r *SQLOutboxRepository) GetPendingRecords(ctx context.Context, limit int) ([]*OutboxRecord, error) {
	query := `
		UPDATE outbox_events
		SET status = $1, last_attempt_at = $2
		WHERE id IN (
			SELECT id FROM outbox_events
			WHERE status = $3 OR (status = $1 AND last_attempt_at < $4)
			ORDER BY created_at ASC
			LIMIT $5
			FOR UPDATE SKIP LOCKED
		)
		RETURNING
			id, tenant_id, aggregate_type, aggregate_id, event_type,
			payload, status, attempts, last_attempt_at, published_at,
			error_message, created_at
	`

	now := time.Now().UTC()
	staleCutoff := now.Add(-processingClaimStaleAfter)
	rows, err := r.db.QueryContext(ctx, query, StatusProcessing, now, StatusPending, staleCutoff, limit)
	if err != nil {
		return nil, fmt.Errorf("query pending records: %w", err)
	}
	defer rows.Close()

	var records []*OutboxRecord
	for rows.Next() {
		record := &OutboxRecord{}
		var lastAttemptAt, publishedAt sql.NullTime
		var errorMessage sql.NullString

		err := rows.Scan(
			&record.ID,
			&record.TenantID,
			&record.AggregateType,
			&record.AggregateID,
			&record.EventType,
			&record.Payload,
			&record.Status,
			&record.Attempts,
			&lastAttemptAt,
			&publishedAt,
			&errorMessage,
			&record.CreatedAt,
		)
		if err != nil {
			return nil, fmt.Errorf("scan record: %w", err)
		}

		if lastAttemptAt.Valid {
			record.LastAttemptAt = &lastAttemptAt.Time
		}
		if publishedAt.Valid {
			record.PublishedAt = &publishedAt.Time
		}
		if errorMessage.Valid {
			record.ErrorMessage = &errorMessage.String
		}

		records = append(records, record)
	}

	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("rows error: %w", err)
	}

	return records, nil
}

// MarkAsPublished removes a successfully-published event from the outbox.
//
// Successful events are DELETED immediately rather than retained as PUBLISHED:
// once an event has been accepted by JetStream the broker owns delivery to all
// durable consumers, so the outbox row has fulfilled its purpose. Keeping only
// FAILED rows (for troubleshooting) keeps the production outbox tables thin and
// preserves storage. The publishedAt argument is retained for interface
// compatibility but is no longer persisted.
func (r *SQLOutboxRepository) MarkAsPublished(ctx context.Context, id uuid.UUID, _ time.Time) error {
	result, err := r.db.ExecContext(ctx, `DELETE FROM outbox_events WHERE id = $1`, id)
	if err != nil {
		return fmt.Errorf("delete published outbox record: %w", err)
	}

	rowsAffected, err := result.RowsAffected()
	if err != nil {
		return fmt.Errorf("rows affected: %w", err)
	}

	if rowsAffected == 0 {
		return fmt.Errorf("event not found: %s", id)
	}

	return nil
}

// MarkAsFailed records a failed publish attempt. It increments the attempt
// counter and keeps the event PENDING for another retry until it reaches
// MaxOutboxAttempts, at which point it is parked as FAILED (terminal) so the
// poller stops selecting it. This bounds retries and prevents the busy-retry
// loop that occurs when an undeliverable event is perpetually reset to PENDING.
// FAILED rows are deliberately retained for troubleshooting.
func (r *SQLOutboxRepository) MarkAsFailed(ctx context.Context, id uuid.UUID, errorMessage string, lastAttemptAt time.Time) error {
	query := `
		UPDATE outbox_events
		SET attempts = attempts + 1,
		    last_attempt_at = $1,
		    error_message = $2,
		    status = CASE WHEN attempts + 1 >= $3 THEN $4 ELSE $5 END
		WHERE id = $6
	`

	_, err := r.db.ExecContext(ctx, query, lastAttemptAt, errorMessage, MaxOutboxAttempts, StatusFailed, StatusPending, id)
	if err != nil {
		return fmt.Errorf("update outbox record: %w", err)
	}

	return nil
}

