package admin

import (
	"testing"

	"opencode2api/internal/config"
)

func TestCandidateConfigPreservesPeers(t *testing.T) {
	current := config.Config{Peers: []config.PeerConfig{{
		Name: "vps1", BaseURL: "http://10.0.0.1:8080", Key: "k", Tier: 1, Models: []string{"deepseek-v4.1-flash-free"},
	}}}
	got := candidateConfig(current, ConfigUpdate{}, nil, nil, nil, nil, nil, current.Reasoning)
	if len(got.Peers) != 1 || got.Peers[0].Name != "vps1" {
		t.Fatalf("peers were dropped: %+v", got.Peers)
	}
	got.Peers[0].Models[0] = "changed"
	if current.Peers[0].Models[0] == "changed" {
		t.Fatal("candidate shares the peer model slice with the live configuration")
	}
}

func TestCandidateConfigPreservesFieldsTheUIForgets(t *testing.T) {
	current := config.Config{
		Storage: config.StorageConfig{UsageSQLite: "usage/usage.sqlite"},
		Models: config.ModelsConfig{
			ContextOverrides: map[string]config.ModelLimits{"free-model": {Context: 320000}},
			AnonymousBackoff: &config.AnonymousBackoffConfig{},
		},
	}
	got := candidateConfig(current, ConfigUpdate{}, nil, nil, nil, nil, nil, current.Reasoning)
	if got.Storage != current.Storage {
		t.Fatalf("storage was reset: %+v", got.Storage)
	}
	if got.Models.ContextOverrides["free-model"].Context != 320000 {
		t.Fatalf("context overrides were reset: %+v", got.Models.ContextOverrides)
	}
	if got.Models.AnonymousBackoff == nil {
		t.Fatal("anonymous backoff was reset")
	}
}
