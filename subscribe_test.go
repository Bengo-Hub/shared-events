package events

import (
	"errors"
	"testing"

	"github.com/nats-io/nats.go"
)

func TestStreamMissing(t *testing.T) {
	if !streamMissing(nats.ErrStreamNotFound) {
		t.Fatal("nats.ErrStreamNotFound must wait for the stream")
	}
	if streamMissing(errors.New("nats: consumer is already bound")) || streamMissing(nil) {
		t.Fatal("other errors keep their own handling")
	}
}
