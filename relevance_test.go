package events

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/google/uuid"
)

func TestUserRelevance(t *testing.T) {
	tenant := uuid.New()
	mine, other := uuid.New(), uuid.New()
	r := UserRelevance{
		ServiceRoles: map[string]bool{"librarian": true},
		OutletKnown:  func(_ context.Context, _ uuid.UUID, id uuid.UUID) bool { return id == mine },
	}
	cases := []struct {
		name    string
		payload map[string]any
		want    bool
	}{
		{"admin always", map[string]any{"roles": []any{"admin"}, "outlet_ids": []any{other.String()}}, true},
		{"own outlet", map[string]any{"roles": []any{"manager"}, "outlet_ids": []any{other.String(), mine.String()}}, true},
		{"single outlet_id", map[string]any{"roles": []any{"cashier"}, "outlet_id": mine.String()}, true},
		{"only other outlets", map[string]any{"roles": []any{"librarian"}, "outlet_ids": []any{other.String()}}, false},
		{"service role, no outlets", map[string]any{"roles": []any{"Librarian"}}, true},
		{"generic role, no outlets", map[string]any{"roles": []any{"manager", "member"}}, false},
		{"no roles", map[string]any{}, false},
	}
	for _, c := range cases {
		if got := r.Relevant(context.Background(), tenant, c.payload); got != c.want {
			t.Errorf("%s: got %v, want %v", c.name, got, c.want)
		}
	}
}

func TestPayloadOutletIDsDedupes(t *testing.T) {
	id := uuid.New()
	got := PayloadOutletIDs(map[string]any{"outlet_ids": []any{id.String(), "bad"}, "outlet_id": id.String()})
	if len(got) != 1 || got[0] != id {
		t.Fatalf("got %v", got)
	}
}

// auth-api's services list decides when present: it overrides the role and outlet rules either
// way, an empty list is a decided "no", and Revoked only fires on a decided "no".
func TestUserRelevanceServicesList(t *testing.T) {
	ctx := context.Background()
	r := UserRelevance{Service: "inventory", ServiceRoles: map[string]bool{"storekeeper": true}}
	decode := func(s string) map[string]any {
		var m map[string]any
		if err := json.Unmarshal([]byte(s), &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	cases := []struct {
		payload          string
		relevant, revoke bool
	}{
		{`{"roles":["cashier"],"services":["inventory","pos"]}`, true, false},
		{`{"roles":["admin"],"services":["pos"]}`, false, true},
		{`{"roles":["storekeeper"],"services":[]}`, false, true},
		{`{"roles":["cashier"],"services":["*"]}`, true, false},
		{`{"roles":["storekeeper"]}`, true, false},
		{`{"roles":["cashier"],"services":null}`, false, false},
	}
	for _, tc := range cases {
		p := decode(tc.payload)
		if got := r.Relevant(ctx, uuid.New(), p); got != tc.relevant {
			t.Errorf("%s: relevant %v, want %v", tc.payload, got, tc.relevant)
		}
		if got := r.Revoked(p); got != tc.revoke {
			t.Errorf("%s: revoked %v, want %v", tc.payload, got, tc.revoke)
		}
	}
	if (UserRelevance{}).Revoked(decode(`{"services":[]}`)) {
		t.Error("without Service nothing is ever revoked")
	}
}
