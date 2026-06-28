package events

import (
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/nats-io/nats.go"
	"go.uber.org/zap"
)

// Defense-in-depth for the JetStream "consumer is already bound to a
// subscription" race. When a pod is replaced, the new pod can try to bind a
// push durable before the NATS server has released the previous pod's binding,
// get "already bound", and (in naive code) abandon the subscription — the
// consumer then silently stops until the next restart. Layer independent
// mitigations so no single failing layer reverts to that state:
//
//	Layer 1 (deploy):   maxSurge=0/maxUnavailable=1 — old pod terminates first.
//	Layer 2 (settle):   wait before the FIRST attempt so the server releases the
//	                    prior binding (NATS_REBIND_SETTLE_SECONDS, default 25s).
//	Layer 3 (retry):    retry on "already bound" with capped backoff over a long,
//	                    self-healing window.
//	Layer 4 (ops):      manual scale-cycle-with-buffer remains a human backstop.

// RebindSettle is the Layer-2 buffer before the first subscribe attempt.
// Defaults to 25s (matching the proven operational scale-cycle buffer); tune via
// NATS_REBIND_SETTLE_SECONDS (set 0 to disable).
func RebindSettle() time.Duration {
	if v := os.Getenv("NATS_REBIND_SETTLE_SECONDS"); v != "" {
		if n, err := strconv.Atoi(v); err == nil && n >= 0 {
			return time.Duration(n) * time.Second
		}
	}
	return 25 * time.Second
}

func isAlreadyBound(err error) bool {
	return err != nil && strings.Contains(err.Error(), "already bound")
}

// missingDeliverGroup reports the one-time migration case where a durable
// JetStream consumer already exists on the server WITHOUT a deliver group
// (created by the legacy SubscribeWithRebind path). nats.go refuses a queue
// subscription against such a consumer with this exact message; the fix is to
// delete the stale consumer so it is recreated WITH a deliver group on retry.
func missingDeliverGroup(err error) bool {
	return err != nil && strings.Contains(err.Error(), "without a deliver group")
}

// SubscribeWithRebind establishes a durable JetStream push subscription with the
// layered resilience above. It runs in the background so it never blocks startup;
// the subscription is retained by the NATS client for the life of the
// connection.
//
// Deprecated: SubscribeWithRebind binds a push durable WITHOUT a deliver group,
// so a JetStream consumer can bind exactly ONE subscription at a time. With >1
// replica the first pod binds and every other pod gets "consumer is already
// bound to a subscription" and retries forever — i.e. it is active-passive, not
// load-balanced, and a rolling deploy can leave the durable dormant. Use
// SubscribeQueueWithRebind instead: it adds a deliver group so all replicas share
// the consumer (load-balanced, no bind conflict) while keeping JetStream
// persistence and the same settle+retry resilience. Retained only for callers
// mid-migration.
func SubscribeWithRebind(log *zap.Logger, js nats.JetStreamContext, subject string, handler nats.MsgHandler, opts ...nats.SubOpt) {
	go func() {
		time.Sleep(RebindSettle()) // Layer 2
		backoff := 3 * time.Second
		const maxBackoff = 30 * time.Second
		for attempt := 1; attempt <= 40; attempt++ { // Layer 3 — long, self-healing window
			if _, err := js.Subscribe(subject, handler, opts...); err == nil {
				if log != nil {
					log.Info("jetstream subscription active", zap.String("subject", subject))
				}
				return
			} else if !isAlreadyBound(err) {
				if log != nil {
					log.Error("jetstream subscribe failed (non-retryable)",
						zap.String("subject", subject), zap.Error(err))
				}
				return
			}
			if log != nil {
				log.Warn("jetstream subscribe bind conflict; retrying",
					zap.String("subject", subject),
					zap.Int("attempt", attempt),
					zap.Duration("backoff", backoff))
			}
			time.Sleep(backoff)
			if backoff < maxBackoff {
				backoff *= 2
			}
		}
		if log != nil {
			log.Error("jetstream subscribe gave up after retries", zap.String("subject", subject))
		}
	}()
}

// QueueSubscribe registers a CORE-NATS queue subscriber and is the uniform
// replacement for `conn.Subscribe`. NATS delivers each message to exactly ONE
// member of the named queue group, so with >1 replica the handler runs once per
// event — not once per pod. Plain `conn.Subscribe` fans every message out to
// every pod and double-processes; always use this instead for core (non-stream)
// subjects.
//
// Use a DISTINCT queue name PER logical consumer. Two consumers that share a
// queue group AND have overlapping subject interest would steal each other's
// messages (e.g. a `pos.sale.finalized` handler and a `pos.>` handler must be in
// different groups so both still receive the sale event).
func QueueSubscribe(log *zap.Logger, conn *nats.Conn, subject, queue string, handler nats.MsgHandler) (*nats.Subscription, error) {
	sub, err := conn.QueueSubscribe(subject, queue, handler)
	if err == nil && log != nil {
		log.Info("core queue subscription active",
			zap.String("subject", subject), zap.String("queue", queue))
	}
	return sub, err
}

// SubscribeQueueWithRebind is the uniform, multi-replica-safe primitive for
// durable JetStream PUSH consumers — and the canonical replacement for the
// deprecated SubscribeWithRebind.
//
// It subscribes via js.QueueSubscribe so the durable consumer carries a DELIVER
// GROUP. NATS then load-balances the subject across every replica in the group
// (each message handled exactly once across the fleet) instead of rejecting the
// extra binders with "consumer is already bound to a subscription". This keeps
// full JetStream persistence/replay while eliminating the active-passive bind
// conflict that SubscribeWithRebind suffers at >1 replica.
//
// Like SubscribeWithRebind it runs in the background (never blocks startup) and
// layers the same resilience: Layer-2 settle (RebindSettle) before the first
// attempt + Layer-3 retry-on-already-bound with capped backoff.
//
// Arguments:
//   - stream  : the JetStream stream the durable lives on (used only for the
//     one-time self-healing migration below; pass "" to skip migration).
//   - subject : the filter subject to consume.
//   - queue   : the deliver-group name. Pass the SAME value as the durable name,
//     and callers MUST also pass nats.Durable(queue) in opts (that is what makes
//     the consumer durable; the queue is used purely as the deliver group when an
//     explicit Durable opt is given). Use a DISTINCT queue/durable per logical
//     consumer so consumers with overlapping subjects never steal each other's
//     messages.
//
// Self-healing migration: if a stale non-deliver-group durable already exists on
// the server (created by the old SubscribeWithRebind path), the first queue
// subscribe fails with "without a deliver group". We delete that consumer once
// and retry, so it is recreated WITH the deliver group automatically on deploy —
// no manual nats CLI step. Multiple replicas may race the delete; deleting an
// already-deleted consumer is harmless.
func SubscribeQueueWithRebind(log *zap.Logger, js nats.JetStreamContext, stream, subject, queue string, handler nats.MsgHandler, opts ...nats.SubOpt) {
	go func() {
		time.Sleep(RebindSettle()) // Layer 2
		backoff := 3 * time.Second
		const maxBackoff = 30 * time.Second
		deletedStale := false
		for attempt := 1; attempt <= 40; attempt++ { // Layer 3 — long, self-healing window
			if _, err := js.QueueSubscribe(subject, queue, handler, opts...); err == nil {
				if log != nil {
					log.Info("jetstream queue subscription active",
						zap.String("subject", subject), zap.String("queue", queue))
				}
				return
			} else if missingDeliverGroup(err) && !deletedStale && stream != "" {
				// One-time migration: drop the legacy non-queue durable so it is
				// recreated with a deliver group on the next attempt below.
				deletedStale = true
				if log != nil {
					log.Warn("jetstream durable lacks deliver group; deleting stale consumer to migrate to queue group",
						zap.String("subject", subject), zap.String("queue", queue))
				}
				if derr := js.DeleteConsumer(stream, queue); derr != nil && log != nil {
					log.Warn("jetstream delete stale consumer failed (will retry subscribe anyway)",
						zap.String("consumer", queue), zap.Error(derr))
				}
				continue // retry immediately
			} else if !isAlreadyBound(err) && !missingDeliverGroup(err) {
				if log != nil {
					log.Error("jetstream queue subscribe failed (non-retryable)",
						zap.String("subject", subject), zap.String("queue", queue), zap.Error(err))
				}
				return
			}
			if log != nil {
				log.Warn("jetstream queue subscribe bind conflict; retrying",
					zap.String("subject", subject), zap.String("queue", queue),
					zap.Int("attempt", attempt), zap.Duration("backoff", backoff))
			}
			time.Sleep(backoff)
			if backoff < maxBackoff {
				backoff *= 2
			}
		}
		if log != nil {
			log.Error("jetstream queue subscribe gave up after retries",
				zap.String("subject", subject), zap.String("queue", queue))
		}
	}()
}
