package events

import "github.com/nats-io/nats.go"

// EventHeaders builds the standard NATS headers every JetStream publish carries.
//
// Nats-Msg-Id is set to the event ID so JetStream drops a second publish of the same event
// inside the stream's duplicate window (2 minutes by default). The outbox needs this: when a
// pod dies after PublishMsg succeeds but before MarkAsPublished runs, another replica
// reclaims the stale PROCESSING row and publishes it again. Without the header, every
// subscriber would receive that event twice.
func EventHeaders(event *Event) nats.Header {
	headers := make(nats.Header)
	headers.Set(nats.MsgIdHdr, event.ID.String())
	headers.Set("event-id", event.ID.String())
	headers.Set("event-type", event.EventType)
	headers.Set("aggregate-type", event.AggregateType)
	headers.Set("aggregate-id", event.AggregateID.String())
	headers.Set("tenant-id", event.TenantID.String())
	headers.Set("event-version", event.Version)
	return headers
}
