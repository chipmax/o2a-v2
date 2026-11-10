package config

import "testing"

func TestNormalizeContextOverrides(t *testing.T) {
	cfg := &Config{Models: ModelsConfig{ContextOverrides: map[string]ModelLimits{
		" deepseek-v4.1-flash-free ": {Context: 1000000},
		"beam-free":                  {Output: 131072},
	}}}
	if err := normalizeContextOverrides(cfg); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	got := cfg.Models.ContextOverrides
	if len(got) != 2 {
		t.Fatalf("want 2 entries, got %d: %#v", len(got), got)
	}
	if limit := got["deepseek-v4.1-flash-free"]; limit.Context != 1000000 {
		t.Fatalf("trimmed key missing: %#v", got)
	}
	if limit := got["beam-free"]; limit.Output != 131072 || limit.Context != 0 {
		t.Fatalf("beam-free: %#v", limit)
	}
}

func TestNormalizeContextOverridesRejects(t *testing.T) {
	cases := []struct {
		name   string
		limits map[string]ModelLimits
	}{
		{"empty id", map[string]ModelLimits{"  ": {Context: 1}}},
		{"overrides nothing", map[string]ModelLimits{"m": {}}},
		{"context too large", map[string]ModelLimits{"m": {Context: 20000000}}},
		{"negative output", map[string]ModelLimits{"m": {Output: -1}}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			cfg := &Config{Models: ModelsConfig{ContextOverrides: tc.limits}}
			if err := normalizeContextOverrides(cfg); err == nil {
				t.Fatalf("want error for %s", tc.name)
			}
		})
	}
}

func TestNormalizeContextOverridesClearsEmpty(t *testing.T) {
	cfg := &Config{}
	if err := normalizeContextOverrides(cfg); err != nil {
		t.Fatalf("normalize: %v", err)
	}
	if cfg.Models.ContextOverrides != nil {
		t.Fatalf("want nil, got %#v", cfg.Models.ContextOverrides)
	}
}

func TestCloneDeepCopiesContextOverrides(t *testing.T) {
	cfg := Config{Models: ModelsConfig{ContextOverrides: map[string]ModelLimits{"m": {Context: 400000}}}}
	clone := Clone(cfg)
	limit := clone.Models.ContextOverrides["m"]
	limit.Context = 1
	clone.Models.ContextOverrides["m"] = limit
	if cfg.Models.ContextOverrides["m"].Context != 400000 {
		t.Fatalf("Clone shares the override map: %#v", cfg.Models.ContextOverrides)
	}
}
