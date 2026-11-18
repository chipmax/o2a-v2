package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"opencode2api/internal/config"
	"opencode2api/internal/httpx"
	"opencode2api/internal/identity"
	"opencode2api/internal/models"
	wire "opencode2api/internal/protocol"
	"opencode2api/internal/telemetry"
)

// kiloLane holds the state of the independent Kilo free-gateway lane. It is
// deliberately separate from the Zen/Go key pools: Kilo has no OpenCode
// credential, no WARP lease, no Bun TLS fingerprint and a per-model shared-pool
// quota rather than a per-key one.
type kiloLane struct {
	cfg    config.KiloConfig
	client *http.Client
	mu     sync.Mutex
	// cooled maps a model ID to the moment its shared provider pool becomes
	// usable again. A 429 cools the model, never the whole upstream.
	cooled map[string]time.Time
	// metadata holds the last live catalog fetch, keyed by model ID.
	metadata map[string]models.KiloModel
}

func newKiloLane(cfg config.KiloConfig) *kiloLane {
	return &kiloLane{
		cfg: cfg,
		client: &http.Client{
			Timeout: 60 * time.Second,
		},
		cooled:   make(map[string]time.Time),
		metadata: make(map[string]models.KiloModel),
	}
}

func (k *kiloLane) enabled() bool { return k != nil && k.cfg.Enabled() }

func (k *kiloLane) isCooled(model string, now time.Time) bool {
	if k == nil {
		return false
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	until, ok := k.cooled[model]
	if !ok || until.Before(now) {
		delete(k.cooled, model)
		return false
	}
	return true
}

func (k *kiloLane) cool(model string, retryAfter time.Duration, now time.Time) {
	if k == nil {
		return
	}
	delay := time.Duration(k.cfg.CooldownSeconds) * time.Second
	if retryAfter > 0 {
		delay = retryAfter
	}
	max := time.Duration(k.cfg.MaxCooldownSeconds) * time.Second
	if delay > max {
		delay = max
	}
	k.mu.Lock()
	k.cooled[model] = now.Add(delay)
	k.mu.Unlock()
}

// doKiloUpstream sends one request to the Kilo free gateway. It deliberately
// bypasses the OpenCode anonymous and key lanes: no WARP lease, no Bun TLS
// fingerprint, no x-opencode headers and no injected official prompt. Only the
// neutral OpenAI-compatible body and an optional configured key are sent.
func (g *Gateway) doKiloUpstream(ctx context.Context, route models.Route, bodies map[config.Tier][]byte, ids identity.RequestIDs, attemptOffset int) (*http.Response, int, error) {
	if !g.kilo.enabled() {
		return nil, 0, errors.New("Kilo lane is not configured")
	}
	raw := bodies[config.TierKilo]
	if len(raw) == 0 {
		return nil, 0, errors.New("no prepared Kilo request body")
	}
	now := time.Now()
	model := route.ID
	limit := max(1, g.cfg.Retry.MaxAttempts)
	var lastErr error
	var lastResp *http.Response
	used := 0
	for attempt := 0; attempt < limit; attempt++ {
		if ctx.Err() != nil {
			return nil, used, ctx.Err()
		}
		if g.kilo.isCooled(model, now) {
			sibling := g.kilo.sibling(model, now)
			if sibling == "" {
				return nil, used, errKiloQuotaSpent
			}
			var obj map[string]any
			if json.Unmarshal(raw, &obj) == nil {
				if _, ok := obj["model"]; ok {
					obj["model"] = sibling
					if rewritten, err := json.Marshal(obj); err == nil {
						raw = rewritten
					}
				}
			}
			model = sibling
			route.ID = sibling
		}
		endpoint := strings.TrimRight(g.kilo.cfg.BaseURL, "/") + "/chat/completions"
		req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(raw))
		if err != nil {
			return nil, used, err
		}
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Accept", "application/json")
		req.Header.Set("User-Agent", "o2a-v2")
		if g.kilo.cfg.Key != "" {
			req.Header.Set("Authorization", "Bearer "+g.kilo.cfg.Key)
		}
		resp, err := g.kilo.client.Do(req)
		used++
		if meta := telemetry.MetaFromContext(ctx); meta != nil {
			meta.Attempts = attemptOffset + used
			meta.Tier = string(config.TierKilo)
			meta.Protocol = wire.Chat
		}
		if err != nil {
			lastErr = err
			if meta := telemetry.MetaFromContext(ctx); meta != nil {
				meta.AttemptOutcome = "transport_error"
			}
			continue
		}
		lastResp = resp
		status := resp.StatusCode
		if status >= 200 && status < 300 {
			if meta := telemetry.MetaFromContext(ctx); meta != nil {
				meta.AttemptOutcome = "success"
			}
			g.recordKiloAttempt(ctx, route, ids, used, status, true, "")
			return resp, used, nil
		}
		if status == http.StatusTooManyRequests {
			var retryAfter time.Duration
			if value := resp.Header.Get("Retry-After"); value != "" {
				if seconds, err := strconv.Atoi(value); err == nil {
					retryAfter = time.Duration(seconds) * time.Second
				}
			}
			g.kilo.cool(model, retryAfter, time.Now())
			g.recordKiloAttempt(ctx, route, ids, used, status, false, "kilo_model_quota")
			httpx.DrainAndClose(resp.Body)
			lastResp = nil
			lastErr = errKiloQuotaSpent
			continue
		}
		if status == http.StatusBadRequest || status == http.StatusNotFound || status == http.StatusUnprocessableEntity {
			g.recordKiloAttempt(ctx, route, ids, used, status, false, "model_rejected")
			return resp, used, nil
		}
		g.recordKiloAttempt(ctx, route, ids, used, status, false, "retryable")
		lastErr = errors.New("Kilo gateway returned HTTP " + strconv.Itoa(status))
		httpx.DrainAndClose(resp.Body)
		lastResp = nil
	}
	return lastResp, used, lastErr
}

func (g *Gateway) recordKiloAttempt(ctx context.Context, route models.Route, ids identity.RequestIDs, attempt int, status int, success bool, outcome string) {
	if meta := telemetry.MetaFromContext(ctx); meta != nil {
		meta.AttemptOutcome = outcome
		leg := "kilo:" + strconv.Itoa(status) + ":0ms"
		meta.Legs = append(meta.Legs, leg)
	}
	if g.monitor == nil {
		return
	}
	g.monitor.RecordAttempt(telemetry.UpstreamAttempt{
		Time: time.Now().UTC(), RequestID: ids.Request, Model: route.ID, Tier: string(config.TierKilo), Attempt: attempt,
		KeyID: "kilo", Channel: "kilo", Anonymous: true, Proxy: "kilo", Status: status,
		Success: success, Outcome: outcome,
	})
	_ = io.Discard
}

// sibling returns another configured Kilo model that is not cooling. The
// caller uses it to retry a request after a per-model shared-pool 429.
func (k *kiloLane) sibling(model string, now time.Time) string {
	if k == nil {
		return ""
	}
	k.mu.Lock()
	defer k.mu.Unlock()
	for _, id := range k.cfg.Models {
		if id == model || k.cooled[id].After(now) {
			continue
		}
		return id
	}
	return ""
}

// refreshCatalog fetches the Kilo model catalog and installs the allowlist.
// A failure keeps the seed from config, matching the DSH market rule that a
// degraded lane must not erase its last known-good configuration.
func (k *kiloLane) refreshCatalog(ctx context.Context, catalog *models.ModelCatalog, logger *slog.Logger) {
	if !k.enabled() || catalog == nil {
		return
	}
	fetched, _, err := models.FetchKiloModels(ctx, k.client, k.cfg)
	if err != nil {
		if logger != nil {
			logger.Warn("Kilo catalog refresh failed; keeping configured models", "component", "models", "event", "kilo_catalog_failed", "error", err)
		}
		return
	}
	ids := make([]string, 0, len(fetched))
	meta := make(map[string]models.KiloModel, len(fetched))
	for _, item := range fetched {
		ids = append(ids, item.ID)
		meta[item.ID] = item
	}
	catalog.SetKiloModels(ids)
	catalog.SetKiloMetadata(fetched)
	k.mu.Lock()
	k.metadata = meta
	k.mu.Unlock()
	if logger != nil {
		logger.Info("Kilo catalog refreshed", "component", "models", "event", "kilo_catalog_refreshed", "models", len(ids))
	}
}
