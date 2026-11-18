package config

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
)

const (
	defaultKiloBaseURL    = "https://ai-gateway.kilo.ai/api/v1"
	defaultKiloCatalogURL = "https://ai-gateway.kilo.ai/api/v1/models"
)

// Normalize fills Kilo defaults and validates the allowlist. An empty Models
// list means the lane is disabled, so a config written before this feature
// keeps serving exactly the same OpenCode routes.
func (k *KiloConfig) Normalize() error {
	if k == nil {
		return nil
	}
	k.BaseURL = strings.TrimSpace(k.BaseURL)
	if k.BaseURL == "" {
		k.BaseURL = defaultKiloBaseURL
	}
	k.CatalogURL = strings.TrimSpace(k.CatalogURL)
	if k.CatalogURL == "" {
		k.CatalogURL = defaultKiloCatalogURL
	}
	k.Key = strings.TrimSpace(k.Key)
	k.QuotaNamespace = strings.TrimSpace(k.QuotaNamespace)
	if k.QuotaNamespace == "" {
		k.QuotaNamespace = "kilo"
	}
	for name, raw := range map[string]string{"kilo.base_url": k.BaseURL, "kilo.catalog_url": k.CatalogURL} {
		u, err := url.Parse(raw)
		if err != nil || u.Host == "" || (strings.ToLower(u.Scheme) != "http" && strings.ToLower(u.Scheme) != "https") {
			return fmt.Errorf("%s must be an http or https URL", name)
		}
	}
	if k.RefreshSeconds <= 0 {
		k.RefreshSeconds = 21600
	}
	if k.CooldownSeconds <= 0 {
		k.CooldownSeconds = 90
	}
	if k.MaxCooldownSeconds <= 0 {
		k.MaxCooldownSeconds = 3600
	}
	if k.CooldownSeconds > k.MaxCooldownSeconds {
		k.CooldownSeconds = k.MaxCooldownSeconds
	}
	seen := make(map[string]bool, len(k.Models))
	models := make([]string, 0, len(k.Models))
	for _, model := range k.Models {
		model = strings.TrimSpace(model)
		if model == "" || seen[model] {
			continue
		}
		if !strings.HasSuffix(model, ":free") && !strings.HasSuffix(model, "/free") {
			return errors.New("kilo.models entries must be free model IDs ending in :free or /free")
		}
		seen[model] = true
		models = append(models, model)
	}
	k.Models = models
	seen = make(map[string]bool, len(k.DataSharingIDs))
	shared := make([]string, 0, len(k.DataSharingIDs))
	for _, id := range k.DataSharingIDs {
		id = strings.TrimSpace(id)
		if id == "" || seen[id] {
			continue
		}
		seen[id] = true
		shared = append(shared, id)
	}
	k.DataSharingIDs = shared
	return nil
}

// Enabled reports whether a Kilo allowlist was configured.
func (k KiloConfig) Enabled() bool { return len(k.Models) > 0 }

// HasModel reports whether model is explicitly assigned to the Kilo lane.
func (k KiloConfig) HasModel(model string) bool {
	for _, id := range k.Models {
		if id == model {
			return true
		}
	}
	return false
}
