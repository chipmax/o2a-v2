package gateway

import (
	"opencode2api/internal/agentcfg"
)

// AgentModels lists every servable model with the same advertised context and
// modalities /v1/models reports, so generated harness configs cannot drift
// from what the gateway actually announces.
func (g *Gateway) AgentModels() []agentcfg.Model {
	routes, _ := g.availableModels()
	out := make([]agentcfg.Model, 0, len(routes))
	seen := make(map[string]bool, len(routes))
	for _, route := range routes {
		if seen[route.ID] {
			continue
		}
		seen[route.ID] = true
		metadata := g.catalog.MetadataForTier(route.ID, route.Tier)
		metadata = g.modelLimits(route.ID, route.Tier, metadata)
		out = append(out, agentcfg.Model{
			ID: route.ID, Context: metadata.ContextWindow, Output: metadata.MaxOutput,
			Reasoning: metadata.Reasoning, Input: metadata.InputModalities,
		})
	}
	return out
}
