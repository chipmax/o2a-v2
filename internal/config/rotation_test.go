package config

import "testing"

func TestRotationAllowsCuratedSubsetAndMissingModels(t *testing.T) {
	cfg := RotationConfig{
		SOTA:               RotationGroup{Alias: "sota", Order: []string{"only-one", "gone-upstream"}},
		Sweet:              RotationGroup{Alias: "sweet"},
		FirstOutputSeconds: 30,
	}
	if err := cfg.Validate([]string{"only-one", "new-model"}); err != nil {
		t.Fatalf("a curated order with a missing model was rejected: %v", err)
	}
}
