package gateway

import (
	"io"
	"log/slog"
	"testing"
	"time"

	"opencode2api/internal/config"
	wire "opencode2api/internal/protocol"
)

// previewGateway builds a gateway whose catalog knows the test model, with the
// requested keys and anonymous lane.
func previewGateway(t *testing.T, anonymous bool, keys ...string) *Gateway {
	t.Helper()
	cfg := config.Config{
		ServerKeys: []string{"test-key"},
		ZenKeys:    keys,
		Anonymous:  anonymous,
		Proxies:    []string{"direct"},
		Prefer:     config.TierZen,
		Upstream:   config.UpstreamConfig{Zen: "http://127.0.0.1:1", Anonymous: "http://127.0.0.1:1"},
		Retry:      config.RetryConfig{MaxAttempts: 2, TimeoutSeconds: 10},
		Models:     config.ModelsConfig{RefreshSeconds: 300, FreeOnly: true},
		Performance: config.PerformanceConfig{
			MaxIdleConns: 16, MaxIdleConnsPerHost: 8,
			IdleConnTimeoutSeconds: 30, ConnectTimeoutSeconds: 2, FailureCooldownSeconds: 1,
		},
	}
	g, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	// A live refresh always carries the native protocol per tier; without it
	// the catalog treats a listed model as unsupported.
	g.catalog.ReplaceWithCapabilities(
		[]string{keyPoolTestModel}, nil,
		map[config.Tier]map[string]wire.Protocol{config.TierZen: {keyPoolTestModel: wire.Chat}},
		nil, nil,
	)
	return g
}

// TestPreviewAnonymousFirst pins the honest plan for a free model on an
// anonymous-first deployment: the lane is tried, and Ready is true because a
// request could succeed right now.
func TestPreviewAnonymousFirst(t *testing.T) {
	g := previewGateway(t, true)
	plan := g.PreviewRoute(keyPoolTestModel, wire.Chat)
	if !plan.Ready {
		t.Fatalf("Ready = false, blockers %v", plan.Blockers)
	}
	if len(plan.Steps) == 0 || plan.Steps[0].Lane != "anonymous" {
		t.Fatalf("steps = %+v, want the anonymous lane first", plan.Steps)
	}
	if plan.Steps[0].Verdict != verdictTry {
		t.Fatalf("anonymous verdict = %q, want try", plan.Steps[0].Verdict)
	}
	if plan.Transport == "" {
		t.Fatal("plan reported no transport")
	}
	if plan.Protocol != string(wire.Chat) {
		t.Fatalf("protocol = %q", plan.Protocol)
	}
}

// TestPreviewReportsCoolingKeys pins the operator-facing why: a tier whose
// every key is cooling is skipped with the real retry horizon, and the plan is
// not Ready.
func TestPreviewReportsCoolingKeys(t *testing.T) {
	g := previewGateway(t, false, "key-one")
	g.zenNodes.nodes[0].cooldownUntil.Store(time.Now().Add(90 * time.Second).UnixNano())

	plan := g.PreviewRoute(keyPoolTestModel, wire.Chat)
	if plan.Ready {
		t.Fatal("Ready = true while every key is cooling")
	}
	var key *PreviewStep
	for index := range plan.Steps {
		if plan.Steps[index].Lane == "key" {
			key = &plan.Steps[index]
		}
	}
	if key == nil {
		t.Fatalf("steps = %+v, want a key step", plan.Steps)
	}
	if key.Verdict != verdictSkip || key.CoolingKeys != 1 || key.RetryAfter <= 0 {
		t.Fatalf("key step = %+v, want skip with a retry hint", *key)
	}
	if len(plan.Blockers) == 0 {
		t.Fatal("no blocker explained the skip")
	}
}

// TestPreviewUnknownModel pins the route error surface: an unexposed model is
// a blocker, never a fake plan.
func TestPreviewUnknownModel(t *testing.T) {
	g := previewGateway(t, true)
	plan := g.PreviewRoute("no-such-model-free", wire.Chat)
	if plan.Ready {
		t.Fatal("Ready = true for an unexposed model")
	}
	if len(plan.Blockers) == 0 {
		t.Fatal("no blocker for an unexposed model")
	}
	if len(plan.Steps) != 0 {
		t.Fatalf("steps = %+v, want none for a model with no route", plan.Steps)
	}
	if plan.Note == "" || plan.GeneratedAt.IsZero() {
		t.Fatalf("plan = %+v, want a note and a timestamp", plan)
	}
}
