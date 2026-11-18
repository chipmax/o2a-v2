package models

import (
	"opencode2api/internal/config"
	wire "opencode2api/internal/protocol"
)

// SetKiloModels installs the Kilo free-model allowlist. The lane is separate
// from Zen/Go: a Kilo model must never inherit an OpenCode key, WARP exit or
// Bun fingerprint, and an OpenCode model must never fall back to Kilo.
func (c *ModelCatalog) SetKiloModels(models []string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.kilo == nil {
		c.kilo = make(map[string]bool, len(models))
	}
	for id := range c.kilo {
		delete(c.kilo, id)
	}
	for _, id := range models {
		c.kilo[id] = true
	}
}

// KiloModels returns a copy of the Kilo allowlist.
func (c *ModelCatalog) KiloModels() []string {
	c.mu.RLock()
	defer c.mu.RUnlock()
	out := make([]string, 0, len(c.kilo))
	for id := range c.kilo {
		out = append(out, id)
	}
	return out
}

// IsKilo reports whether model is explicitly assigned to the Kilo lane.
func (c *ModelCatalog) IsKilo(model string) bool {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.kilo[model]
}

// KiloMetadata returns the last live discovery metadata for a Kilo model and
// whether the model was flagged as routing to data-sharing providers.
func (c *ModelCatalog) KiloMetadata(model string) (Metadata, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return c.kiloMeta[model], c.kiloShared[model]
}

// SetKiloMetadata installs the live catalog metadata alongside the allowlist.
func (c *ModelCatalog) SetKiloMetadata(items []KiloModel) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.kiloMeta == nil {
		c.kiloMeta = make(map[string]Metadata, len(items))
	}
	if c.kiloShared == nil {
		c.kiloShared = make(map[string]bool, len(items))
	}
	for id := range c.kiloMeta {
		delete(c.kiloMeta, id)
	}
	for id := range c.kiloShared {
		delete(c.kiloShared, id)
	}
	for _, item := range items {
		c.kiloMeta[item.ID] = Metadata{ContextWindow: item.Context, MaxOutput: item.MaxOutput}
		c.kiloShared[item.ID] = item.DataSharing
	}
}

// KiloRoute builds the route for a Kilo model. It is Chat-only on the wire:
// the gateway converts other client protocols to and from Chat around it.
func KiloRoute(model string) Route {
	return Route{
		ID: model, Tier: config.TierKilo, Protocol: wire.Chat,
		Protocols: map[config.Tier]wire.Protocol{config.TierKilo: wire.Chat},
	}
}
