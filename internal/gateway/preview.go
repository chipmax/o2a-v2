package gateway

import (
	"time"

	"opencode2api/internal/config"
	modelcatalog "opencode2api/internal/models"
	wire "opencode2api/internal/protocol"
)

// PreviewStep is one lane the gateway would try for a request right now, with
// the state that decided its verdict. It is read-only: nothing is sent
// upstream and no cooldown is touched.
type PreviewStep struct {
	Lane        string `json:"lane"`
	Tier        string `json:"tier,omitempty"`
	Verdict     string `json:"verdict"`
	Reason      string `json:"reason,omitempty"`
	Keys        int    `json:"keys,omitempty"`
	UsableKeys  int    `json:"usable_keys,omitempty"`
	CoolingKeys int    `json:"cooling_keys,omitempty"`
	Quarantined int    `json:"quarantined_keys,omitempty"`
	RetryAfter  int    `json:"retry_after_seconds,omitempty"`
	Exits       int    `json:"exits,omitempty"`
	Healthy     int    `json:"healthy_exits,omitempty"`
}

// PreviewPlan answers "what would happen to this model right now, and why".
// Ready is the honest readiness the operator sees: true only when at least one
// lane would actually be tried.
type PreviewPlan struct {
	Model       string        `json:"model"`
	Protocol    string        `json:"protocol,omitempty"`
	Transport   string        `json:"transport,omitempty"`
	Ready       bool          `json:"ready"`
	Blockers    []string      `json:"blockers,omitempty"`
	Steps       []PreviewStep `json:"steps"`
	Note        string        `json:"note"`
	GeneratedAt time.Time     `json:"generated_at"`
}

// Verdict values, kept as strings so the UI can style them without a mapping.
const (
	verdictTry      = "try"
	verdictFallback = "fallback"
	verdictSkip     = "skip"
)

// PreviewRoute builds the routing plan for model without sending a request.
// It reports the same decision the request path makes (anonymous lane first,
// then the key tiers in preference order, then peers) plus the live state that
// could veto each lane: wall, breaker, cooldown, quarantine, exit health.
func (g *Gateway) PreviewRoute(model string, requested wire.Protocol) PreviewPlan {
	plan := PreviewPlan{
		Model:       model,
		Protocol:    string(requested),
		Note:        "kế hoạch chỉ đọc: không gửi request nào tới upstream và không đổi cooldown",
		GeneratedAt: time.Now().UTC(),
	}
	now := time.Now()
	diagnostic := g.catalog.Diagnostic(model, requested, len(g.cfg.ZenKeys) > 0, len(g.cfg.GoKeys) > 0, g.cfg.Anonymous)
	if diagnostic.NativeProtocol != "" {
		plan.Protocol = string(diagnostic.NativeProtocol)
	}
	if diagnostic.RouteError != "" {
		plan.Blockers = append(plan.Blockers, diagnostic.RouteError)
		return plan
	}

	proxyTotal, proxyHealthy := 0, 0
	if g.transports != nil {
		proxyTotal, proxyHealthy = g.transports.healthCounts()
	}
	warpActive, warpCooling, laneGated := 0, 0, false
	if g.warp != nil {
		snapshot := g.warp.Snapshot()
		warpActive, warpCooling, laneGated = snapshot.Active, snapshot.Cooling, snapshot.LaneGated
	}
	plan.Transport = g.previewTransport(proxyHealthy, warpActive)

	if diagnostic.Anonymous {
		plan.Steps = append(plan.Steps, g.previewAnonymousStep(model, now, proxyTotal, proxyHealthy, warpActive, warpCooling, laneGated))
	}

	tiers := diagnostic.KeyTiers
	if len(tiers) == 0 && !diagnostic.Anonymous && (diagnostic.Tier == config.TierZen || diagnostic.Tier == config.TierGo) {
		tiers = []config.Tier{diagnostic.Tier}
	}
	for _, tier := range tiers {
		plan.Steps = append(plan.Steps, g.previewKeyStep(tier, now, proxyHealthy, warpActive))
	}

	if g.peers != nil && g.peers.Len() > 0 {
		step := PreviewStep{Lane: "peer", Verdict: verdictFallback, Exits: g.peers.Len()}
		if plan.Transport == "" {
			step.Reason = "chỉ dùng khi mọi làn cục bộ thất bại"
		} else {
			step.Reason = "dự phòng khi mọi làn cục bộ thất bại"
		}
		plan.Steps = append(plan.Steps, step)
	}

	for _, step := range plan.Steps {
		if step.Verdict == verdictTry {
			plan.Ready = true
		}
	}
	if !plan.Ready {
		plan.Blockers = append(plan.Blockers, previewBlockers(plan.Steps)...)
		if len(plan.Blockers) == 0 {
			plan.Blockers = append(plan.Blockers, "không có làn nào sẵn sàng")
		}
	}
	return plan
}

// previewTransport names the path a free-lane request would leave through.
func (g *Gateway) previewTransport(proxyHealthy, warpActive int) string {
	switch {
	case g.warp != nil && warpActive > 0:
		return "warp"
	case proxyHealthy > 0:
		return "proxy"
	case g.cfg.Anonymous:
		return ""
	default:
		return "direct"
	}
}

func (g *Gateway) previewAnonymousStep(model string, now time.Time, proxyTotal, proxyHealthy, warpActive, warpCooling int, laneGated bool) PreviewStep {
	step := PreviewStep{Lane: "anonymous", Tier: string(config.TierZen), Exits: proxyTotal, Healthy: proxyHealthy}
	if g.warp != nil {
		step.Exits, step.Healthy = warpActive+warpCooling, warpActive
	}
	switch {
	case g.anonymous != nil && g.anonymous.Open():
		step.Verdict = verdictSkip
		step.Reason = "cầu dao làn ẩn danh đang mở sau nhiều lần lỗi"
		return step
	case laneGated:
		step.Verdict = verdictSkip
		step.Reason = "lane gate đang tạm dừng đăng ký exit mới"
		return step
	case step.Healthy == 0:
		step.Verdict = verdictSkip
		step.Reason = "không có exit nào sống (WARP/proxy)"
		return step
	}
	step.Verdict = verdictTry
	step.Reason = "làn miễn phí đi trước"
	if g.anonymous != nil && g.anonymous.wallActive(model, now) {
		if g.hasUsableKeyFallback(modelcatalog.Route{ID: model, KeyTiers: g.anonymousFallbackTiers()}) {
			step.Reason = "tường quota đang mở: chỉ thử 1 exit rồi chuyển sang khoá"
		} else {
			step.Reason = "tường quota đang mở nhưng không có khoá dự phòng: vẫn đi hết danh sách exit"
		}
	}
	if g.warp != nil {
		step.Reason += " · WARP"
	}
	return step
}

// anonymousFallbackTiers lists the tiers an anonymous-first free route could
// fall into, so the wall explanation uses the same test as the request path.
func (g *Gateway) anonymousFallbackTiers() []config.Tier {
	tiers := make([]config.Tier, 0, 2)
	if len(g.cfg.ZenKeys) > 0 {
		tiers = append(tiers, config.TierZen)
	}
	if len(g.cfg.GoKeys) > 0 {
		tiers = append(tiers, config.TierGo)
	}
	return tiers
}

func (g *Gateway) previewKeyStep(tier config.Tier, now time.Time, proxyHealthy, warpActive int) PreviewStep {
	nodes := g.zenNodes
	if tier == config.TierGo {
		nodes = g.goNodes
	}
	step := PreviewStep{Lane: "key", Tier: string(tier)}
	if nodes == nil || nodes.Len() == 0 {
		step.Verdict = verdictSkip
		step.Reason = "tier chưa cấu hình khoá"
		return step
	}
	cooling, quarantined, nextReady, nextCredential := nodes.CoolingSummary(now)
	step.Keys = nodes.Len()
	step.CoolingKeys = cooling
	step.Quarantined = quarantined
	step.UsableKeys = nodes.Len() - cooling
	if step.UsableKeys < 0 {
		step.UsableKeys = 0
	}
	switch {
	case quarantined == nodes.Len():
		step.Verdict = verdictSkip
		step.Reason = "mọi khoá bị upstream từ chối (401/403) và đang cách ly"
		step.RetryAfter = int(time.Until(nextCredential).Seconds())
		return step
	case cooling == nodes.Len():
		step.Verdict = verdictSkip
		step.Reason = "mọi khoá đang nghỉ sau khi bị giới hạn"
		step.RetryAfter = int(time.Until(nextReady).Seconds())
		return step
	}
	step.Verdict = verdictTry
	if cooling > 0 || quarantined > 0 {
		step.Reason = "còn khoá dùng được"
	} else {
		step.Reason = "mọi khoá sẵn sàng"
	}
	if g.warp != nil && warpActive > 0 {
		step.Reason += " · WARP"
	} else if proxyHealthy == 0 {
		step.Verdict = verdictSkip
		step.Reason = "không có proxy khoẻ để đi"
	}
	return step
}

// previewBlockers turns skipped lanes into short operator-facing reasons.
func previewBlockers(steps []PreviewStep) []string {
	blockers := make([]string, 0, len(steps))
	for _, step := range steps {
		if step.Verdict != verdictSkip || step.Reason == "" {
			continue
		}
		label := step.Lane
		if step.Tier != "" {
			label += ":" + step.Tier
		}
		blockers = append(blockers, label+" — "+step.Reason)
	}
	return blockers
}
