// Package balancealert is the one low-balance contract for every kind of credit on the platform.
//
// A tenant's prepaid wallets (AI credits, eTIMS API tokens, SMS, WhatsApp and any service added
// to subscriptions-api's credit registry later) and the platform owner's provider balances (fal,
// Higgsfield, ElevenLabs, messaging gateways) all raise the same event, billing.balance.low,
// and notifications-api has one consumer for it. The owning service only decides the numbers it
// knows (remaining, the low mark, the period); this package decides when that is a crossing and
// builds the event and the dedupe key, so the rule lives in one place.
//
// Two ways to use it:
//   - A spend path that knows the balance before and after one movement calls Crossed.
//   - A poller that only sees the current balance calls LevelOf on every tick and lets the
//     dedupe key (once per period) keep it to one alert.
//
// Either way, Notify claims the dedupe key and then publishes. The claim backend is the caller's
// (Redis ClaimOnce, or a database row inside the spend transaction), so this package carries no
// storage dependency.
package balancealert

import (
	"context"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"

	events "github.com/Bengo-Hub/shared-events"
)

// Subject parts. The shared-events subject is {aggregate_type}.{event_type}, so the event type
// never repeats the aggregate. StreamSubjects is the wildcard the owning stream must bind
// (subscriptions-api's "subscription" stream binds it; any service may publish into it).
const (
	AggregateType  = "billing"
	EventType      = "balance.low"
	Subject        = AggregateType + "." + EventType
	StreamSubjects = AggregateType + ".>"
)

// Scopes.
const (
	ScopeTenant   = "tenant"
	ScopePlatform = "platform"
)

// Levels.
const (
	LevelLow   = "low"
	LevelEmpty = "empty"
)

// DefaultLowRatio is the share of a period's total at which a tenant is warned.
const DefaultLowRatio = 0.20

// Alert is the event payload. Field names are the wire contract read by notifications-api.
type Alert struct {
	Scope    string `json:"scope"`
	TenantID string `json:"tenant_id,omitempty"` // tenant scope only
	// Resource is the credit service code (ai_credits, etims_api, sms, whatsapp) or the provider
	// (fal, higgsfield, elevenlabs).
	Resource      string    `json:"resource"`
	ResourceLabel string    `json:"resource_label"`
	Level         string    `json:"level"`
	Remaining     float64   `json:"remaining"`
	Unit          string    `json:"unit"`      // credits, tokens, messages, usd, characters
	Threshold     float64   `json:"threshold"` // the low mark in Unit
	Period        string    `json:"period"`    // what "once" means, e.g. 2026-11 or 2026-11-03
	BuyURL        string    `json:"buy_url,omitempty"`
	Estimate      bool      `json:"estimate,omitempty"` // the remaining figure is derived, not read
	OccurredAt    time.Time `json:"occurred_at"`
	// Epoch narrows "once per period" further without showing in the payload, for example the
	// wallet's lifetime top-ups, so a top-up re-arms the alert inside the same month.
	Epoch string `json:"-"`
}

// LowMark returns ratio of total, the remaining figure at or below which a balance is low.
// A ratio outside (0,1) falls back to DefaultLowRatio.
func LowMark(total, ratio float64) float64 {
	if ratio <= 0 || ratio >= 1 {
		ratio = DefaultLowRatio
	}
	if total <= 0 {
		return 0
	}
	return total * ratio
}

// Crossed reports the level a single movement crossed into, from before to after remaining. A
// movement that stays on one side of a mark, or moves up, crosses nothing. Empty wins over low
// when one spend crosses both.
func Crossed(before, after, lowMark float64) (level string, crossed bool) {
	if after >= before {
		return "", false
	}
	if before > 0 && after <= 0 {
		return LevelEmpty, true
	}
	if lowMark > 0 && before > lowMark && after <= lowMark {
		return LevelLow, true
	}
	return "", false
}

// LevelOf classifies a balance read on its own (a poller). It returns false when the balance
// is above the mark.
func LevelOf(remaining, lowMark float64) (level string, low bool) {
	if remaining <= 0 {
		return LevelEmpty, true
	}
	if lowMark > 0 && remaining <= lowMark {
		return LevelLow, true
	}
	return "", false
}

// Owner is the tenant id for tenant scope and "platform" otherwise.
func (a Alert) Owner() string {
	if a.Scope == ScopeTenant {
		return a.TenantID
	}
	return ScopePlatform
}

// DedupeKey is the "once" key: scope, owner, resource, level, period and epoch.
func (a Alert) DedupeKey() string {
	k := strings.Join([]string{"balance-low", a.Scope, a.Owner(), a.Resource, a.Level, a.Period}, ":")
	if a.Epoch != "" {
		k += ":" + a.Epoch
	}
	return k
}

// Validate checks the fields the consumer needs.
func (a Alert) Validate() error {
	switch {
	case a.Scope != ScopeTenant && a.Scope != ScopePlatform:
		return fmt.Errorf("balancealert: scope %q", a.Scope)
	case a.Scope == ScopeTenant && a.TenantID == "":
		return fmt.Errorf("balancealert: tenant scope needs tenant_id")
	case a.Resource == "":
		return fmt.Errorf("balancealert: resource is required")
	case a.Level != LevelLow && a.Level != LevelEmpty:
		return fmt.Errorf("balancealert: level %q", a.Level)
	case a.Period == "":
		return fmt.Errorf("balancealert: period is required")
	}
	return nil
}

// Payload is the event payload map.
func (a Alert) Payload() map[string]interface{} {
	if a.OccurredAt.IsZero() {
		a.OccurredAt = time.Now().UTC()
	}
	p := map[string]interface{}{
		"scope": a.Scope, "resource": a.Resource, "resource_label": a.label(), "level": a.Level,
		"remaining": round2(a.Remaining), "unit": a.Unit, "threshold": round2(a.Threshold),
		"period": a.Period, "occurred_at": a.OccurredAt.UTC().Format(time.RFC3339),
	}
	if a.TenantID != "" {
		p["tenant_id"] = a.TenantID
	}
	if a.BuyURL != "" {
		p["buy_url"] = a.BuyURL
	}
	if a.Estimate {
		p["estimate"] = true
	}
	return p
}

// Event builds the shared-events envelope. The aggregate id is derived from the dedupe key, so
// the same alert always carries the same aggregate id; the tenant id is uuid.Nil for platform
// scope.
func (a Alert) Event() (*events.Event, error) {
	if err := a.Validate(); err != nil {
		return nil, err
	}
	tenant := uuid.Nil
	if a.Scope == ScopeTenant {
		id, err := uuid.Parse(a.TenantID)
		if err != nil {
			return nil, fmt.Errorf("balancealert: tenant_id: %w", err)
		}
		tenant = id
	}
	agg := uuid.NewSHA1(uuid.NameSpaceURL, []byte(a.DedupeKey()))
	return events.NewEvent(EventType, AggregateType, agg, tenant, a.Payload()), nil
}

// ClaimFunc claims key for ttl and reports whether this caller is the first. sharedcache.ClaimOnce
// has this shape.
type ClaimFunc func(ctx context.Context, key string, ttl time.Duration) bool

// PublishFunc sends the event (a JetStream publish, or an outbox insert inside a transaction).
type PublishFunc func(ctx context.Context, evt *events.Event) error

// Notify claims the alert's dedupe key and publishes the event. It returns false without
// publishing when the key was already claimed. A nil claim publishes every time (tests, a single
// instance). The claim is taken first so two replicas never both publish; a publish error after a
// successful claim is returned for the caller to log, and the next period retries.
func Notify(ctx context.Context, claim ClaimFunc, ttl time.Duration, a Alert, publish PublishFunc) (bool, error) {
	evt, err := a.Event()
	if err != nil {
		return false, err
	}
	if claim != nil && !claim(ctx, a.DedupeKey(), ttl) {
		return false, nil
	}
	if err := publish(ctx, evt); err != nil {
		return true, err
	}
	return true, nil
}

func (a Alert) label() string {
	if a.ResourceLabel != "" {
		return a.ResourceLabel
	}
	return a.Resource
}

func round2(v float64) float64 { return math.Round(v*100) / 100 }
