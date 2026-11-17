package gateway

import (
	"net/http"
	"net/netip"
	"slices"
	"strconv"
	"time"

	"opencode2api/internal/buildinfo"
	"opencode2api/internal/config"
	"opencode2api/internal/httpx"
	modelcatalog "opencode2api/internal/models"
	wire "opencode2api/internal/protocol"
)

type healthResponse struct {
	Status  string           `json:"status"`
	Ready   bool             `json:"ready"`
	Version string           `json:"version"`
	Models  healthModels     `json:"models"`
	Keys    healthKeys       `json:"keys"`
	Proxies healthProxies    `json:"proxies"`
	Warp    *healthWarp      `json:"warp,omitempty"`
	Peers   []map[string]any `json:"peers,omitempty"`
	Load    *healthLoad      `json:"load,omitempty"`
	Issues  []string         `json:"issues,omitempty"`
}

type healthModels struct {
	Status            string     `json:"status"`
	Total             int        `json:"total"`
	Exposed           int        `json:"exposed"`
	Zen               int        `json:"zen"`
	Go                int        `json:"go"`
	LastRefresh       *time.Time `json:"last_refresh,omitempty"`
	StaleAfterSeconds int        `json:"stale_after_seconds"`
	CacheSource       string     `json:"cache_source,omitempty"`
	Stale             bool       `json:"stale"`
}

type healthKeys struct {
	Zen         int  `json:"zen"`
	Go          int  `json:"go"`
	Total       int  `json:"total"`
	Anonymous   bool `json:"anonymous"`
	Quarantined int  `json:"credential_quarantined,omitempty"`
}

// healthLoad reports the admission gate. It is omitted when max_inflight is 0,
// so a deployment that never enabled the gate keeps the same document.
type healthLoad struct {
	Inflight int    `json:"inflight"`
	Max      int    `json:"max"`
	Total    uint64 `json:"total"`
	Queued   uint64 `json:"queued_total"`
}

type healthProxies struct {
	Total     int `json:"total"`
	Healthy   int `json:"healthy"`
	Unhealthy int `json:"unhealthy"`
}

// healthWarp reports the state of the userspace WARP pool. It is omitted when
// the switch is off so existing readiness consumers see the same document.
type healthWarp struct {
	Status  string `json:"status"`
	Stack   string `json:"stack"`
	Size    int    `json:"size"`
	Preload int    `json:"preload"`
	Active  int    `json:"active"`
	Standby int    `json:"standby"`
	// EgressIPv6 counts active exits on a public IPv6 address. Those are the
	// exits with their own free-tier /64 bucket; an IPv4 WARP egress is a
	// Cloudflare address shared with every other user and is not a bucket.
	EgressIPv6 int `json:"egress_ipv6"`
	Cooling    int `json:"cooling"`
	// CoolingPrefixes is the number of egress prefixes parked by the free lane.
	// It is the honest denominator for "cooling": the count is per /64, so it
	// can exceed Size while replacements keep landing on fresh blocks.
	CoolingPrefixes int `json:"cooling_prefixes,omitempty"`
	// LaneGated reports that the free lane refused several fresh exits in a row
	// and the pool paused registrations until LaneUntil. Without it the pool
	// looks merely "small" while it is really waiting out an upstream refusal.
	LaneGated    bool       `json:"lane_gated,omitempty"`
	LaneUntil    *time.Time `json:"lane_until,omitempty"`
	LaneRefusals int        `json:"lane_refusals,omitempty"`
}

// handleHealth reports readiness. It answers 503 while the model catalog is
// pending or empty, while WARP is enabled but no exit is active, or while no
// proxy is healthy when free traffic cannot leave through WARP (WARP off, or Go
// keys configured). Any issue, including a stale catalog or no upstream
// credentials, marks the status degraded, except that a pending catalog reports
// starting.
func (g *Gateway) handleHealth(w http.ResponseWriter, _ *http.Request) {
	report, httpStatus := g.HealthReport()
	httpx.WriteJSON(w, httpStatus, report)
}

// HealthReport builds the readiness document /healthz serves. The console reads
// it too, so its "Ready" chip cannot disagree with the endpoint clients use.
func (g *Gateway) HealthReport() (healthResponse, int) {
	_, models := g.availableModels()
	proxyTotal, proxyHealthy := g.transports.healthCounts()
	zenKeys, goKeys := g.zenNodes.Len(), g.goNodes.Len()
	staleAfter := max(2*time.Duration(g.cfg.Models.RefreshSeconds)*time.Second, time.Minute)
	var warpStatus *healthWarp
	warpActive := 0
	if g.warp != nil {
		snapshot := g.warp.Snapshot()
		warpActive = snapshot.Active
		egressIPv6 := 0
		for _, raw := range snapshot.Egress {
			if address, err := netip.ParseAddr(raw); err == nil && address.Is6() {
				egressIPv6++
			}
		}
		warpStatus = &healthWarp{
			Status:          "unavailable",
			Stack:           snapshot.Stack,
			Size:            snapshot.Size,
			Preload:         snapshot.Preload,
			Active:          snapshot.Active,
			Standby:         snapshot.Standby,
			EgressIPv6:      egressIPv6,
			Cooling:         snapshot.Cooling,
			CoolingPrefixes: snapshot.CoolingPrefixes,
		}
		if snapshot.LaneRefusals > 0 || snapshot.LaneGated {
			warpStatus.LaneRefusals = snapshot.LaneRefusals
			warpStatus.LaneGated = snapshot.LaneGated
			if !snapshot.LaneUntil.IsZero() {
				until := snapshot.LaneUntil.UTC()
				warpStatus.LaneUntil = &until
			}
		}
		if snapshot.Active > 0 {
			warpStatus.Status = "ready"
		}
	}

	modelStatus := "ready"
	var lastRefresh *time.Time
	issues := make([]string, 0, 3)
	if models.UpdatedAt.IsZero() {
		modelStatus = "pending"
		issues = append(issues, "model_catalog_pending")
	} else {
		updatedAt := models.UpdatedAt.UTC()
		lastRefresh = &updatedAt
		if models.Exposed == 0 {
			modelStatus = "empty"
			issues = append(issues, "model_catalog_empty")
		} else if models.Stale || time.Since(models.UpdatedAt) > staleAfter {
			modelStatus = "stale"
			issues = append(issues, "model_catalog_stale")
		}
	}
	if zenKeys+goKeys == 0 && !g.cfg.Anonymous {
		issues = append(issues, "no_upstream_keys")
	}
	warpBlocking := g.warp != nil && warpActive == 0
	if warpBlocking {
		issues = append(issues, "warp_unavailable")
	}
	// Proxies carry Go-tier traffic whenever Go keys exist, and every lane
	// while WARP is off; free-model traffic alone does not need them.
	proxyBlocking := proxyHealthy == 0 && (g.warp == nil || goKeys > 0)
	if proxyHealthy == 0 {
		issues = append(issues, "no_healthy_proxies")
	}

	status := "ok"
	if len(issues) > 0 {
		status = "degraded"
	}
	blocking := modelStatus == "pending" || modelStatus == "empty" || warpBlocking || proxyBlocking
	httpStatus := http.StatusOK
	ready := !blocking
	if blocking {
		httpStatus = http.StatusServiceUnavailable
		if modelStatus == "pending" {
			status = "starting"
		}
	}
	quarantined := 0
	now := time.Now()
	_, zenQuarantined, _, _ := g.zenNodes.CoolingSummary(now)
	_, goQuarantined, _, _ := g.goNodes.CoolingSummary(now)
	quarantined = zenQuarantined + goQuarantined
	var load *healthLoad
	if g.inflight != nil {
		load = &healthLoad{
			Inflight: int(g.inflightNow.Load()),
			Max:      cap(g.inflight),
			Total:    g.inflightTotal.Load(),
			Queued:   g.queuedTotal.Load(),
		}
	}
	return healthResponse{
		Status:  status,
		Ready:   ready,
		Version: buildinfo.Version,
		Models: healthModels{
			Status:            modelStatus,
			Total:             models.Total,
			Exposed:           models.Exposed,
			Zen:               models.Zen,
			Go:                models.Go,
			LastRefresh:       lastRefresh,
			StaleAfterSeconds: int(staleAfter / time.Second),
			CacheSource:       models.CacheSource,
			Stale:             models.Stale,
		},
		Keys: healthKeys{Zen: zenKeys, Go: goKeys, Total: zenKeys + goKeys, Anonymous: g.cfg.Anonymous, Quarantined: quarantined},
		Proxies: healthProxies{
			Total:     proxyTotal,
			Healthy:   proxyHealthy,
			Unhealthy: proxyTotal - proxyHealthy,
		},
		Warp:   warpStatus,
		Peers:  g.peers.peerSnapshot(),
		Load:   load,
		Issues: issues,
	}, httpStatus
}

func (g *Gateway) availableModels() ([]modelcatalog.Route, modelcatalog.CatalogSnapshot) {
	return g.catalog.AvailableModels(g.zenNodes.Len() > 0, g.goNodes.Len() > 0, g.cfg.Anonymous)
}

// reasoningLevels are the effort values the gateway accepts and forwards, from
// lowest to highest. "auto" and "none" are gateway controls, not levels.
var reasoningLevels = []string{"minimal", "low", "medium", "high", "xhigh", "max"}

// reasoningEfforts lists the levels a reasoning model can be asked for. When an
// "auto" probe has found the highest level the upstream accepts, the list stops
// there so clients do not offer levels that would be rejected.
func (g *Gateway) reasoningEfforts(model string, route modelcatalog.Route) []string {
	if g.cfg.ForcedEffort(model) == "auto" {
		if ceiling := slices.Index(reasoningLevels, g.resolvedEffort(model, route.Tier, route.Protocol)); ceiling >= 0 {
			return slices.Clone(reasoningLevels[:ceiling+1])
		}
	}
	return slices.Clone(reasoningLevels)
}

// modelLimits applies models.context_overrides and live context observations
// to the metadata a discovery client sees. The free tier enforces looser
// limits than its catalog states, so the listing follows what this deployment
// has measured instead of a client-side hard-code. The override is the initial
// seed: traffic can raise it after a larger success and lower it after a
// context-limit rejection.
func (g *Gateway) modelLimits(model string, tier config.Tier, md modelcatalog.Metadata) modelcatalog.Metadata {
	limit, ok := g.cfg.Models.ContextOverrides[model]
	seed := 0
	if ok {
		if limit.Context > 0 {
			seed = limit.Context
			md.ContextWindow = limit.Context
		}
		if limit.Input > 0 {
			md.MaxInput = limit.Input
		}
		if limit.Output > 0 {
			md.MaxOutput = limit.Output
		}
	}
	if g.contextLimits != nil {
		md.ContextWindow = g.contextLimits.Advertised(model, tier, md.ContextWindow, seed)
	}
	return md
}

// contextLimit is the window currently advertised for model on tier. It is
// used to annotate context-limit errors and to tell the observation board what
// the client was working with when its request failed.
func (g *Gateway) contextLimit(model string, tier config.Tier) int {
	md := g.catalog.MetadataForTier(model, tier)
	return g.modelLimits(model, tier, md).ContextWindow
}

// observeContextRejection records a context-length rejection and reports the
// resulting advertised window and whether the observation changed it. It does
// not touch the response, so a retry loop can learn without leaking the header
// onto a later successful attempt.
func (g *Gateway) observeContextRejection(model string, tier config.Tier, status int, body []byte, requestBytes int64) (int, bool) {
	if tier == "" || g.contextLimits == nil || g.catalog == nil || !isContextLengthResponse(status, body) {
		return 0, false
	}
	advertised := g.contextLimit(model, tier)
	_, applied := g.contextLimits.ObserveRejection(model, tier, requestBytes, body, advertised)
	if applied {
		advertised = g.contextLimit(model, tier)
	}
	return advertised, applied
}

// noteContextRejection records a context-length rejection and writes the
// resulting X-Context-Limit header. It reports the advertised window and
// whether the observation changed the advertisement. A caller must still
// forward the rejection; this helper only learns from it.
func (g *Gateway) noteContextRejection(w http.ResponseWriter, model string, tier config.Tier, status int, body []byte, requestBytes int64) (int, bool) {
	advertised, applied := g.observeContextRejection(model, tier, status, body, requestBytes)
	if advertised > 0 {
		w.Header().Set("X-Context-Limit", strconv.Itoa(advertised))
	}
	return advertised, applied
}

// handleModels lists every servable model with its tier-scoped metadata in
// the OpenAI list shape, plus, when Go keys are configured, one entry per
// rotation group alias that has a Go-routable model.
func (g *Gateway) handleModels(w http.ResponseWriter, _ *http.Request) {
	now := time.Now().Unix()
	routes, _ := g.availableModels()
	data := make([]map[string]any, 0, len(routes))
	for _, route := range routes {
		model := route.ID
		// Tier-scoped metadata: the advertised context window must match
		// the tier that will serve the request (anonymous ⇒ Zen).
		md := g.catalog.MetadataForTier(model, route.Tier)
		md = g.modelLimits(model, route.Tier, md)
		entry := map[string]any{
			"id": model, "object": "model", "created": now, "owned_by": "opencode",
			"metadata":        md,
			"native_protocol": route.Protocol,
		}
		if route.Protocol == wire.SystemOne {
			entry["supported_endpoints"] = []string{"/v1/systemone"}
		} else {
			entry["supported_endpoints"] = []string{"/v1/chat/completions", "/v1/responses", "/v1/messages"}
		}
		// Top-level OpenAI-standard fields: discovery clients (jcode, Pi, …)
		// read context/reasoning at the top level of each model entry.
		if md.ContextWindow > 0 {
			entry["context_window"] = md.ContextWindow
			entry["context_length"] = md.ContextWindow
		}
		if md.MaxInput > 0 {
			entry["max_input"] = md.MaxInput
		}
		if md.MaxOutput > 0 {
			entry["max_output"] = md.MaxOutput
			entry["max_output_tokens"] = md.MaxOutput
		}
		if len(md.InputModalities) > 0 {
			entry["input_modalities"] = md.InputModalities
		}
		if len(md.OutputModalities) > 0 {
			entry["output_modalities"] = md.OutputModalities
		}
		// Where the advertised window came from (catalog / learned / config), so
		// an agent or an operator script can tell measured values from defaults.
		source := "catalog"
		if g.contextLimits != nil {
			obs := g.contextLimits.Observation(model, route.Tier)
			if obs.Observed > 0 || obs.RejectedAt > 0 || obs.Ceiling > 0 {
				source = "learned"
			}
		}
		if limit, ok := g.cfg.Models.ContextOverrides[model]; ok && limit.Context > 0 {
			source = "config"
		}
		entry["capability_source"] = source
		if md.Reasoning {
			entry["reasoning"] = true
			entry["supports_reasoning"] = true
			efforts := g.reasoningEfforts(model, route)
			entry["reasoning_efforts"] = efforts
			// OpenClaw and similar harnesses read effort levels from compat
			// rather than inferring them, and otherwise offer only off/max.
			entry["compat"] = map[string]any{"supportsReasoningEffort": true, "supportedReasoningEfforts": efforts}
			if effort := g.resolvedEffort(model, route.Tier, route.Protocol); effort != "" && effort != "none" {
				entry["default_reasoning_effort"] = effort
			}
		}
		if md.Cost != nil {
			cost := map[string]any{}
			// Camel-case aliases match OpenClaw's model cost shape.
			for _, field := range []struct {
				names []string
				value *float64
			}{
				{[]string{"input"}, md.Cost.Input},
				{[]string{"output"}, md.Cost.Output},
				{[]string{"cache_read", "cacheRead"}, md.Cost.CacheRead},
				{[]string{"cache_write", "cacheWrite"}, md.Cost.CacheWrite},
			} {
				if field.value == nil {
					continue
				}
				for _, name := range field.names {
					cost[name] = *field.value
				}
			}
			entry["cost"] = cost
		}
		if md.ToolCall {
			entry["tool_call"] = true
		}
		if md.StructuredOutput {
			entry["structured_output"] = true
		}
		data = append(data, entry)
	}
	for _, group := range g.rotation.Snapshot().Groups {
		if g.goNodes.Len() == 0 {
			continue
		}
		available := false
		for _, model := range group.Order {
			if route, err := g.catalog.RouteForTier(model, config.TierGo, false, true); err == nil && route.Protocol != wire.SystemOne {
				available = true
				break
			}
		}
		if !available {
			continue
		}
		data = append(data, map[string]any{"id": group.Alias, "object": "model", "created": now, "owned_by": "opencode-go-rotation"})
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"object": "list", "data": data})
}
