package gateway

import (
	"net/http"
	"net/http/httptest"
	"testing"

	"opencode2api/internal/config"
	modelcatalog "opencode2api/internal/models"
)

func TestModelLimitsFollowsObservations(t *testing.T) {
	store, _ := modelcatalog.OpenContextLimits("")
	g := &Gateway{
		cfg: config.Config{Models: config.ModelsConfig{ContextOverrides: map[string]config.ModelLimits{
			"m": {Context: 320_000},
		}}},
		contextLimits: store,
	}
	md := g.modelLimits("m", config.TierGo, modelcatalog.Metadata{ContextWindow: 200_000})
	if md.ContextWindow != 320_000 {
		t.Fatalf("seeded context = %d, want 320000", md.ContextWindow)
	}
	store.ObserveSuccess("m", config.TierGo, 330_055, 1_500_000)
	md = g.modelLimits("m", config.TierGo, modelcatalog.Metadata{ContextWindow: 200_000})
	if md.ContextWindow != 320_000 {
		t.Fatalf("observed context must not undercut the seed: got %d", md.ContextWindow)
	}

	g.cfg.Models.ContextOverrides = nil
	md = g.modelLimits("m", config.TierGo, modelcatalog.Metadata{ContextWindow: 200_000})
	if md.ContextWindow != 313_000 {
		t.Fatalf("learned context = %d, want 313000", md.ContextWindow)
	}
	md = g.modelLimits("m", config.TierZen, modelcatalog.Metadata{ContextWindow: 200_000})
	if md.ContextWindow != 200_000 {
		t.Fatalf("tier leaked: zen context = %d, want 200000", md.ContextWindow)
	}
}

func TestModelLimitsAppliesOverridesWithoutStore(t *testing.T) {
	g := &Gateway{cfg: config.Config{Models: config.ModelsConfig{ContextOverrides: map[string]config.ModelLimits{
		"m": {Context: 300_000, Input: 250_000, Output: 64_000},
	}}}}
	md := g.modelLimits("m", config.TierGo, modelcatalog.Metadata{ContextWindow: 200_000, MaxInput: 200_000, MaxOutput: 32_000})
	if md.ContextWindow != 300_000 || md.MaxInput != 250_000 || md.MaxOutput != 64_000 {
		t.Fatalf("override not applied: %+v", md)
	}
}

func TestObserveContextRejectionLeavesResponseClean(t *testing.T) {
	store, _ := modelcatalog.OpenContextLimits("")
	g := &Gateway{
		cfg: config.Config{Models: config.ModelsConfig{ContextOverrides: map[string]config.ModelLimits{
			"m": {Context: 320_000},
		}}},
		contextLimits: store,
		catalog:       modelcatalog.Catalog(config.TierZen, nil),
	}
	rec := httptest.NewRecorder()
	advertised, applied := g.observeContextRejection("m", config.TierZen, http.StatusBadRequest, []byte(`{"error":{"message":"maximum context length is 330000 tokens"}}`), 0)
	if advertised != 313_000 || !applied {
		t.Fatalf("advertised = %d, applied = %v, want 313000 true", advertised, applied)
	}
	if got := rec.Header().Get("X-Context-Limit"); got != "" {
		t.Fatalf("observation leaked a header: %q", got)
	}
}

func TestIsContextLengthResponse(t *testing.T) {
	cases := []struct {
		name   string
		status int
		body   string
		want   bool
	}{
		{"explicit limit", http.StatusBadRequest, `{"error":{"message":"maximum context length is 330000 tokens"}}`, true},
		{"free tier wording", http.StatusBadRequest, `{"error":{"message":"prompt is too long: 331001 tokens > 330000 maximum"}}`, true},
		{"context window", http.StatusUnprocessableEntity, `{"error":{"message":"context window exceeded"}}`, true},
		{"payload too large", http.StatusRequestEntityTooLarge, `{"error":{"message":"prompt is too long"}}`, true},
		{"unrelated 400", http.StatusBadRequest, `{"error":{"message":"invalid request"}}`, false},
		{"context text on 500", http.StatusInternalServerError, `{"error":{"message":"maximum context length exceeded"}}`, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := isContextLengthResponse(tc.status, []byte(tc.body)); got != tc.want {
				t.Fatalf("isContextLengthResponse = %v, want %v", got, tc.want)
			}
		})
	}
}
