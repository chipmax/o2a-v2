package gateway

import (
	"context"
	"net/http"
	"time"

	wire "opencode2api/internal/protocol"
)

// gatewayBusyMessage is what an overflow request receives once the queue wait
// is gone. It names the limit instead of pretending the upstream failed.
const gatewayBusyMessage = "the gateway is at its concurrent-request limit. Retry shortly."

// gated bounds concurrent inference turns gateway-wide. The first overflow
// waits up to queue_wait_seconds for a slot, then answers 429 gateway_busy with
// a short Retry-After; a nil gate leaves the handler untouched. Wrapping every
// POST endpoint (not just the chat one) is the point: a burst on the
// Anthropic or Responses route must count against the same budget.
func (g *Gateway) gated(protocol wire.Protocol, next http.HandlerFunc) http.HandlerFunc {
	if g.inflight == nil {
		return next
	}
	return func(w http.ResponseWriter, r *http.Request) {
		started := time.Now()
		if !g.acquireSlot(r.Context()) {
			if r.Context().Err() == nil {
				w.Header().Set("Retry-After", "5")
				wire.WriteError(w, protocol, http.StatusTooManyRequests, gatewayBusyMessage, "gateway_busy", "")
			}
			return
		}
		if waited := time.Since(started); waited > 50*time.Millisecond {
			g.queuedTotal.Add(1)
			g.logger.Info("request waited for an inflight slot", "component", "gateway", "event", "inflight_queued", "waited_ms", waited.Milliseconds())
		}
		defer g.releaseSlot()
		next(w, r)
	}
}

// acquireSlot takes one inflight slot, waiting at most queueWait. It reports
// false when the request should be refused (or was canceled while waiting).
func (g *Gateway) acquireSlot(ctx context.Context) bool {
	select {
	case g.inflight <- struct{}{}:
		g.inflightNow.Add(1)
		g.inflightTotal.Add(1)
		return true
	default:
	}
	if g.queueWait <= 0 {
		return false
	}
	timer := time.NewTimer(g.queueWait)
	defer timer.Stop()
	select {
	case g.inflight <- struct{}{}:
		g.inflightNow.Add(1)
		g.inflightTotal.Add(1)
		return true
	case <-timer.C:
		return false
	case <-ctx.Done():
		return false
	}
}

func (g *Gateway) releaseSlot() {
	<-g.inflight
	g.inflightNow.Add(-1)
}
