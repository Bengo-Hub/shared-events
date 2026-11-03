package events

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"go.uber.org/zap"
)

// Tenant purge: a confirmed, irreversible deletion of everything a tenant owns, on the
// "tenant.purge" subject of the "tenant" stream. subscriptions-api sends it for a dormant account
// a platform owner confirmed; auth-api sends it when a platform admin deletes a tenant or declines
// its registration. Every service with tenant data deletes that tenant's rows on receipt.

const (
	TenantStream       = "tenant"
	TenantPurgeSubject = "tenant.purge"
)

// Purge reasons a service acts on. Anything else is acked and ignored.
const (
	PurgeReasonDormancy             = "dormancy"
	PurgeReasonRegistrationRejected = "registration_rejected"
	PurgeReasonTenantDeleted        = "tenant_deleted"
)

var purgeReasons = map[string]bool{
	PurgeReasonDormancy: true, PurgeReasonRegistrationRejected: true, PurgeReasonTenantDeleted: true,
}

// ConfirmedTenantPurge reports the tenant a tenant.purge event deletes, and whether the event is
// a confirmed purge for a known reason. ok=false means: ack and do nothing.
func ConfirmedTenantPurge(evt *Event) (tenantID uuid.UUID, reason string, ok bool) {
	if evt == nil {
		return uuid.Nil, "", false
	}
	tenantID = evt.TenantID
	if raw, _ := evt.Payload["tenant_id"].(string); raw != "" {
		if id, err := uuid.Parse(raw); err == nil && id != uuid.Nil {
			tenantID = id
		}
	}
	confirmed, _ := evt.Payload["confirmed"].(bool)
	reason, _ = evt.Payload["reason"].(string)
	return tenantID, reason, tenantID != uuid.Nil && confirmed && purgeReasons[reason]
}

// NewTenantPurgeEvent builds the confirmed tenant.purge event for a reason above.
func NewTenantPurgeEvent(tenantID uuid.UUID, slug, reason string) *Event {
	evt := NewEvent("purge", "tenant", tenantID, tenantID, map[string]any{
		"tenant_id":    tenantID.String(),
		"tenant_slug":  slug,
		"reason":       reason,
		"confirmed":    true,
		"confirmed_at": time.Now().UTC().Format(time.RFC3339),
		"scope":        "all_services",
	})
	if slug != "" {
		evt.WithTenantSlug(slug)
	}
	return evt
}

// TenantPurger deletes every row a tenant owns in one service database: each table of the current
// schema with a tenant_id column, then the tenant row itself (tables named tenants or
// auth_tenants, by id). The table list comes from the database, so a new table is never missed.
// Foreign-key order is found by retrying: a table blocked by a child is tried again on the next
// pass, each attempt inside a savepoint, all in one transaction.
type TenantPurger struct {
	DB      *sql.DB
	Log     *zap.Logger
	Service string // durable "<service>-tenant-purge"
	// Skip lists tables never purged. Outbox and processed-event tables are always skipped:
	// events still in flight must keep their rows.
	Skip []string
	// TenantTables lists the tables keyed by the tenant's own id (default tenants, auth_tenants).
	TenantTables []string
}

type tenantColumn struct {
	table string
	uuid  bool
}

// Purge deletes the tenant's rows and returns how many went.
func (p *TenantPurger) Purge(ctx context.Context, tenantID uuid.UUID) (int64, error) {
	if p == nil || p.DB == nil {
		return 0, errors.New("tenant purge: no database")
	}
	if tenantID == uuid.Nil {
		return 0, errors.New("tenant purge: refusing a nil tenant id")
	}
	cols, err := p.tenantColumns(ctx)
	if err != nil {
		return 0, err
	}
	tx, err := p.DB.BeginTx(ctx, nil)
	if err != nil {
		return 0, fmt.Errorf("tenant purge: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }()

	var total int64
	pending := cols
	for pass := 1; len(pending) > 0; pass++ {
		if pass > 30 {
			return 0, fmt.Errorf("tenant purge: foreign keys still block %d tables", len(pending))
		}
		var blocked []tenantColumn
		for _, c := range pending {
			cond := "tenant_id::text = $1"
			if c.uuid {
				cond = "tenant_id = $1::uuid"
			}
			n, err := execInSavepoint(ctx, tx, fmt.Sprintf(`DELETE FROM %s WHERE %s`, quoteIdent(c.table), cond), tenantID.String())
			if isForeignKeyViolation(err) {
				blocked = append(blocked, c)
				continue
			}
			if err != nil {
				return 0, fmt.Errorf("tenant purge: %s: %w", c.table, err)
			}
			total += n
		}
		if len(blocked) == len(pending) {
			return 0, fmt.Errorf("tenant purge: foreign keys block %d tables", len(blocked))
		}
		pending = blocked
	}
	tenantTables := p.TenantTables
	if tenantTables == nil {
		tenantTables = []string{"tenants", "auth_tenants"}
	}
	for _, t := range tenantTables {
		var exists bool
		if err := tx.QueryRowContext(ctx, `SELECT EXISTS (SELECT 1 FROM information_schema.tables
			WHERE table_schema = current_schema() AND table_name = $1)`, t).Scan(&exists); err != nil {
			return 0, fmt.Errorf("tenant purge: %w", err)
		}
		if !exists {
			continue
		}
		n, err := execInSavepoint(ctx, tx, fmt.Sprintf(`DELETE FROM %s WHERE id::text = $1`, quoteIdent(t)), tenantID.String())
		if err != nil {
			return 0, fmt.Errorf("tenant purge: %s: %w", t, err)
		}
		total += n
	}
	if err := tx.Commit(); err != nil {
		return 0, fmt.Errorf("tenant purge: commit: %w", err)
	}
	return total, nil
}

func (p *TenantPurger) tenantColumns(ctx context.Context) ([]tenantColumn, error) {
	rows, err := p.DB.QueryContext(ctx, `
		SELECT c.table_name, c.data_type = 'uuid'
		FROM information_schema.columns c
		JOIN information_schema.tables t ON t.table_schema = c.table_schema AND t.table_name = c.table_name
		WHERE c.table_schema = current_schema() AND t.table_type = 'BASE TABLE' AND c.column_name = 'tenant_id'
		  AND c.data_type IN ('uuid', 'text', 'character varying')
		ORDER BY c.table_name`)
	if err != nil {
		return nil, fmt.Errorf("tenant purge: list tables: %w", err)
	}
	defer rows.Close()
	skip := map[string]bool{}
	for _, s := range p.Skip {
		skip[s] = true
	}
	var out []tenantColumn
	for rows.Next() {
		var c tenantColumn
		if err := rows.Scan(&c.table, &c.uuid); err != nil {
			return nil, err
		}
		if skip[c.table] || strings.Contains(c.table, "outbox") || strings.Contains(c.table, "processed_event") {
			continue
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

func execInSavepoint(ctx context.Context, tx *sql.Tx, query string, args ...any) (int64, error) {
	if _, err := tx.ExecContext(ctx, "SAVEPOINT tenant_purge"); err != nil {
		return 0, err
	}
	res, err := tx.ExecContext(ctx, query, args...)
	if err != nil {
		if _, rerr := tx.ExecContext(ctx, "ROLLBACK TO SAVEPOINT tenant_purge"); rerr != nil {
			return 0, rerr
		}
		return 0, err
	}
	if _, err := tx.ExecContext(ctx, "RELEASE SAVEPOINT tenant_purge"); err != nil {
		return 0, err
	}
	n, _ := res.RowsAffected()
	return n, nil
}

// isForeignKeyViolation recognises SQLSTATE 23503 from pgx and lib/pq alike.
func isForeignKeyViolation(err error) bool {
	if err == nil {
		return false
	}
	var coded interface{ SQLState() string }
	if errors.As(err, &coded) {
		return coded.SQLState() == "23503"
	}
	return strings.Contains(err.Error(), "23503") || strings.Contains(err.Error(), "violates foreign key")
}

func quoteIdent(s string) string { return `"` + strings.ReplaceAll(s, `"`, `""`) + `"` }

// Subscribe binds the durable "<Service>-tenant-purge" consumer on tenant.purge, creating the
// tenant stream when missing. Confirmed purges delete the tenant's data; anything else is acked.
func (p *TenantPurger) Subscribe(js nats.JetStreamContext) error {
	if js == nil {
		return errors.New("tenant purge: no JetStream")
	}
	if p.Service == "" {
		return errors.New("tenant purge: Service is required for the durable name")
	}
	if _, err := js.StreamInfo(TenantStream); err != nil {
		if _, addErr := js.AddStream(&nats.StreamConfig{
			Name: TenantStream, Subjects: []string{"tenant.>"}, Retention: nats.LimitsPolicy,
			MaxAge: 72 * time.Hour, Storage: nats.FileStorage,
		}); addErr != nil && !errors.Is(addErr, nats.ErrStreamNameAlreadyInUse) {
			return fmt.Errorf("tenant purge: ensure stream: %w", addErr)
		}
	}
	durable := p.Service + "-tenant-purge"
	SubscribeQueueWithRebind(p.Log, js, TenantStream, TenantPurgeSubject, durable, p.handle,
		nats.Durable(durable), nats.DeliverAll(), nats.AckExplicit(),
		nats.AckWait(2*time.Minute), nats.MaxDeliver(5))
	return nil
}

func (p *TenantPurger) handle(msg *nats.Msg) {
	log := p.Log
	if log == nil {
		log = zap.NewNop()
	}
	evt, err := FromJSON(msg.Data)
	if err != nil {
		log.Error("tenant.purge: undecodable, acking", zap.Error(err))
		_ = msg.Ack()
		return
	}
	tenantID, reason, ok := ConfirmedTenantPurge(evt)
	if !ok {
		log.Warn("tenant.purge: not a confirmed purge, acking", zap.String("tenant_id", tenantID.String()), zap.String("reason", reason))
		_ = msg.Ack()
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	n, err := p.Purge(ctx, tenantID)
	if err != nil {
		log.Error("tenant.purge: failed, will retry", zap.String("tenant_id", tenantID.String()), zap.Error(err))
		_ = msg.Nak()
		return
	}
	log.Warn("tenant.purge: tenant data deleted", zap.String("tenant_id", tenantID.String()),
		zap.String("reason", reason), zap.Int64("rows", n))
	_ = msg.Ack()
}
