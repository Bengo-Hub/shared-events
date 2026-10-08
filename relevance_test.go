package events

import (
	"context"
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
