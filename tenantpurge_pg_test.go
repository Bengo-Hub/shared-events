package events

import (
	"context"
	"database/sql"
	"os"
	"strings"
	"testing"

	"github.com/google/uuid"
	_ "github.com/jackc/pgx/v5/stdlib"
)

// TestTenantPurgerOnPostgres needs EVENTS_TEST_POSTGRES_URL (a scratch database). It runs in a
// throwaway schema: a child listed before its parent (FK order found by retrying), a text
// tenant_id, an outbox that must survive, another tenant that must survive, and the tenant row.
func TestTenantPurgerOnPostgres(t *testing.T) {
	dsn := os.Getenv("EVENTS_TEST_POSTGRES_URL")
	if dsn == "" {
		t.Skip("EVENTS_TEST_POSTGRES_URL not set")
	}
	admin, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer admin.Close()
	schema := "tp_" + strings.ReplaceAll(uuid.NewString()[:8], "-", "")
	if _, err := admin.Exec("CREATE SCHEMA " + schema); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = admin.Exec("DROP SCHEMA " + schema + " CASCADE") })
	sep := "?"
	if strings.Contains(dsn, "?") {
		sep = "&"
	}
	db, err := sql.Open("pgx", dsn+sep+"search_path="+schema)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := context.Background()
	gone, kept := uuid.New(), uuid.New()
	for _, stmt := range []string{
		`CREATE TABLE tenants (id uuid PRIMARY KEY, slug text)`,
		`CREATE TABLE a_parent (id uuid PRIMARY KEY, tenant_id uuid NOT NULL)`,
		`CREATE TABLE a_child (id uuid PRIMARY KEY, tenant_id uuid NOT NULL, parent_id uuid REFERENCES a_parent(id))`,
		`CREATE TABLE notes (id serial PRIMARY KEY, tenant_id varchar NOT NULL)`,
		`CREATE TABLE outbox_events (id serial PRIMARY KEY, tenant_id uuid NOT NULL)`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatal(stmt, err)
		}
	}
	for _, tid := range []uuid.UUID{gone, kept} {
		parent := uuid.New()
		for _, q := range []struct {
			sql  string
			args []any
		}{
			{`INSERT INTO tenants VALUES ($1, 'x')`, []any{tid}},
			{`INSERT INTO a_parent VALUES ($1, $2)`, []any{parent, tid}},
			{`INSERT INTO a_child VALUES ($1, $2, $3)`, []any{uuid.New(), tid, parent}},
			{`INSERT INTO notes (tenant_id) VALUES ($1)`, []any{tid.String()}},
			{`INSERT INTO outbox_events (tenant_id) VALUES ($1)`, []any{tid}},
		} {
			if _, err := db.Exec(q.sql, q.args...); err != nil {
				t.Fatal(q.sql, err)
			}
		}
	}

	n, err := (&TenantPurger{DB: db, Service: "test"}).Purge(ctx, gone)
	if err != nil {
		t.Fatal(err)
	}
	if n != 4 { // parent, child, note, tenant row
		t.Fatalf("deleted %d rows, want 4", n)
	}
	count := func(q string, args ...any) int {
		var c int
		if err := db.QueryRow(q, args...).Scan(&c); err != nil {
			t.Fatal(err)
		}
		return c
	}
	if c := count(`SELECT count(*) FROM a_parent WHERE tenant_id = $1`, gone) + count(`SELECT count(*) FROM tenants WHERE id = $1`, gone); c != 0 {
		t.Fatalf("%d rows of the purged tenant remain", c)
	}
	if c := count(`SELECT count(*) FROM outbox_events WHERE tenant_id = $1`, gone); c != 1 {
		t.Fatal("the outbox must be left alone")
	}
	if c := count(`SELECT count(*) FROM a_child WHERE tenant_id = $1`, kept) + count(`SELECT count(*) FROM notes WHERE tenant_id = $1`, kept.String()); c != 2 {
		t.Fatalf("the other tenant lost rows: %d left, want 2", c)
	}
}

func TestConfirmedTenantPurge(t *testing.T) {
	id := uuid.New()
	evt := NewTenantPurgeEvent(id, "shop", PurgeReasonRegistrationRejected)
	if got, reason, ok := ConfirmedTenantPurge(evt); !ok || got != id || reason != PurgeReasonRegistrationRejected {
		t.Fatalf("a confirmed registration_rejected purge must be accepted: %v %s %v", got, reason, ok)
	}
	evt.Payload["confirmed"] = false
	if _, _, ok := ConfirmedTenantPurge(evt); ok {
		t.Fatal("an unconfirmed purge must be ignored")
	}
	evt.Payload["confirmed"], evt.Payload["reason"] = true, "because"
	if _, _, ok := ConfirmedTenantPurge(evt); ok {
		t.Fatal("an unknown reason must be ignored")
	}
}
