package events

import (
	"testing"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
)

func TestEventIDFromMsg(t *testing.T) {
	id := uuid.New()

	t.Run("from event-id header", func(t *testing.T) {
		msg := nats.NewMsg("pos.sale.finalized")
		msg.Header.Set("event-id", id.String())
		msg.Data = []byte(`{"payload":{}}`)
		got, err := EventIDFromMsg(msg)
		if err != nil || got != id {
			t.Fatalf("want %s, got %s err=%v", id, got, err)
		}
	})

	t.Run("from body id when no header", func(t *testing.T) {
		msg := nats.NewMsg("ordering.order.confirmed")
		msg.Data = []byte(`{"id":"` + id.String() + `","type":"x"}`)
		got, err := EventIDFromMsg(msg)
		if err != nil || got != id {
			t.Fatalf("want %s, got %s err=%v", id, got, err)
		}
	})

	t.Run("from body event_id fallback", func(t *testing.T) {
		msg := nats.NewMsg("treasury.payment.succeeded")
		msg.Data = []byte(`{"event_id":"` + id.String() + `"}`)
		got, err := EventIDFromMsg(msg)
		if err != nil || got != id {
			t.Fatalf("want %s, got %s err=%v", id, got, err)
		}
	})

	t.Run("error when absent", func(t *testing.T) {
		msg := nats.NewMsg("x.y")
		msg.Data = []byte(`{"foo":"bar"}`)
		if _, err := EventIDFromMsg(msg); err == nil {
			t.Fatal("expected ErrNoEventID, got nil")
		}
	})
}
