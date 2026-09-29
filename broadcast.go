package events

import (
	"strings"
	"sync"

	"github.com/google/uuid"
	"github.com/nats-io/nats.go"
	"go.uber.org/zap"
)

// Broadcaster fans a message out to every replica of a service, for WebSocket/SSE hubs and
// in-process cache invalidation.
//
// Why this exists: a hub keeps its connected clients in memory, so a client whose socket
// landed on pod B never sees a broadcast raised on pod A unless something relays it. Each
// hub used to carry its own Redis PSubscribe relay, and several of them re-delivered their
// own publishes locally (no origin check). Broadcaster is the one shared relay.
//
// Transport is core NATS with a plain (non-queue) subscription, so every pod receives every
// message, which is what fan-out needs. A queue group would hand each message to one pod
// only and is wrong here. Subjects live under the "_rt." prefix, outside every
// "{service}.>" JetStream stream, so nothing is persisted: delivery is best effort and a pod
// that is reconnecting misses messages. Clients must keep their existing resync path
// (version poll, refetch on reconnect) for that case.
//
// Publish delivers to this pod's own handlers directly and then relays to the other pods;
// each pod drops messages carrying its own origin ID, so no replica sees a message twice.
// With a nil connection (tests, local dev) the Broadcaster still works as a local bus.
type Broadcaster struct {
	conn      *nats.Conn
	log       *zap.Logger
	namespace string
	origin    string

	mu       sync.RWMutex
	handlers map[string][]BroadcastHandler
	subs     []*nats.Subscription
}

// BroadcastMessage is one fan-out message as seen by a handler.
type BroadcastMessage struct {
	Topic    string
	TenantID string // "" when the message is not tenant scoped
	Scope    string // optional sub-audience, e.g. "user:<id>" or "outlet:<id>"; "" = everyone
	Data     []byte
}

// BroadcastHandler receives messages for one topic. It runs on the NATS delivery goroutine
// (or the publisher's goroutine for local delivery) and must not block for long.
type BroadcastHandler func(BroadcastMessage)

const (
	broadcastPrefix    = "_rt"
	broadcastOriginHdr = "rt-origin"
	emptyToken         = "_"
)

// NewBroadcaster creates a Broadcaster. namespace groups topics, normally the owning service
// name ("pos", "logistics"); a consumer in another service uses the publisher's namespace.
func NewBroadcaster(log *zap.Logger, conn *nats.Conn, namespace string) *Broadcaster {
	if log == nil {
		log = zap.NewNop()
	}
	return &Broadcaster{
		conn:      conn,
		log:       log.Named("broadcast"),
		namespace: subjectToken(namespace),
		origin:    uuid.NewString(),
		handlers:  map[string][]BroadcastHandler{},
	}
}

// Subscribe registers handler for topic. The first handler for a topic opens the NATS
// subscription; later ones share it.
func (b *Broadcaster) Subscribe(topic string, handler BroadcastHandler) error {
	topic = subjectToken(topic)
	b.mu.Lock()
	defer b.mu.Unlock()
	first := len(b.handlers[topic]) == 0
	b.handlers[topic] = append(b.handlers[topic], handler)
	if !first || b.conn == nil {
		return nil
	}
	subject := strings.Join([]string{broadcastPrefix, b.namespace, topic, "*", "*"}, ".")
	sub, err := b.conn.Subscribe(subject, func(msg *nats.Msg) {
		if msg.Header.Get(broadcastOriginHdr) == b.origin {
			return // already delivered locally by Publish
		}
		tokens := strings.Split(msg.Subject, ".")
		if len(tokens) != 5 {
			return
		}
		b.dispatch(BroadcastMessage{
			Topic:    topic,
			TenantID: fromToken(tokens[3]),
			Scope:    fromToken(tokens[4]),
			Data:     msg.Data,
		})
	})
	if err != nil {
		b.handlers[topic] = b.handlers[topic][:len(b.handlers[topic])-1]
		return err
	}
	b.subs = append(b.subs, sub)
	b.log.Info("broadcast subscription active", zap.String("subject", subject))
	return nil
}

// Publish delivers msg to this pod's handlers and relays it to every other replica. A relay
// failure is logged and returned, but local delivery has already happened.
func (b *Broadcaster) Publish(topic, tenantID, scope string, data []byte) error {
	topic = subjectToken(topic)
	b.dispatch(BroadcastMessage{Topic: topic, TenantID: tenantID, Scope: scope, Data: data})
	if b.conn == nil {
		return nil
	}
	msg := nats.NewMsg(strings.Join([]string{
		broadcastPrefix, b.namespace, topic, toToken(tenantID), toToken(scope),
	}, "."))
	msg.Header.Set(broadcastOriginHdr, b.origin)
	msg.Data = data
	if err := b.conn.PublishMsg(msg); err != nil {
		b.log.Warn("broadcast relay failed", zap.String("topic", topic), zap.Error(err))
		return err
	}
	return nil
}

// Close drains the NATS subscriptions. Handlers stay registered for local delivery.
func (b *Broadcaster) Close() {
	b.mu.Lock()
	defer b.mu.Unlock()
	for _, s := range b.subs {
		_ = s.Unsubscribe()
	}
	b.subs = nil
}

func (b *Broadcaster) dispatch(m BroadcastMessage) {
	b.mu.RLock()
	hs := b.handlers[m.Topic]
	b.mu.RUnlock()
	for _, h := range hs {
		h(m)
	}
}

// subjectToken makes s safe as one NATS subject token (no dots, spaces or wildcards).
func subjectToken(s string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case '.', ' ', '\t', '\n', '\r', '*', '>':
			return '_'
		}
		return r
	}, s)
}

func toToken(s string) string {
	if s == "" {
		return emptyToken
	}
	return subjectToken(s)
}

func fromToken(s string) string {
	if s == emptyToken {
		return ""
	}
	return s
}
