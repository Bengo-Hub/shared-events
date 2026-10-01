package events

import (
	"sync"
	"sync/atomic"
)

// FanoutHub is the shared registry behind every realtime hub (WebSocket or SSE): it tracks
// this replica's connected subscribers per tenant and delivers each message to the right
// ones on every replica via a Broadcaster.
//
// It deliberately knows nothing about sockets. A service hub subscribes a connection, then
// runs its own write loop (its WebSocket library, ping interval and write deadline) reading
// Sub.C. That removes the per-hub copies of the registry, relay and origin check that drifted
// apart across services (some re-delivered their own publishes, one never relayed at all).
//
// Delivery rules:
//   - Publish(tenant, "", data): every subscriber of that tenant.
//   - Publish(tenant, scope, data): only subscribers holding that scope (e.g. "user:<id>",
//     "outlet:<id>", "task:<id>").
//   - Never across tenants.
//   - A subscriber whose buffer is full misses the message (slow clients never block others);
//     clients must resync on reconnect.
type FanoutHub struct {
	relay *Broadcaster
	topic string
	buf   int

	mu   sync.RWMutex
	subs map[string]map[*Sub]struct{} // tenant -> subscribers
}

// Sub is one connected client. Read messages from C until it is closed by Unsubscribe.
type Sub struct {
	Tenant string
	scopes map[string]struct{}
	C      chan []byte
	// Dropped counts messages lost to a full buffer (for logging by the owner).
	Dropped atomic.Int64
}

// NewFanoutHub creates a hub on topic. relay may be nil (single replica, tests): delivery is
// then local only. buf is the per-subscriber buffer (default 32).
func NewFanoutHub(relay *Broadcaster, topic string, buf int) (*FanoutHub, error) {
	if buf <= 0 {
		buf = 32
	}
	h := &FanoutHub{relay: relay, topic: topic, buf: buf, subs: map[string]map[*Sub]struct{}{}}
	if relay != nil {
		if err := relay.Subscribe(topic, func(m BroadcastMessage) { h.deliver(m.TenantID, m.Scope, m.Data) }); err != nil {
			return h, err
		}
	}
	return h, nil
}

// Subscribe registers a client for tenant with optional scopes it should also receive.
func (h *FanoutHub) Subscribe(tenant string, scopes ...string) *Sub {
	s := &Sub{Tenant: tenant, scopes: map[string]struct{}{}, C: make(chan []byte, h.buf)}
	for _, sc := range scopes {
		if sc != "" {
			s.scopes[sc] = struct{}{}
		}
	}
	h.mu.Lock()
	if h.subs[tenant] == nil {
		h.subs[tenant] = map[*Sub]struct{}{}
	}
	h.subs[tenant][s] = struct{}{}
	h.mu.Unlock()
	return s
}

// Unsubscribe removes the client and closes its channel. Safe to call once per Sub.
func (h *FanoutHub) Unsubscribe(s *Sub) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if set, ok := h.subs[s.Tenant]; ok {
		if _, ok := set[s]; ok {
			delete(set, s)
			close(s.C)
		}
		if len(set) == 0 {
			delete(h.subs, s.Tenant)
		}
	}
}

// Publish sends data to the tenant (scope "") or to one scope within it, on every replica.
func (h *FanoutHub) Publish(tenant, scope string, data []byte) {
	if h.relay == nil {
		h.deliver(tenant, scope, data)
		return
	}
	// The Broadcaster delivers to this replica's handler first, then relays to the others.
	_ = h.relay.Publish(h.topic, tenant, scope, data)
}

// Count reports this replica's subscribers for a tenant (diagnostics, connection caps).
func (h *FanoutHub) Count(tenant string) int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.subs[tenant])
}

func (h *FanoutHub) deliver(tenant, scope string, data []byte) {
	h.mu.RLock()
	defer h.mu.RUnlock()
	for s := range h.subs[tenant] {
		if scope != "" {
			if _, ok := s.scopes[scope]; !ok {
				continue
			}
		}
		select {
		case s.C <- data:
		default:
			s.Dropped.Add(1)
		}
	}
}
