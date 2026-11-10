package config

import (
	"fmt"
	"regexp"
	"slices"
)

// SOTAModels seeds the default order of the "sota" rotation group. It is only
// a starting point: the group may be curated to any subset, and entries that
// disappear from the catalog are ignored at sync time. Every model outside the
// sota order can be discovered by the "sweet" group.
var SOTAModels = []string{"deepseek-v4.1-flash", "glm-5.3", "grok-4.6", "kimi-k3", "qwen3.8-max"}

// RotationGroup is one model-rotation group: Alias is the model name clients
// request to use the group, and Order is the failover order of its models.
type RotationGroup struct {
	Alias string   `json:"alias"`
	Order []string `json:"order"`
}

// RotationConfig configures the two model-rotation groups, which are served by
// the Go tier. FirstOutputSeconds bounds how long an attempt may take to
// produce its first output before the next attempt runs.
type RotationConfig struct {
	SOTA               RotationGroup `json:"sota"`
	Sweet              RotationGroup `json:"sweet"`
	FirstOutputSeconds int           `json:"first_output_seconds"`
}

// IsSOTA reports whether model is one of SOTAModels.
func IsSOTA(model string) bool { return slices.Contains(SOTAModels, model) }

// Defaults fills in what is unset: the aliases "sota" and "sweet", the SOTA
// order (SOTAModels) and a 30-second first-output timeout. The sweet order has
// no default: the rotation state adds the Go tier's other models to the group
// as they appear.
func (c *RotationConfig) Defaults() {
	if c.SOTA.Alias == "" {
		c.SOTA.Alias = "sota"
	}
	if c.Sweet.Alias == "" {
		c.Sweet.Alias = "sweet"
	}
	if len(c.SOTA.Order) == 0 {
		c.SOTA.Order = slices.Clone(SOTAModels)
	}
	if c.FirstOutputSeconds == 0 {
		c.FirstOutputSeconds = 30
	}
}

// Validate requires a first-output timeout of 1-300 seconds; two distinct
// aliases of up to 64 letters, digits, dots, dashes or underscores, starting
// with a letter or digit, that do not shadow a model ID (one of realModels,
// SOTAModels or the sweet order); and orders without empty or repeated
// entries. Orders are not required to match a hard-coded catalog. A nil
// realModels skips the catalog check.
func (c RotationConfig) Validate(realModels []string) error {
	if c.FirstOutputSeconds < 1 || c.FirstOutputSeconds > 300 {
		return fmt.Errorf("first_output_seconds must be between 1 and 300")
	}
	if c.SOTA.Alias == c.Sweet.Alias {
		return fmt.Errorf("the two rotation aliases must differ")
	}
	valid := regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,63}$`)
	for _, g := range []RotationGroup{c.SOTA, c.Sweet} {
		if !valid.MatchString(g.Alias) {
			return fmt.Errorf("an alias must be 1-64 letters, digits, dots, dashes or underscores")
		}
		if slices.Contains(realModels, g.Alias) || IsSOTA(g.Alias) || slices.Contains(c.Sweet.Order, g.Alias) {
			return fmt.Errorf("an alias must not shadow a real model id")
		}
		seen := map[string]bool{}
		for _, model := range g.Order {
			if model == "" || seen[model] {
				return fmt.Errorf("a rotation order contains an empty or repeated model")
			}
			seen[model] = true
		}
		// Orders are intentionally not tied to a hard-coded catalog: a model
		// that disappears upstream becomes unavailable at sync time instead of
		// making the whole configuration fail.
	}
	return nil
}
