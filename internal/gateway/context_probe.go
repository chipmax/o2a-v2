package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"opencode2api/internal/config"
	"opencode2api/internal/identity"
	modelcatalog "opencode2api/internal/models"
	"opencode2api/internal/protocol"
)

const (
	// contextProbeDefaultRequests is the default request budget for one probe.
	// A probe grows the window by 15% per success, so four requests raise the
	// advertised value by more than half a doubling while still stopping early
	// once the upstream rejects a prompt.
	contextProbeDefaultRequests = 4
	// contextProbeMaxRequests bounds the operator-supplied budget; every
	// attempt costs real tokens from the free tier.
	contextProbeMaxRequests = 8
	// contextProbeHardCap stops a probe from ever building a request larger
	// than this, whatever the advertised window claims.
	contextProbeHardCap = 2_000_000
	// contextProbeFallbackRatio is the bytes-per-token estimate used before
	// traffic has calibrated the model. English filler is about 3.6; the extra
	// margin keeps early attempts below the limit.
	contextProbeFallbackRatio = 4.0
	// contextProbeAttemptTimeout bounds one attempt. A 300k-token prompt on the
	// free lane can take a minute or two; five minutes leaves room without
	// letting a hung upstream block the operator forever.
	contextProbeAttemptTimeout = 5 * time.Minute
)

// QuotaExhaustedError reports that the probe could not learn anything because
// the free-tier bucket for the egress it used is spent. It is deliberately
// distinct from a context rejection: the window is unknown, not smaller.
type QuotaExhaustedError struct {
	Model      string
	RetryAfter time.Duration
	Detail     string
}

func (e *QuotaExhaustedError) Error() string {
	if e.RetryAfter > 0 {
		return fmt.Sprintf("free-tier quota is spent for this egress; the bucket resets in %s",
			e.RetryAfter.Round(time.Second))
	}
	return "free-tier quota is spent for this egress"
}

// QuotaExhausted reports whether err is a spent free-tier bucket rather than a
// measurement failure.
func QuotaExhausted(err error) (*QuotaExhaustedError, bool) {
	var quota *QuotaExhaustedError
	if errors.As(err, &quota) {
		return quota, true
	}
	return nil, false
}

// ContextLimitInfo is the context-window state the gateway would advertise for
// one model right now, together with the observations behind it.
type ContextLimitInfo struct {
	Model      string      `json:"model"`
	Tier       config.Tier `json:"tier"`
	Context    int         `json:"context_window"`
	Observed   int         `json:"observed,omitempty"`
	RejectedAt int         `json:"rejected_at,omitempty"`
	Ceiling    int         `json:"ceiling,omitempty"`
	Source     string      `json:"source"`
}

// ContextLimitInfo reports the advertised context window and the observations
// for model, or false when the model cannot be routed on the chat protocol.
func (m *RuntimeController) ContextLimitInfo(model string) (ContextLimitInfo, bool) {
	runtime := m.current.Load()
	if runtime == nil || runtime.gateway == nil {
		return ContextLimitInfo{}, false
	}
	g := runtime.gateway
	route := g.catalog.Diagnostic(model, protocol.Protocol("chat"), len(g.cfg.ZenKeys) > 0, len(g.cfg.GoKeys) > 0, g.cfg.Anonymous)
	if route.RouteError != "" || route.Tier == "" {
		return ContextLimitInfo{}, false
	}
	md := g.modelLimits(model, route.Tier, g.catalog.MetadataForTier(model, route.Tier))
	info := ContextLimitInfo{Model: model, Tier: route.Tier, Context: md.ContextWindow, Source: "catalog"}
	if g.contextLimits != nil {
		obs := g.contextLimits.Observation(model, route.Tier)
		info.Observed, info.RejectedAt, info.Ceiling = obs.Observed, obs.RejectedAt, obs.Ceiling
		if obs.Observed > 0 || obs.RejectedAt > 0 || obs.Ceiling > 0 {
			info.Source = "learned"
		}
	}
	if limit, ok := g.cfg.Models.ContextOverrides[model]; ok && limit.Context > 0 {
		info.Source = "config"
	}
	return info, true
}

// ContextProbeAttempt is one request sent while measuring a model's window.
// TargetTokens is the size the gateway aimed for, PromptTokens the exact count
// the upstream reported on success.
type ContextProbeAttempt struct {
	TargetTokens int    `json:"target_tokens"`
	Status       int    `json:"status"`
	DurationMS   int64  `json:"duration_ms"`
	PromptTokens int    `json:"prompt_tokens,omitempty"`
	RetryAfter   int    `json:"retry_after_seconds,omitempty"`
	Detail       string `json:"detail,omitempty"`
}

// ContextProbeResult reports what one measurement established: the window
// before and after, the observations now stored, and every request that was
// sent so the operator can see the cost of the answer.
type ContextProbeResult struct {
	Model            string                `json:"model"`
	Tier             config.Tier           `json:"tier"`
	AdvertisedBefore int                   `json:"advertised_before"`
	AdvertisedAfter  int                   `json:"advertised_after"`
	Observed         int                   `json:"observed,omitempty"`
	RejectedAt       int                   `json:"rejected_at,omitempty"`
	Ceiling          int                   `json:"ceiling,omitempty"`
	Source           string                `json:"source"`
	Outcome          string                `json:"outcome"`
	Attempts         []ContextProbeAttempt `json:"attempts"`
	ElapsedMS        int64                 `json:"elapsed_ms"`
}

// ProbeContext measures how much context model actually accepts on the free
// lane by sending real requests through this gateway and recording what the
// upstream reports. It starts just above the advertised window and grows by
// 15% per success, bisecting after the first rejection, so a model whose real
// window is far above the catalog converges in a handful of requests.
//
// The requests are marked diagnostic, so they do not update key or relay
// cooldowns and do not count as conversation traffic; their results are folded
// into the context-limit board explicitly.
func (m *RuntimeController) ProbeContext(ctx context.Context, model string, maxRequests int) (ContextProbeResult, error) {
	result := ContextProbeResult{Model: model}
	runtime := m.current.Load()
	if runtime == nil || runtime.gateway == nil {
		return result, errors.New("gateway runtime is unavailable")
	}
	g := runtime.gateway
	key := runtime.config.FirstEnabledServerKey()
	if key == "" {
		return result, errors.New("no local server key is configured")
	}
	if g.contextLimits == nil {
		return result, errors.New("context observations are unavailable")
	}
	route := g.catalog.Diagnostic(model, protocol.Protocol("chat"), len(g.cfg.ZenKeys) > 0, len(g.cfg.GoKeys) > 0, g.cfg.Anonymous)
	if route.RouteError != "" {
		return result, fmt.Errorf("model %s is not routable: %s", model, route.RouteError)
	}
	if route.Tier == "" {
		return result, fmt.Errorf("model %s has no serving tier", model)
	}
	tier := route.Tier
	store := g.contextLimits
	advertised := func() int {
		return g.modelLimits(model, tier, g.catalog.MetadataForTier(model, tier)).ContextWindow
	}
	result.Tier = tier
	result.AdvertisedBefore = advertised()

	budget := maxRequests
	if budget <= 0 {
		budget = contextProbeDefaultRequests
	}
	if budget > contextProbeMaxRequests {
		budget = contextProbeMaxRequests
	}

	observation := store.Observation(model, tier)
	bytesPerToken := observation.BytesPerToken
	if bytesPerToken < 2 || bytesPerToken > 8 {
		bytesPerToken = contextProbeFallbackRatio
	}
	lower := observation.Observed
	upper := 0
	target := int(float64(result.AdvertisedBefore) * 1.12)
	if result.AdvertisedBefore <= 0 {
		target = 100_000
	}
	if lower > 0 && int(float64(lower)*1.12) > target {
		target = int(float64(lower) * 1.12)
	}
	if target < 1_000 {
		target = 1_000
	}

	// A probe on a fully cooling anonymous pool would spend requests that can
	// only answer 429. Report the bucket as spent (with the soonest reopen)
	// instead of burning the operator's remaining quota on a doomed attempt.
	if g.cfg.Anonymous && g.warp != nil {
		live, soonest := 0, time.Duration(0)
		states := g.warp.States()
		for _, state := range states {
			if !state.Cooling {
				live++
				continue
			}
			until := time.Until(state.CoolingUntil)
			if until > 0 && (soonest == 0 || until < soonest) {
				soonest = until
			}
		}
		if len(states) > 0 && live == 0 {
			return result, &QuotaExhaustedError{Model: model, RetryAfter: soonest}
		}
	}

	started := time.Now()
	for len(result.Attempts) < budget {
		if err := ctx.Err(); err != nil {
			break
		}
		if upper > 0 && lower > 0 && upper-lower <= upper/50 {
			break
		}
		attemptTarget := target
		if upper > 0 {
			attemptTarget = (lower + upper) / 2
			if attemptTarget <= lower || attemptTarget >= upper {
				break
			}
		}
		if attemptTarget > contextProbeHardCap {
			attemptTarget = contextProbeHardCap
		}
		attempt, requestBytes, promptTokens, body, err := runContextProbe(ctx, m.Handler(), key, model, attemptTarget, bytesPerToken)
		result.Attempts = append(result.Attempts, attempt)
		if err != nil {
			return result, err
		}
		if attempt.Status >= 200 && attempt.Status < 300 {
			if promptTokens > 0 {
				store.ObserveSuccess(model, tier, promptTokens, requestBytes)
				if promptTokens > lower {
					lower = promptTokens
				}
			}
			if upper == 0 {
				next := int(float64(max(lower, attemptTarget)) * 1.15)
				if next <= attemptTarget {
					next = attemptTarget + attemptTarget/5
				}
				if next > contextProbeHardCap {
					break
				}
				target = next
			}
			continue
		}
		if isContextLengthResponse(attempt.Status, body) {
			store.ObserveRejection(model, tier, requestBytes, body, advertised())
			upper = attemptTarget
			if current := advertised(); current > 0 && current < upper {
				upper = current
			}
			if lower == 0 {
				break
			}
			continue
		}
		if attempt.Status == http.StatusTooManyRequests {
			return result, &QuotaExhaustedError{
				Model:      model,
				RetryAfter: time.Duration(attempt.RetryAfter) * time.Second,
				Detail:     attempt.Detail,
			}
		}
		return result, fmt.Errorf("upstream answered HTTP %d: %s", attempt.Status, attempt.Detail)
	}

	result.AdvertisedAfter = advertised()
	if observation = store.Observation(model, tier); observation.Observed > 0 || observation.RejectedAt > 0 || observation.Ceiling > 0 {
		result.Source = "learned"
	} else {
		result.Source = "catalog"
	}
	result.Observed, result.RejectedAt, result.Ceiling = observation.Observed, observation.RejectedAt, observation.Ceiling
	switch {
	case result.AdvertisedAfter > result.AdvertisedBefore:
		result.Outcome = "raised"
	case result.AdvertisedAfter < result.AdvertisedBefore:
		result.Outcome = "lowered"
	default:
		result.Outcome = "unchanged"
	}
	result.ElapsedMS = max(time.Since(started).Milliseconds(), 0)
	return result, nil
}

// probeCapture is a minimal http.ResponseWriter that keeps the status and body
// of a probe attempt instead of sending them.
type probeCapture struct {
	header http.Header
	body   bytes.Buffer
	status int
}

func (recorder *probeCapture) Header() http.Header { return recorder.header }

func (recorder *probeCapture) WriteHeader(status int) {
	if recorder.status != http.StatusOK || status == http.StatusOK {
		return
	}
	recorder.status = status
}

func (recorder *probeCapture) Write(data []byte) (int, error) {
	return recorder.body.Write(data)
}

// runContextProbe sends one filler prompt of about targetTokens tokens through
// the gateway and reports what happened. requestBytes is the body size used to
// calibrate rejected-prompt estimates.
func runContextProbe(ctx context.Context, handler http.Handler, key, model string, targetTokens int, bytesPerToken float64) (ContextProbeAttempt, int64, int, []byte, error) {
	attempt := ContextProbeAttempt{TargetTokens: targetTokens}
	payload := contextProbePayload(model, targetTokens, bytesPerToken)
	bodyLen := int64(len(payload))
	requestCtx, cancel := context.WithTimeout(WithDiagnosticRequest(ctx), contextProbeAttemptTimeout)
	defer cancel()
	request, err := http.NewRequestWithContext(requestCtx, http.MethodPost, "http://gateway.local/v1/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return attempt, bodyLen, 0, nil, errors.New("could not construct the probe request")
	}
	request.Header.Set("Authorization", "Bearer "+key)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "application/json")
	request.Header.Set("x-opencode-session", identity.CanonicalSessionID(identity.RandomID("probe", 16)))

	recorder := &probeCapture{header: make(http.Header), status: http.StatusOK}
	started := time.Now()
	handler.ServeHTTP(recorder, request)
	attempt.DurationMS = max(time.Since(started).Milliseconds(), 0)
	attempt.Status = recorder.status
	body := recorder.body.Bytes()
	if len(body) > 16_384 {
		attempt.Detail = truncateProbeDetail(body[:16_384])
	} else {
		attempt.Detail = truncateProbeDetail(body)
	}
	if attempt.Status >= 200 && attempt.Status < 300 {
		var decoded struct {
			Usage struct {
				PromptTokens int `json:"prompt_tokens"`
				InputTokens  int `json:"input_tokens"`
			} `json:"usage"`
		}
		if json.Unmarshal(body, &decoded) == nil {
			attempt.PromptTokens = decoded.Usage.PromptTokens
			if attempt.PromptTokens == 0 {
				attempt.PromptTokens = decoded.Usage.InputTokens
			}
		}
		if isQuotaProbeBody(body) {
			// The v2 lane answers 200 with an error document once the egress
			// /64 has spent its bucket: that is quota, not a context answer.
			attempt.Status = http.StatusTooManyRequests
			attempt.RetryAfter = int(quotaResetAfter(time.Now()).Seconds())
		} else {
			attempt.Detail = ""
		}
	}
	if attempt.Status == http.StatusTooManyRequests && attempt.RetryAfter == 0 {
		attempt.RetryAfter = int(parseRetryAfter(recorder.header.Get("Retry-After")).Seconds())
	}
	return attempt, bodyLen, attempt.PromptTokens, body, nil
}

// contextProbePayload builds a one-message chat request whose user content is
// about targetTokens tokens of neutral filler. The size is only an estimate;
// the upstream's usage count replaces it on success.
func contextProbePayload(model string, targetTokens int, bytesPerToken float64) []byte {
	const sentence = "The quick brown fox jumps over the lazy dog. "
	wanted := int(float64(targetTokens) * bytesPerToken)
	if wanted < 512 {
		wanted = 512
	}
	repeat := wanted / len(sentence)
	if repeat < 1 {
		repeat = 1
	}
	payload := map[string]any{
		"model":      model,
		"messages":   []map[string]any{{"role": "user", "content": strings.Repeat(sentence, repeat)}},
		"max_tokens": 16,
		"stream":     false,
	}
	encoded, err := json.Marshal(payload)
	if err != nil {
		return []byte("{}")
	}
	return encoded
}

// isQuotaProbeBody reports a probe answer that is really the free tier's quota
// error document (the v2 lane sends it with HTTP 200). It reuses the gateway's
// quota markers so the probe and the live path agree on the wording.
func isQuotaProbeBody(body []byte) bool {
	text := strings.ToLower(string(body))
	if !strings.Contains(text, `"error"`) {
		return false
	}
	for _, marker := range quotaErrorMarkers {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

// truncateProbeDetail is the short explanation kept for one attempt: the
// upstream error text without control characters and limited to a few hundred
// characters so the WebUI stays readable.
func truncateProbeDetail(body []byte) string {
	text := strings.TrimSpace(string(body))
	text = strings.ReplaceAll(text, "\n", " ")
	text = strings.ReplaceAll(text, "\r", " ")
	if len(text) > 320 {
		text = text[:320] + "…"
	}
	return text
}

// ContextLimits returns the shared observation board, used by diagnostics.
func (m *RuntimeController) ContextLimits() *modelcatalog.ContextLimits {
	return m.contextLimits
}
