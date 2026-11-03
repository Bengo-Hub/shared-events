package balancealert

import (
	"context"
	"errors"
	"testing"
	"time"

	events "github.com/Bengo-Hub/shared-events"
)

func TestCrossed(t *testing.T) {
	cases := []struct {
		before, after, mark float64
		level               string
		crossed             bool
	}{
		{100, 90, 20, "", false},
		{25, 20, 20, LevelLow, true},
		{21, 5, 20, LevelLow, true},
		{20, 10, 20, "", false}, // already low
		{10, 0, 20, LevelEmpty, true},
		{30, -3, 20, LevelEmpty, true}, // overage spend crosses both, empty wins
		{0, -5, 20, "", false},         // already empty
		{10, 30, 20, "", false},        // top-up
		{5, 2, 0, "", false},           // no mark, not empty
	}
	for _, c := range cases {
		l, ok := Crossed(c.before, c.after, c.mark)
		if l != c.level || ok != c.crossed {
			t.Errorf("Crossed(%v,%v,%v) = %q,%v want %q,%v", c.before, c.after, c.mark, l, ok, c.level, c.crossed)
		}
	}
}

func TestLevelOfAndLowMark(t *testing.T) {
	if l, ok := LevelOf(0, 5); !ok || l != LevelEmpty {
		t.Fatalf("zero = %q %v", l, ok)
	}
	if l, ok := LevelOf(4.99, 5); !ok || l != LevelLow {
		t.Fatalf("below = %q %v", l, ok)
	}
	if _, ok := LevelOf(6, 5); ok {
		t.Fatal("above the mark is not low")
	}
	if m := LowMark(500, 0); m != 100 {
		t.Fatalf("default ratio mark = %v", m)
	}
	if m := LowMark(500, 0.1); m != 50 {
		t.Fatalf("mark = %v", m)
	}
}

func TestEventShapeAndKey(t *testing.T) {
	a := Alert{Scope: ScopeTenant, TenantID: "6f1c1b5e-9d1e-4a39-9a43-2d0a1f6c1a11", Resource: "sms", ResourceLabel: "SMS",
		Level: LevelLow, Remaining: 19, Unit: "messages", Threshold: 20, Period: "2026-11", Epoch: "1500"}
	evt, err := a.Event()
	if err != nil {
		t.Fatal(err)
	}
	if evt.Subject() != "billing.balance.low" || Subject != evt.Subject() {
		t.Fatalf("subject = %s", evt.Subject())
	}
	if evt.TenantID.String() != a.TenantID || evt.Payload["resource"] != "sms" || evt.Payload["level"] != "low" {
		t.Fatalf("event = %+v", evt)
	}
	if _, has := evt.Payload["Epoch"]; has {
		t.Fatal("epoch must not be in the payload")
	}
	again, _ := a.Event()
	if again.AggregateID != evt.AggregateID {
		t.Fatal("aggregate id must be stable per dedupe key")
	}
	if k := a.DedupeKey(); k != "balance-low:tenant:"+a.TenantID+":sms:low:2026-11:1500" {
		t.Fatalf("key = %s", k)
	}
	p := Alert{Scope: ScopePlatform, Resource: "fal", Level: LevelEmpty, Period: "2026-11-03"}
	if k := p.DedupeKey(); k != "balance-low:platform:platform:fal:empty:2026-11-03" {
		t.Fatalf("platform key = %s", k)
	}
	if _, err := (Alert{Scope: ScopeTenant, Resource: "sms", Level: LevelLow, Period: "x"}).Event(); err == nil {
		t.Fatal("tenant scope without tenant id must be refused")
	}
}

func TestNotifyClaimsOnce(t *testing.T) {
	claimed := map[string]bool{}
	claim := func(_ context.Context, key string, _ time.Duration) bool {
		if claimed[key] {
			return false
		}
		claimed[key] = true
		return true
	}
	var sent int
	pub := func(_ context.Context, e *events.Event) error { sent++; return nil }
	a := Alert{Scope: ScopePlatform, Resource: "higgsfield", Level: LevelLow, Period: "2026-11-03", Estimate: true}
	for i := 0; i < 3; i++ {
		if _, err := Notify(context.Background(), claim, time.Hour, a, pub); err != nil {
			t.Fatal(err)
		}
	}
	if sent != 1 {
		t.Fatalf("sent %d times, want once", sent)
	}
	boom := errors.New("down")
	a.Level = LevelEmpty
	ok, err := Notify(context.Background(), claim, time.Hour, a, func(context.Context, *events.Event) error { return boom })
	if !ok || !errors.Is(err, boom) {
		t.Fatalf("publish error must surface: %v %v", ok, err)
	}
}
