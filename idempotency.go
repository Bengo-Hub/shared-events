package events

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
)

// ProcessedEventsSchema is the DDL each consuming service runs once (idempotent)
// to back the IdempotencyStore. The composite PK (event_id, consumer) lets
// several independent consumers each handle the same event exactly once.
const ProcessedEventsSchema = `
CREATE TABLE IF NOT EXISTS processed_events (
    event_id     uuid        NOT NULL,
    consumer     text        NOT NULL,
    processed_at timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (event_id, consumer)
);`

// IdempotencyStore records which (event_id, consumer) pairs have already been
// handled so a JetStream redelivery — or a duplicate delivered to a second
// replica — applies its side effects at most once. Backed by a raw *sql.DB,
// mirroring SQLOutboxRepository so it drops into the same services.
type IdempotencyStore struct {
	db *sql.DB
}

// NewIdempotencyStore creates a store over the given database handle.
func NewIdempotencyStore(db *sql.DB) *IdempotencyStore { return &IdempotencyStore{db: db} }

// EnsureSchema creates the processed_events table if it does not exist.
func (s *IdempotencyStore) EnsureSchema(ctx context.Context) error {
	if _, err := s.db.ExecContext(ctx, ProcessedEventsSchema); err != nil {
		return fmt.Errorf("ensure processed_events schema: %w", err)
	}
	return nil
}

// Claim atomically records (eventID, consumer) and reports whether THIS call won
// the claim: true = first time → proceed with side effects; false = already
// taken → skip (duplicate). Race-safe across replicas via the primary key +
// ON CONFLICT DO NOTHING. This is the recommended gate for irreversible side
// effects (e.g. a supplier payout): claim BEFORE acting so it can never run twice.
func (s *IdempotencyStore) Claim(ctx context.Context, eventID uuid.UUID, consumer string) (bool, error) {
	res, err := s.db.ExecContext(ctx,
		`INSERT INTO processed_events (event_id, consumer) VALUES ($1, $2)
		 ON CONFLICT (event_id, consumer) DO NOTHING`,
		eventID, consumer)
	if err != nil {
		return false, fmt.Errorf("claim processed event: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("claim rows affected: %w", err)
	}
	return n == 1, nil
}

// AlreadyProcessed reports whether (eventID, consumer) was already handled,
// without claiming it. Prefer Claim when the check guards a side effect.
func (s *IdempotencyStore) AlreadyProcessed(ctx context.Context, eventID uuid.UUID, consumer string) (bool, error) {
	var exists bool
	err := s.db.QueryRowContext(ctx,
		`SELECT EXISTS(SELECT 1 FROM processed_events WHERE event_id = $1 AND consumer = $2)`,
		eventID, consumer).Scan(&exists)
	if err != nil {
		return false, fmt.Errorf("check processed event: %w", err)
	}
	return exists, nil
}

// MarkProcessed records (eventID, consumer); a duplicate is a no-op. Use this for
// the process-THEN-mark pattern (at-least-once delivery + dedupe) when the side
// effect is itself safe to retry but should be acknowledged once.
func (s *IdempotencyStore) MarkProcessed(ctx context.Context, eventID uuid.UUID, consumer string) error {
	if _, err := s.db.ExecContext(ctx,
		`INSERT INTO processed_events (event_id, consumer) VALUES ($1, $2)
		 ON CONFLICT (event_id, consumer) DO NOTHING`,
		eventID, consumer); err != nil {
		return fmt.Errorf("mark processed event: %w", err)
	}
	return nil
}

// ErrNoEventID is returned when a message carries no usable event id.
var ErrNoEventID = errors.New("events: message has no event id")

// EventIDFromMsg extracts the canonical event id from a NATS message: the
// "event-id" header set by the outbox Publisher, falling back to "id" / "event_id"
// in the JSON body for native-envelope publishers. The returned id is the key to
// pass to Claim / AlreadyProcessed / MarkProcessed.
func EventIDFromMsg(msg *nats.Msg) (uuid.UUID, error) {
	if msg.Header != nil {
		if v := msg.Header.Get("event-id"); v != "" {
			return uuid.Parse(v)
		}
	}
	var body struct {
		ID      string `json:"id"`
		EventID string `json:"event_id"`
	}
	if err := json.Unmarshal(msg.Data, &body); err == nil {
		if body.ID != "" {
			return uuid.Parse(body.ID)
		}
		if body.EventID != "" {
			return uuid.Parse(body.EventID)
		}
	}
	return uuid.Nil, ErrNoEventID
}
