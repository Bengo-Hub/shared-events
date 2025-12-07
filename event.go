package events

import (
	"encoding/json"
	"time"

	"github.com/google/uuid"
)

// Event represents a domain event with standard fields.
type Event struct {
	ID          uuid.UUID              `json:"id"`
	EventType   string                 `json:"event_type"`
	AggregateType string               `json:"aggregate_type"`
	AggregateID uuid.UUID              `json:"aggregate_id"`
	TenantID    uuid.UUID              `json:"tenant_id"`
	Payload     map[string]interface{} `json:"payload"`
	Metadata    map[string]interface{} `json:"metadata,omitempty"`
	Timestamp   time.Time              `json:"timestamp"`
	Version     string                 `json:"version,omitempty"` // Event schema version
}

// NewEvent creates a new event with standard fields.
func NewEvent(eventType, aggregateType string, aggregateID, tenantID uuid.UUID, payload map[string]interface{}) *Event {
	return &Event{
		ID:            uuid.New(),
		EventType:     eventType,
		AggregateType: aggregateType,
		AggregateID:   aggregateID,
		TenantID:      tenantID,
		Payload:       payload,
		Metadata:      make(map[string]interface{}),
		Timestamp:     time.Now().UTC(),
		Version:       "1.0",
	}
}

// ToJSON serializes the event to JSON.
func (e *Event) ToJSON() ([]byte, error) {
	return json.Marshal(e)
}

// FromJSON deserializes JSON to an event.
func FromJSON(data []byte) (*Event, error) {
	var event Event
	if err := json.Unmarshal(data, &event); err != nil {
		return nil, err
	}
	return &event, nil
}

// Subject returns the NATS subject for this event.
// Format: {aggregate_type}.{event_type}
func (e *Event) Subject() string {
	return e.AggregateType + "." + e.EventType
}

// WithMetadata adds metadata to the event.
func (e *Event) WithMetadata(key string, value interface{}) *Event {
	if e.Metadata == nil {
		e.Metadata = make(map[string]interface{})
	}
	e.Metadata[key] = value
	return e
}

// WithVersion sets the event schema version.
func (e *Event) WithVersion(version string) *Event {
	e.Version = version
	return e
}

