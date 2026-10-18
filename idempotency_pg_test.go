package events

import (
	"context"
	"database/sql"
	"os"
	"testing"
	"time"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// TestPruneOnPostgres needs EVENTS_TEST_POSTGRES_URL (a scratch database).
func TestPruneOnPostgres(t *testing.T) {
	dsn := os.Getenv("EVENTS_TEST_POSTGRES_URL")
	if dsn == "" {
		t.Skip("EVENTS_TEST_POSTGRES_URL not set")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	if _, err := db.ExecContext(ctx, `DROP TABLE IF EXISTS processed_events`); err != nil {
		t.Fatal(err)
	}
	s := NewIdempotencyStore(db)
	if err := s.EnsureSchema(ctx); err != nil {
		t.Fatal(err)
	}
	if err := s.EnsureSchema(ctx); err != nil {
		t.Fatal("EnsureSchema must be safe to run twice:", err)
	}

	// 25 old claims and 3 recent ones.
	for i := 0; i < 25; i++ {
		if _, err := db.ExecContext(ctx, `INSERT INTO processed_events (event_id, consumer, processed_at) VALUES ($1, 'c', now() - interval '40 days')`, uuid.New()); err != nil {
			t.Fatal(err)
		}
	}
	recent := uuid.New()
	for _, id := range []uuid.UUID{recent, uuid.New(), uuid.New()} {
		if err := s.MarkProcessed(ctx, id, "c"); err != nil {
			t.Fatal(err)
		}
	}

	n, err := s.Prune(ctx, 30*24*time.Hour, 10) // three batches: 10, 10, 5
	if err != nil || n != 25 {
		t.Fatalf("pruned %d, %v; want 25", n, err)
	}
	var left int
	if err := db.QueryRowContext(ctx, `SELECT count(*) FROM processed_events`).Scan(&left); err != nil || left != 3 {
		t.Fatalf("left %d, %v; want 3", left, err)
	}
	if ok, _ := s.AlreadyProcessed(ctx, recent, "c"); !ok {
		t.Fatal("a recent claim must survive the prune")
	}
	if n, err := s.Prune(ctx, 0, 0); err != nil || n != 0 {
		t.Fatalf("a second prune with defaults removes nothing: %d, %v", n, err)
	}
}
