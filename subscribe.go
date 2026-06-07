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

// SubscribeWithRebind establishes a durable JetStream push subscription with the
// layered resilience above. It runs in the background so it never blocks startup;
// the subscription is retained by the NATS client for the life of the
// connection. Use this in place of js.Subscribe(subject, handler, opts...) for
// push-durable consumers so a deploy/restart can never silently drop them.
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
