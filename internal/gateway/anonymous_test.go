package gateway

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"opencode2api/internal/config"
	"opencode2api/internal/identity"
	modelcatalog "opencode2api/internal/models"
	wire "opencode2api/internal/protocol"
)

func TestAnonymousQuotaDocumentIsRetiredAndReported(t *testing.T) {
	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"FreeUsageLimitError","message":"Free usage limit reached"}}`)
	}))
	defer upstream.Close()

	cfg := config.Config{
		ServerKeys: []string{"test-key"},
		Anonymous:  true,
		Proxies:    []string{"direct"},
		Upstream:   config.UpstreamConfig{Anonymous: upstream.URL},
		Retry:      config.RetryConfig{MaxAttempts: 3, TimeoutSeconds: 10},
		Performance: config.PerformanceConfig{
			MaxIdleConns: 16, MaxIdleConnsPerHost: 8, MaxConnsPerHost: 0,
			IdleConnTimeoutSeconds: 30, ConnectTimeoutSeconds: 2, FailureCooldownSeconds: 1,
		},
	}
	g, err := New(cfg, slog.New(slog.NewTextHandler(io.Discard, nil)), nil, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	const model = "deepseek-v4.1-flash-free"
	g.catalog.Replace([]string{model}, nil)
	route := modelcatalog.Route{ID: model, Tier: config.TierZen, Protocol: wire.Chat, Anonymous: true}
	bodies := map[config.Tier][]byte{
		config.TierZen: []byte(`{"model":"` + model + `","messages":[{"role":"user","content":"hi"}]}`),
	}

	resp, err, attempts := g.doAnonymousUpstream(context.Background(), route, bodies, identity.RequestIDs{Session: "quota-document"}, 0)
	if !errors.Is(err, errFreeTierQuotaSpent) {
		t.Fatalf("error = %v, want errFreeTierQuotaSpent", err)
	}
	if resp == nil || resp.StatusCode != http.StatusOK {
		t.Fatalf("response = %+v, want the upstream 200 document kept for classification", resp)
	}
	if calls.Load() != 1 || attempts != 1 {
		t.Fatalf("calls = %d, attempts = %d, want 1 each", calls.Load(), attempts)
	}
	node := g.anonymous.nodes[0]
	if !node.quotaCooldown.Load() || node.cooldownUntil.Load() == 0 {
		t.Fatal("the exit was not retired until the quota window resets")
	}
}

func TestAnonymousWallFastFailsPerFamily(t *testing.T) {
	nodes := &anonymousNodes{}
	now := time.Now()
	if nodes.noteWall("deepseek-v4.1-flash-free", now) {
		t.Fatal("one 429 must not open the wall")
	}
	if !nodes.noteWall("deepseek-v4.1-flash-free", now.Add(time.Second)) {
		t.Fatal("the second 429 in the window did not open the wall")
	}
	if !nodes.wallActive("deepseek-v4.1-flash-free", now.Add(2*time.Second)) {
		t.Fatal("the wall is not active for the affected family")
	}
	if nodes.wallActive("glm-5.3-free", now.Add(2*time.Second)) {
		t.Fatal("a wall for one family leaked into another family")
	}
	if got := nodes.wallProbeTimeout("deepseek-v4.1-flash-free", 30*time.Second); got != anonymousWallProbeTimeout {
		t.Fatalf("probe timeout = %s, want %s", got, anonymousWallProbeTimeout)
	}
	if got := nodes.wallProbeTimeout("deepseek-v4.1-flash-free", 3*time.Second); got != 3*time.Second {
		t.Fatalf("probe timeout = %s, want the shorter configured value", got)
	}
	nodes.clearWall("deepseek-v4.1-flash-free")
	if nodes.wallActive("deepseek-v4.1-flash-free", now.Add(2*time.Second)) {
		t.Fatal("a successful attempt did not clear the wall")
	}
}
