package gateway

import (
	"context"
	"errors"
	"io"
	"net/http"
	"sync"
	"time"

	"opencode2api/internal/config"
	"opencode2api/internal/httpx"
	"opencode2api/internal/identity"
	"opencode2api/internal/models"
)

const (
	warpFallbackDefaultCooldown = 60 * time.Second
	warpFallbackMaxCooldown     = time.Hour
)

// doAnonymousUpstream tries WARP first when it is eligible. If WARP cannot
// serve the request for a retryable reason and static proxies are configured,
// it parks WARP for a short cooldown and runs the proxy walk in the same
// request. Deterministic request rejections are returned without a fallback.
func (g *Gateway) doAnonymousUpstream(ctx context.Context, route models.Route, bodies map[config.Tier][]byte, ids identity.RequestIDs, attemptOffset int) (*http.Response, error, int) {
	if !g.usesWARP(route.ID, config.TierZen, true) {
		return g.doStaticAnonymousUpstream(ctx, route, bodies, ids, attemptOffset)
	}
	if !g.warpFallbackEnabled() {
		return g.doWarpAnonymousUpstream(ctx, route, bodies, ids, attemptOffset)
	}

	warpCtx := ctx
	var cancel context.CancelFunc
	if budget := g.warpFallbackBudget(); budget > 0 {
		warpCtx, cancel = context.WithTimeout(ctx, budget)
	}
	resp, err, used := g.doWarpAnonymousUpstream(warpCtx, route, bodies, ids, attemptOffset)
	if cancel != nil {
		if resp != nil && resp.Body != nil {
			resp.Body = &cancelOnClose{ReadCloser: resp.Body, cancel: cancel}
		} else {
			cancel()
		}
	}
	if !g.warpFallbackEligible(ctx, resp, err) {
		return resp, err, used
	}

	retryAfter := time.Duration(0)
	if resp != nil {
		retryAfter = parseRetryAfter(resp.Header.Get("Retry-After"))
	}
	reason := warpFallbackReason(resp, err)
	if resp != nil {
		httpx.DrainAndClose(resp.Body)
		resp = nil
	}
	cooldown := g.warpFallbackDelay(err, retryAfter)
	g.pauseWARPFor(cooldown)
	g.logger.Warn("WARP exhausted or limited; falling back to static proxies", "component", "upstream", "event", "warp_fallback_to_proxies", "request_id", ids.Request, "model", route.ID, "attempts", used, "cooldown_seconds", int(cooldown.Seconds()), "reason", reason)
	return g.doStaticAnonymousUpstream(ctx, route, bodies, ids, attemptOffset+used)
}

func (g *Gateway) warpFallbackEnabled() bool {
	return g.cfg.Warp.FallbackToProxies && g.anonymous != nil && g.anonymous.Len() > 0
}

func (g *Gateway) warpFallbackBudget() time.Duration {
	seconds := g.cfg.Warp.FallbackBudgetSeconds
	if seconds <= 0 {
		return 0
	}
	return time.Duration(seconds) * time.Second
}

func (g *Gateway) warpFallbackEligible(ctx context.Context, resp *http.Response, err error) bool {
	if ctx.Err() != nil {
		return false
	}
	if err == nil {
		if resp == nil {
			return true
		}
		return resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode/100 == 5
	}
	if errors.Is(err, errDeterministicClientRejection) {
		return false
	}
	return true
}

func (g *Gateway) warpFallbackDelay(err error, retryAfter time.Duration) time.Duration {
	delay := warpFallbackDefaultCooldown
	if errors.Is(err, errFreeTierQuotaSpent) {
		delay = time.Duration(g.cfg.Warp.PauseAfterQuotaSeconds) * time.Second
		if delay <= 0 {
			delay = 5 * time.Minute
		}
	} else if g.cfg.Warp.FallbackCooldownSeconds > 0 {
		delay = time.Duration(g.cfg.Warp.FallbackCooldownSeconds) * time.Second
	}
	if retryAfter > delay {
		delay = retryAfter
	}
	if delay > warpFallbackMaxCooldown {
		delay = warpFallbackMaxCooldown
	}
	return delay
}

func (g *Gateway) pauseWARPFor(delay time.Duration) {
	if g.warp == nil || delay <= 0 {
		return
	}
	until := time.Now().Add(delay).UnixNano()
	for {
		current := g.warpPausedUntil.Load()
		if until <= current || g.warpPausedUntil.CompareAndSwap(current, until) {
			return
		}
	}
}

func warpFallbackReason(resp *http.Response, err error) string {
	switch {
	case errors.Is(err, errFreeTierQuotaSpent):
		return "quota"
	case errors.Is(err, errFreeTierIPBlocked):
		return "ip_blocked"
	case errors.Is(err, errAnonymousLanePaused):
		return "lane_paused"
	case errors.Is(err, errWARPUnavailable):
		return "warp_unavailable"
	case resp != nil && resp.StatusCode == http.StatusTooManyRequests:
		return "rate_limited"
	case resp != nil && resp.StatusCode/100 == 5:
		return "upstream_error"
	default:
		return "transport"
	}
}

type cancelOnClose struct {
	io.ReadCloser
	cancel context.CancelFunc
	once   sync.Once
}

func (c *cancelOnClose) Close() error {
	err := c.ReadCloser.Close()
	c.once.Do(c.cancel)
	return err
}
