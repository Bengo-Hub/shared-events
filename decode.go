package events

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/google/uuid"
)

// Envelope is the shared-events wire envelope with the payload left RAW so DecodeEvent can
// unmarshal it into a caller-specified type without the lossiness of map[string]interface{}
// (which turns every number into a float64 and drops type fidelity).
type Envelope struct {
	ID            uuid.UUID              `json:"id"`
	EventType     string                 `json:"event_type"`
	AggregateType string                 `json:"aggregate_type"`
	AggregateID   uuid.UUID              `json:"aggregate_id"`
	TenantID      uuid.UUID              `json:"tenant_id"`
	TenantSlug    string                 `json:"tenant_slug,omitempty"`
	Payload       json.RawMessage        `json:"payload"`
	Metadata      map[string]interface{} `json:"metadata,omitempty"`
	Timestamp     time.Time              `json:"timestamp"`
	Version       string                 `json:"version,omitempty"`
}

// DecodeEvent decodes a shared-events envelope and unmarshals its `payload` object into a
// typed value T, also returning the parsed envelope for the top-level fields (TenantID,
// TenantSlug, EventType, ID, ...).
//
// This is the ONE correct way to read an event payload. It eliminates the recurring class of
// bugs where a consumer reads business fields at the TOP LEVEL or under a `data` key instead
// of under `payload` — the envelope shape is fixed here, so the wrong shape is impossible.
//
//	type userCreated struct {
//	    UserID string `json:"user_id"`
//	    Email  string `json:"email"`
//	}
//	env, p, err := events.DecodeEvent[userCreated](msg.Data)
//	// env.TenantID is the envelope tenant; p.UserID / p.Email come from payload.
//
// Callers that also need tenant_id from the payload (some publishers duplicate it there)
// should prefer env.TenantID and fall back to a payload field only when it is uuid.Nil.
func DecodeEvent[T any](data []byte) (Envelope, T, error) {
	var (
		env     Envelope
		payload T
	)
	if err := json.Unmarshal(data, &env); err != nil {
		return env, payload, fmt.Errorf("decode event envelope: %w", err)
	}
	if len(env.Payload) > 0 {
		if err := json.Unmarshal(env.Payload, &payload); err != nil {
			return env, payload, fmt.Errorf("decode event payload: %w", err)
		}
	}
	return env, payload, nil
}
