package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"opencode2api/internal/config"
	"opencode2api/internal/identity"
)

// quotaProbeTimeout bounds one quota probe attempt. A minimal prompt is
// answered in seconds when the lane is open, so a slow answer is itself a
// reason to leave the lane cooling.
const quotaProbeTimeout = 90 * time.Second

// quotaProbeTarget is one cooling credential or relay the quota probe may test.
type quotaProbeTarget struct {
	id        string
	tier      config.Tier
	key       string
	node      *upstreamNode
	anonymous *anonymousNode
	proxy     *proxyTransport
	until     time.Time
}

// StartQuotaProbe launches the loop that re-tests cooling upstream credentials
// and static relays with one minimal request per interval. A quota window can
// reopen before the Retry-After it announced (or a hint can overshoot), and
// until this probe existed the lane only noticed when a real request happened
// to arrive. The loop is a no-op when models.quota_probe_seconds is 0 or no
// server key exists to authenticate the probe.
func (g *Gateway) StartQuotaProbe(ctx context.Context) {
	if g == nil {
		return
	}
	interval := time.Duration(g.cfg.Models.QuotaProbeSeconds) * time.Second
	if interval <= 0 || g.cfg.FirstEnabledServerKey() == "" {
		return
	}
	g.logger.Info("quota probe loop started", "component", "gateway", "event", "quota_probe_started", "interval_s", int(interval.Seconds()))
	go func() {
		timer := time.NewTimer(interval)
		defer timer.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-timer.C:
			}
			if ctx.Err() == nil {
				g.probeCoolingQuota(ctx)
			}
			timer.Reset(interval)
		}
	}()
}

// probeCoolingQuota tests at most one cooling target. Probing everything on
// every tick would spend more free-tier quota than the traffic it protects, so
// targets rotate by "soonest to reopen" and each one is retried no more often
// than the configured interval.
func (g *Gateway) probeCoolingQuota(ctx context.Context) {
	now := time.Now()
	g.logger.Debug("quota probe tick", "component", "gateway", "event", "quota_probe_tick",
		"zen_cooling", coolingNodes(g.zenNodes, now), "go_cooling", coolingNodes(g.goNodes, now))
	target, ok := g.nextQuotaProbeTarget(now)
	g.logger.Debug("quota probe target", "component", "gateway", "event", "quota_probe_target",
		"found", ok, "id", target.id, "until", target.until)
	if !ok {
		return
	}
	model := g.quotaProbeModel(target)
	if model == "" {
		return
	}
	status, retryAfter, err := g.sendQuotaProbe(ctx, target, model)
	g.noteQuotaProbe(target.id, time.Now())
	outcome := "still_cooling"
	switch {
	case err != nil:
		outcome = "error"
	case status >= 200 && status < 300:
		g.reopenQuotaTarget(target)
		if target.anonymous != nil {
			g.anonymous.clearWall(model)
		}
		outcome = "reopened"
	case status == http.StatusTooManyRequests:
		g.extendQuotaTarget(target, retryAfter)
	default:
		outcome = "unchanged"
	}
	fields := []any{
		"component", "gateway", "event", "quota_probe",
		"target", target.id, "tier", string(target.tier), "model", model,
		"status", status, "outcome", outcome,
	}
	if err != nil {
		fields = append(fields, "error", g.redactorForLog(err))
	}
	g.logger.Info("quota probe", fields...)
}

// nextQuotaProbeTarget picks the cooling target whose window reopens soonest
// and that has not been probed within the interval. With a WARP pool the
// anonymous exits are not candidates: a cooling WARP exit has already released
// its tunnel and a fresh registration would land on a different /64, so only
// key tiers and static relays can be re-tested.
func (g *Gateway) nextQuotaProbeTarget(now time.Time) (quotaProbeTarget, bool) {
	interval := time.Duration(g.cfg.Models.QuotaProbeSeconds) * time.Second
	var best quotaProbeTarget
	found := false
	consider := func(target quotaProbeTarget) {
		if !target.until.After(now) || g.probedRecently(target.id, now, interval) {
			return
		}
		if !found || target.until.Before(best.until) {
			best, found = target, true
		}
	}
	considerNodes := func(nodes *upstreamNodes, tier config.Tier) {
		if nodes == nil {
			return
		}
		for _, node := range nodes.nodes {
			if until := node.cooldownUntil.Load(); until > 0 {
				consider(quotaProbeTarget{
					id: config.Fingerprint(node.key), tier: tier, key: node.key, node: node,
					until: time.Unix(0, until),
				})
			}
		}
	}
	considerNodes(g.zenNodes, config.TierZen)
	considerNodes(g.goNodes, config.TierGo)
	if g.warp == nil && g.cfg.Anonymous && g.anonymous != nil {
		for _, node := range g.anonymous.nodes {
			if node.proxy == nil || !node.quotaCooldown.Load() {
				continue
			}
			until := node.cooldownUntil.Load()
			if until <= 0 {
				continue
			}
			consider(quotaProbeTarget{
				id: "relay:" + config.RedactURL(node.proxy.name), tier: config.TierZen,
				anonymous: node, proxy: node.proxy, until: time.Unix(0, until),
			})
		}
	}
	return best, found
}

// quotaProbeModel picks a model that routes on the target's lane. Key tiers
// prefer their own route; a static relay needs a free model so the anonymous
// lane is the one being measured.
func (g *Gateway) quotaProbeModel(target quotaProbeTarget) string {
	firstErr := ""
	for _, model := range g.catalog.List() {
		if target.anonymous != nil {
			if !g.catalog.IsFreeModel(model) {
				continue
			}
			if _, err := g.catalog.Route(model, len(g.cfg.ZenKeys) > 0, len(g.cfg.GoKeys) > 0, g.cfg.Anonymous); err == nil {
				return model
			} else if firstErr == "" {
				firstErr = model + ": " + err.Error()
			}
			continue
		}
		if _, err := g.catalog.RouteForTier(model, target.tier, len(g.cfg.ZenKeys) > 0, len(g.cfg.GoKeys) > 0); err == nil {
			return model
		} else if firstErr == "" {
			firstErr = model + ": " + err.Error()
		}
	}
	if firstErr == "" {
		firstErr = "the catalog lists no models yet"
	}
	g.logger.Warn("no model routes on the cooling lane", "component", "gateway", "event", "quota_probe_no_model", "tier", string(target.tier), "first_error", g.redactorForLog(errors.New(firstErr)))
	return ""
}

// sendQuotaProbe pins one minimal request to the target and reports the status
// and Retry-After. The request is a diagnostic, so it changes no cooldown or
// proxy health by itself; probeCoolingQuota applies the outcome explicitly.
func (g *Gateway) sendQuotaProbe(ctx context.Context, target quotaProbeTarget, model string) (int, time.Duration, error) {
	payload, err := json.Marshal(map[string]any{
		"model":      model,
		"messages":   []map[string]any{{"role": "user", "content": "ping"}},
		"max_tokens": 1,
		"stream":     false,
	})
	if err != nil {
		return 0, 0, err
	}
	requestCtx, cancel := context.WithTimeout(ctx, quotaProbeTimeout)
	defer cancel()
	if target.anonymous != nil {
		requestCtx = WithRelayOverride(requestCtx, RelaySelection{Name: target.proxy.name})
	} else {
		requestCtx = WithDebugKeyOverride(requestCtx, DebugKeyOverride{Tier: target.tier, KeyID: config.Fingerprint(target.key)})
	}
	request, err := http.NewRequestWithContext(requestCtx, http.MethodPost, "http://gateway.local/v1/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return 0, 0, err
	}
	request.Header.Set("Authorization", "Bearer "+g.cfg.FirstEnabledServerKey())
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("x-opencode-session", identity.CanonicalSessionID(identity.RandomID("quota-probe", 16)))
	recorder := &probeCapture{header: make(http.Header), status: http.StatusOK}
	g.Handler().ServeHTTP(recorder, request)
	return recorder.status, parseRetryAfter(recorder.header.Get("Retry-After")), nil
}

// reopenQuotaTarget ends the cooldown after a probe answered successfully.
func (g *Gateway) reopenQuotaTarget(target quotaProbeTarget) {
	switch {
	case target.anonymous != nil:
		g.anonymous.MarkSuccess(target.anonymous)
	case target.tier == config.TierGo && g.goNodes != nil:
		g.goNodes.MarkSuccess(target.node)
	case g.zenNodes != nil:
		g.zenNodes.MarkSuccess(target.node)
	}
}

// extendQuotaTarget keeps the target cooling until the new hint, bounded to one
// day: the free tier's counting window is the UTC calendar day.
func (g *Gateway) extendQuotaTarget(target quotaProbeTarget, retryAfter time.Duration) {
	delay := retryAfter
	if delay <= 0 {
		delay = quotaResetAfter(time.Now())
	}
	delay = min(delay, maxAnonymousQuotaCooldown)
	target.until = time.Now().Add(delay)
	if target.anonymous != nil {
		target.anonymous.cooldownUntil.Store(target.until.UnixNano())
		target.anonymous.quotaCooldown.Store(true)
		return
	}
	target.node.cooldownUntil.Store(target.until.UnixNano())
}

// coolingNodes counts the key nodes whose cooldown is still running.
func coolingNodes(nodes *upstreamNodes, now time.Time) int {
	if nodes == nil {
		return 0
	}
	count := 0
	for _, node := range nodes.nodes {
		if until := node.cooldownUntil.Load(); until > 0 && until > now.UnixNano() {
			count++
		}
	}
	return count
}

func (g *Gateway) probedRecently(id string, now time.Time, interval time.Duration) bool {	g.probeMu.Lock()
	defer g.probeMu.Unlock()
	last, ok := g.probedAt[id]
	return ok && now.Sub(last) < interval
}

func (g *Gateway) noteQuotaProbe(id string, now time.Time) {
	g.probeMu.Lock()
	defer g.probeMu.Unlock()
	if g.probedAt == nil {
		g.probedAt = make(map[string]time.Time)
	}
	g.probedAt[id] = now
}

// redactorForLog keeps upstream URLs and credentials out of probe logs.
func (g *Gateway) redactorForLog(err error) string {
	if err == nil {
		return ""
	}
	redactor := config.SecretRedactor()
	redactor.Replace(g.cfg)
	return redactor.String(err.Error())
}
