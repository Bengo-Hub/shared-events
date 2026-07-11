package events

import (
	"testing"

	"github.com/google/uuid"
)

func TestDecodeEvent(t *testing.T) {
	type userCreated struct {
		UserID string   `json:"user_id"`
		Email  string   `json:"email"`
		Roles  []string `json:"roles"`
	}

	tenant := uuid.New()
	agg := uuid.New()
	evt := NewEvent("created", "auth.user", agg, tenant, map[string]interface{}{
		"user_id": "u-123",
		"email":   "jane@example.com",
		"roles":   []string{"admin", "finance_admin"},
	}).WithTenantSlug("acme")

	data, err := evt.ToJSON()
	if err != nil {
		t.Fatalf("ToJSON: %v", err)
	}

	env, p, err := DecodeEvent[userCreated](data)
	if err != nil {
		t.Fatalf("DecodeEvent: %v", err)
	}
	if env.TenantID != tenant {
		t.Errorf("envelope tenant = %s, want %s", env.TenantID, tenant)
	}
	if env.TenantSlug != "acme" {
		t.Errorf("envelope slug = %q, want acme", env.TenantSlug)
	}
	if env.AggregateType != "auth.user" || env.EventType != "created" {
		t.Errorf("envelope aggregate/event = %s.%s, want auth.user.created", env.AggregateType, env.EventType)
	}
	if p.UserID != "u-123" || p.Email != "jane@example.com" {
		t.Errorf("payload = %+v, want user_id/email populated", p)
	}
	if len(p.Roles) != 2 || p.Roles[0] != "admin" {
		t.Errorf("payload roles = %v, want [admin finance_admin]", p.Roles)
	}
}

// TestDecodeEvent_TopLevelIgnored guards the core contract: business fields at the TOP
// level (not under payload) are NOT read — that is the exact bug this helper prevents.
func TestDecodeEvent_TopLevelIgnored(t *testing.T) {
	type body struct {
		Name string `json:"name"`
	}
	// A malformed producer that put `name` at the top level instead of under payload.
	raw := []byte(`{"tenant_id":"` + uuid.Nil.String() + `","name":"top-level","payload":{}}`)
	_, p, err := DecodeEvent[body](raw)
	if err != nil {
		t.Fatalf("DecodeEvent: %v", err)
	}
	if p.Name != "" {
		t.Errorf("top-level field leaked into payload: %q", p.Name)
	}
}
