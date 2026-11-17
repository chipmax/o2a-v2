package config

import "testing"

// TestPromptEnvironmentNormalize covers the contract the free-tier gate relies
// on: values are trimmed, windows folds to win32, and text that could break the
// single <env> pair is refused at load time rather than becoming a 429.
func TestPromptEnvironmentNormalize(t *testing.T) {
	yes := true
	cfg := minimalConfig()
	cfg.Upstream.Go = "https://opencode.ai/zen/go"
	cfg.Logging = LoggingConfig{Level: "info", RingSize: 1000}
	cfg.Upstream.PromptEnvironment = &PromptEnvironmentConfig{
		WorkingDirectory: "  C:/work/repo  ",
		WorkspaceRoot:    "C:/work",
		Platform:         " Windows ",
		IsGitRepo:        &yes,
	}
	normalized, err := Normalize("", cfg)
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	environment := normalized.Upstream.PromptEnvironment
	if environment.WorkingDirectory != "C:/work/repo" || environment.Platform != "win32" {
		t.Fatalf("normalize did not trim/fold: %+v", environment)
	}
	if environment.IsGitRepo == nil || !*environment.IsGitRepo {
		t.Fatal("is_git_repo was lost")
	}

	clone := Clone(normalized)
	*clone.Upstream.PromptEnvironment.IsGitRepo = false
	if !*normalized.Upstream.PromptEnvironment.IsGitRepo {
		t.Fatal("Clone shares is_git_repo with the original config")
	}
}

// TestDeadModelsDefaultAndExplicitEmpty pins the omit-vs-empty contract: an
// absent field seeds the two retired Zen IDs, while an explicit [] disables the
// filter so an operator can re-enable an ID after upstream restores it.
func TestDeadModelsDefaultAndExplicitEmpty(t *testing.T) {
	cfg := minimalConfig()
	cfg.Upstream.Go = "https://opencode.ai/zen/go"
	cfg.Logging = LoggingConfig{Level: "info", RingSize: 1000}
	normalized, err := Normalize("", cfg)
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if len(normalized.Models.DeadIDs) != len(DefaultDeadModels) {
		t.Fatalf("default dead IDs not applied: %v", normalized.Models.DeadIDs)
	}

	cfg.Models.DeadIDs = []string{}
	normalized, err = Normalize("", cfg)
	if err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if len(normalized.Models.DeadIDs) != 0 {
		t.Fatalf("explicit empty list did not disable the filter: %v", normalized.Models.DeadIDs)
	}
}

func TestPromptEnvironmentRejectsUnsafeValues(t *testing.T) {
	cases := map[string]PromptEnvironmentConfig{
		"control":   {WorkingDirectory: "bad\ndir"},
		"angle":     {WorkspaceRoot: "<env>"},
		"separator": {WorkingDirectory: "bad\u2028dir"},
		"platform":  {Platform: "freebsd"},
	}
	for name, environment := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := minimalConfig()
			cfg.Upstream.PromptEnvironment = &environment
			if _, err := Normalize("", cfg); err == nil {
				t.Fatal("Normalize accepted an unsafe prompt environment")
			}
		})
	}
}
