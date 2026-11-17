package models

import (
	"testing"

	"opencode2api/internal/config"
	wire "opencode2api/internal/protocol"
)

// TestDeadModelsAreHiddenEverywhere guards the retired-ID filter: a stale disk
// cache can still advertise a model upstream no longer serves, so discovery,
// snapshot counts and direct routes must all agree it is gone.
func TestDeadModelsAreHiddenEverywhere(t *testing.T) {
	catalog := Catalog(config.TierZen, nil)
	catalog.ReplaceWithCapabilities(
		[]string{"live-model", "longcat-2.5-preview-free", "nemotron-3-ultra-free"},
		nil,
		map[config.Tier]map[string]wire.Protocol{
			config.TierZen: {
				"live-model":               wire.Chat,
				"longcat-2.5-preview-free": wire.Chat,
				"nemotron-3-ultra-free":    wire.Chat,
			},
		},
		nil, nil,
	)
	catalog.SetDeadModels([]string{"longcat-2.5-preview-free", "nemotron-3-ultra-free", "  "})

	routes, snapshot := catalog.AvailableModels(true, false, true)
	if len(routes) != 1 || routes[0].ID != "live-model" {
		t.Fatalf("discovery still exposes retired models: %+v", routes)
	}
	if snapshot.Total != 1 || snapshot.Exposed != 1 || snapshot.Zen != 1 {
		t.Fatalf("snapshot counts include retired models: %+v", snapshot)
	}
	if _, err := catalog.Route("longcat-2.5-preview-free", true, false, true); err == nil {
		t.Fatal("a retired model is still routable")
	}

	catalog.SetDeadModels(nil)
	if _, err := catalog.Route("longcat-2.5-preview-free", true, false, true); err != nil {
		t.Fatalf("clearing the denylist did not restore routing: %v", err)
	}
}
