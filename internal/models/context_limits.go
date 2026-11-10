package models

import (
	"encoding/json"
	"fmt"
	"math"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"sync"
	"time"

	"opencode2api/internal/config"
)

const (
	contextLimitsVersion = 1
	// The catalog and config both cap a model's context at this value; reject
	// anything outside the same range when restoring observations so a hand
	// edit cannot make discovery advertise a nonsensical window.
	minContextLimit = 1_000
	maxContextLimit = 10_000_000
	// Context observations are saved lazily. Losing the last few seconds of
	// samples on a hard kill is harmless; writing on every request is not.
	contextSaveInterval = 5 * time.Second
	// Only calibrate bytes-per-token from requests large enough that the
	// gateway's injected system prompt and tool schema are a small share of
	// the total. Request bodies below this size say more about the preamble
	// than about the model's tokenizer.
	contextCalibrationFloor = 32_768
)

// ContextObservation is what live traffic has proven about one model's usable
// context on one tier. Observed is the largest prompt the upstream accepted,
// RejectedAt the smallest prompt a context-limit error rejected, and Ceiling a
// numeric limit parsed from an upstream error body. BytesPerToken calibrates
// the client request body size to prompt_tokens for estimating rejected
// requests that carry no token count in their error body.
type ContextObservation struct {
	Observed      int       `json:"observed,omitempty"`
	RejectedAt    int       `json:"rejected_at,omitempty"`
	Ceiling       int       `json:"ceiling,omitempty"`
	BytesPerToken float64   `json:"bytes_per_token,omitempty"`
	UpdatedAt     time.Time `json:"updated_at,omitempty"`
}

// ContextLimits is a process-wide, persistent board of traffic observations
// keyed by tier and model. It survives hot reloads (the controller shares one
// pointer) and process restarts (it is saved next to the config).
type ContextLimits struct {
	mu       sync.Mutex
	path     string
	entries  map[string]ContextObservation
	lastSave time.Time
}

type contextLimitsFile struct {
	Version int                           `json:"version"`
	Entries map[string]ContextObservation `json:"entries"`
}

// ContextLimitStatePath returns the sidecar file used for a config path. An
// empty config path disables persistence, which keeps tests and embedded uses
// side-effect free.
func ContextLimitStatePath(configPath string) string {
	if configPath == "" {
		return ""
	}
	return configPath + ".context-limits.json"
}

// OpenContextLimits loads the observations saved for path. A missing file is
// normal. A malformed file returns a usable empty board plus an error so the
// caller can log the damage and keep serving; the next save replaces it.
func OpenContextLimits(path string) (*ContextLimits, error) {
	limits := &ContextLimits{path: path, entries: make(map[string]ContextObservation)}
	if path == "" {
		return limits, nil
	}
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return limits, nil
		}
		return limits, err
	}
	var file contextLimitsFile
	if err := json.Unmarshal(data, &file); err != nil {
		return limits, fmt.Errorf("parse context limits %s: %w", path, err)
	}
	if file.Version != contextLimitsVersion {
		return limits, fmt.Errorf("context limits %s has unsupported version %d", path, file.Version)
	}
	for key, entry := range file.Entries {
		tier, model, ok := splitContextKey(key)
		if !ok || model == "" {
			continue
		}
		entry.Observed = clampContextLimit(entry.Observed)
		entry.RejectedAt = clampContextLimit(entry.RejectedAt)
		entry.Ceiling = clampContextLimit(entry.Ceiling)
		if math.IsNaN(entry.BytesPerToken) || math.IsInf(entry.BytesPerToken, 0) ||
			entry.BytesPerToken < 1 || entry.BytesPerToken > 16 {
			entry.BytesPerToken = 0
		}
		if entry.Observed == 0 && entry.RejectedAt == 0 && entry.Ceiling == 0 {
			continue
		}
		limits.entries[string(tier)+":"+model] = entry
	}
	return limits, nil
}

// Save flushes the board atomically. Errors are intentionally ignored at the
// call site: observations are an optimization, and a read-only config
// directory must not make inference fail.
func (c *ContextLimits) Save() {
	if c == nil {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.saveLocked(time.Now())
}

// Advertised returns the context window a discovery client should see. catalog
// is the value the capability metadata supplies, seed is the operator's
// models.context_overrides value (zero when unset), and traffic observations
// can raise the catalog/seed or lower it after an upstream rejection.
func (c *ContextLimits) Advertised(model string, tier config.Tier, catalog, seed int) int {
	base := catalog
	if seed > 0 {
		base = seed
	}
	base = clampContextLimit(base)
	if c == nil || model == "" {
		return base
	}
	c.mu.Lock()
	entry, ok := c.entries[contextKey(model, tier)]
	c.mu.Unlock()
	if !ok {
		return base
	}
	if observed := contextSafe(entry.Observed); observed > base {
		base = observed
	}
	if entry.Ceiling > 0 {
		if ceiling := contextSafe(entry.Ceiling); base == 0 || ceiling < base {
			base = ceiling
		}
	}
	if entry.RejectedAt > 0 {
		if rejected := contextRejectedSafe(entry.RejectedAt); base == 0 || rejected < base {
			base = rejected
		}
	}
	return base
}

// Observed returns the largest successful prompt recorded for model/tier. It is
// used by diagnostics and tests; zero means no large success has been seen.
func (c *ContextLimits) Observed(model string, tier config.Tier) int {
	if c == nil || model == "" {
		return 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.entries[contextKey(model, tier)].Observed
}

// Observation returns a copy of the measured entry for model/tier. The zero
// value means no traffic has proven anything about this model yet.
func (c *ContextLimits) Observation(model string, tier config.Tier) ContextObservation {
	if c == nil || model == "" {
		return ContextObservation{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.entries[contextKey(model, tier)]
}

// ObserveSuccess folds one successful response into the board. requestBytes is
// the client request body size that produced promptTokens; it calibrates the
// estimate used when an error body carries no numeric context limit. A success
// at or near a previously recorded rejection/ceiling clears that stale upper
// bound: the limit has clearly moved or the earlier observation was noisy.
func (c *ContextLimits) ObserveSuccess(model string, tier config.Tier, promptTokens int, requestBytes int64) {
	if c == nil || model == "" || promptTokens <= 0 {
		return
	}
	now := time.Now().UTC()
	c.mu.Lock()
	defer c.mu.Unlock()
	key := contextKey(model, tier)
	entry := c.entries[key]
	changed := false
	if entry.Ceiling > 0 && promptTokens*100 >= entry.Ceiling*98 {
		entry.Ceiling = 0
		changed = true
	}
	if entry.RejectedAt > 0 && promptTokens*100 >= entry.RejectedAt*98 {
		entry.RejectedAt = 0
		changed = true
	}
	if promptTokens > entry.Observed {
		entry.Observed = promptTokens
		changed = true
	}
	if requestBytes > 0 && promptTokens >= contextCalibrationFloor {
		ratio := float64(requestBytes) / float64(promptTokens)
		if ratio >= 1 && ratio <= 16 && !math.IsNaN(ratio) && !math.IsInf(ratio, 0) {
			if entry.BytesPerToken == 0 {
				entry.BytesPerToken = ratio
			} else {
				// Bias toward recent evidence without letting one odd body
				// (images, huge tool schemas) erase the calibration.
				entry.BytesPerToken = entry.BytesPerToken*0.8 + ratio*0.2
			}
			changed = true
		}
	}
	if !changed {
		return
	}
	entry.UpdatedAt = now
	c.entries[key] = entry
	c.maybeSaveLocked(now)
}

// ObserveRejection folds one context-limit error into the board. advertised is
// the value the gateway published when the request was sent; it is used to
// distinguish an agent that overshot a trusted seed from a request inside the
// advertised window that failed because the upstream limit moved down.
//
// The returned estimate is 0 when no calibration exists, and applied reports
// whether the observation changed the advertised window.
func (c *ContextLimits) ObserveRejection(model string, tier config.Tier, requestBytes int64, body []byte, advertised int) (estimate int, applied bool) {
	if c == nil || model == "" {
		return 0, false
	}
	explicit := parseContextLimit(body)
	now := time.Now().UTC()
	c.mu.Lock()
	defer c.mu.Unlock()
	key := contextKey(model, tier)
	entry := c.entries[key]
	if requestBytes > 0 && entry.BytesPerToken > 0 {
		// The 3% factor covers the system prompt/tool schema the gateway
		// injects on top of the client body; without it the estimate would be
		// consistently a little below the real prompt_tokens count.
		estimate = int(math.Round(float64(requestBytes) / entry.BytesPerToken * 1.03))
	}
	changed := false
	if explicit > 0 {
		if entry.Ceiling == 0 || explicit < entry.Ceiling {
			entry.Ceiling = explicit
			changed = true
			applied = true
		}
	}
	if estimate > 0 {
		withinProven := entry.Observed > 0 && estimate <= entry.Observed
		withinBudget := advertised > 0 && estimate <= advertised+advertised/20
		if withinProven || (entry.Observed > 0 && entry.Observed < advertised && withinBudget) || (entry.Observed == 0 && withinBudget) {
			if entry.RejectedAt == 0 || estimate < entry.RejectedAt {
				entry.RejectedAt = estimate
				applied = true
			}
			if withinProven && entry.Observed > 0 {
				// A request inside a previously proven window failed, so the
				// old success is no longer a usable floor.
				entry.Observed = 0
			}
			changed = true
		}
	}
	if !changed {
		return estimate, false
	}
	entry.UpdatedAt = now
	c.entries[key] = entry
	c.maybeSaveLocked(now)
	return estimate, applied
}

func (c *ContextLimits) saveLocked(now time.Time) {
	if c == nil || c.path == "" {
		return
	}
	entries := make(map[string]ContextObservation, len(c.entries))
	for key, entry := range c.entries {
		entries[key] = entry
	}
	data, err := json.Marshal(contextLimitsFile{Version: contextLimitsVersion, Entries: entries})
	if err != nil {
		return
	}
	if dir := filepath.Dir(c.path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return
		}
	}
	tmp := c.path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o600); err != nil {
		return
	}
	if err := os.Rename(tmp, c.path); err != nil {
		_ = os.Remove(tmp)
		return
	}
	c.lastSave = now
}

func (c *ContextLimits) maybeSaveLocked(now time.Time) {
	if c == nil || c.path == "" || now.Sub(c.lastSave) < contextSaveInterval {
		return
	}
	c.saveLocked(now)
}

func contextKey(model string, tier config.Tier) string {
	return string(tier) + ":" + model
}

func splitContextKey(key string) (config.Tier, string, bool) {
	tier, model, ok := strings.Cut(key, ":")
	if !ok || tier == "" || model == "" {
		return "", "", false
	}
	switch config.Tier(tier) {
	case config.TierZen, config.TierGo:
		return config.Tier(tier), model, true
	default:
		return "", "", false
	}
}

func clampContextLimit(value int) int {
	if value < minContextLimit {
		return 0
	}
	if value > maxContextLimit {
		return maxContextLimit
	}
	return value
}

// contextSafe leaves 5% below a proven or stated limit so an agent's own
// tokenizer, tool schema, image accounting and the gateway's injected preamble
// all have room before the upstream hard limit. Rounding down to 1k keeps
// /v1/models stable and readable.
func contextSafe(value int) int {
	if value <= 0 {
		return 0
	}
	safe := value - value*5/100
	if value >= 10_000 {
		safe = safe / 1_000 * 1_000
	}
	if safe < 1 {
		safe = 1
	}
	return safe
}

// contextRejectedSafe leaves 10% below a prompt the upstream rejected. A
// rejection only proves the limit is somewhere below that prompt, so the
// larger margin avoids another failed turn while traffic converges.
func contextRejectedSafe(value int) int {
	if value <= 0 {
		return 0
	}
	safe := value - value/10
	if value >= 10_000 {
		safe = safe / 1_000 * 1_000
	}
	if safe < 1 {
		safe = 1
	}
	return safe
}

var contextLimitPatterns = []*regexp.Regexp{
	regexp.MustCompile(`(?i)(?:maximum|max)\s+context\s+(?:length|window)\s*(?:is|of|:)?\s*([0-9][0-9,._]*)\s*([kKmM]?)`),
	regexp.MustCompile(`(?i)context\s+(?:length|window)\s*(?:is|of|:)?\s*([0-9][0-9,._]*)\s*([kKmM]?)`),
	regexp.MustCompile(`(?i)tokens?\s*>\s*([0-9][0-9,._]*)\s*([kKmM]?)\s*(?:maximum|max)\b`),
}

// parseContextLimit extracts a numeric context limit from an upstream error
// body. Many providers name the real limit in the message; using it is exact
// and lets the gateway lower the advertised window without waiting for another
// failed request.
func parseContextLimit(body []byte) int {
	if len(body) == 0 {
		return 0
	}
	text := string(body)
	for _, pattern := range contextLimitPatterns {
		match := pattern.FindStringSubmatch(text)
		if len(match) < 2 {
			continue
		}
		number := strings.NewReplacer(",", "", "_", "").Replace(match[1])
		value, err := strconv.ParseFloat(number, 64)
		if err != nil || math.IsNaN(value) || math.IsInf(value, 0) {
			continue
		}
		switch strings.ToLower(match[2]) {
		case "k":
			value *= 1_000
		case "m":
			value *= 1_000_000
		}
		if value < minContextLimit || value > maxContextLimit {
			continue
		}
		return int(value)
	}
	return 0
}
