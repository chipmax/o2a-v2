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

const keyPoolTestModel = "deepseek-v4.1-flash-free"

// keyPoolGateway builds a Zen-key gateway aimed at upstream.
func keyPoolGateway(t *testing.T, upstream string, keys ...string) *Gateway {
	t.Helper()
	cfg := config.Config{
		ServerKeys: []string{"test-key"},
		ZenKeys:    keys,
		Prefer:     config.TierZen,
		Upstream:   config.UpstreamConfig{Zen: upstream},
		Retry:      config.RetryConfig{MaxAttempts: 4, TimeoutSeconds: 10},
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
	g.catalog.Replace([]string{keyPoolTestModel}, nil)
	return g
}

func keyPoolCall(t *testing.T, g *Gateway) (*http.Response, error, int) {
	t.Helper()
	route := modelcatalog.Route{ID: keyPoolTestModel, Tier: config.TierZen, Protocol: wire.Chat}
	bodies := map[config.Tier][]byte{
		config.TierZen: []byte(`{"model":"` + keyPoolTestModel + `","messages":[{"role":"user","content":"hi"}]}`),
	}
	return g.doKeyUpstream(context.Background(), route, bodies, identity.RequestIDs{Session: "key-pool-test"}, 0)
}

// TestQuarantineOnCredentialRejection covers the production incident the
// quarantine exists for: a key the upstream rejects must leave rotation instead
// of returning every cooldown, and once every key is out the tier answers from
// its own state instead of forwarding a 401.
func TestQuarantineOnCredentialRejection(t *testing.T) {
	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"error":{"type":"invalid_credential","message":"Invalid credential"}}`)
	}))
	defer upstream.Close()

	g := keyPoolGateway(t, upstream.URL, "key-one")
	resp, err, attempts := keyPoolCall(t, g)
	if !errors.Is(err, errKeyPoolCredentialExhausted) {
		t.Fatalf("err = %v, want errKeyPoolCredentialExhausted (response %+v)", err, resp)
	}
	if attempts != 1 || calls.Load() != 1 {
		t.Fatalf("attempts = %d, calls = %d, want 1 each", attempts, calls.Load())
	}
	node := g.zenNodes.nodes[0]
	if node.QuarantinedUntil().IsZero() || !node.QuarantinedUntil().After(time.Now().Add(23*time.Hour)) {
		t.Fatalf("quarantine = %v, want the 24h default", node.QuarantinedUntil())
	}

	// The second request is answered by the pre-check: zero upstream calls.
	resp, err, attempts = keyPoolCall(t, g)
	if !errors.Is(err, errKeyPoolCredentialExhausted) {
		t.Fatalf("second err = %v, want the credential sentinel", err)
	}
	if attempts != 0 || calls.Load() != 1 {
		t.Fatalf("pre-check attempts = %d, calls = %d, want 0 and 1", attempts, calls.Load())
	}
	if retryAfter, ok := keyPoolRetryAfter(err); !ok || retryAfter <= 0 {
		t.Fatalf("retry hint = %v, %v; want a positive horizon", retryAfter, ok)
	}
}

// TestQuotaPrecheckBlocksAFullyCoolingPool pins the zero-call 429: a tier with
// no usable key must not spend an attempt to learn what it already knows.
func TestQuotaPrecheckBlocksAFullyCoolingPool(t *testing.T) {
	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusOK)
	}))
	defer upstream.Close()

	g := keyPoolGateway(t, upstream.URL, "key-one")
	g.zenNodes.nodes[0].cooldownUntil.Store(time.Now().Add(time.Minute).UnixNano())

	_, err, attempts := keyPoolCall(t, g)
	if !errors.Is(err, errKeyPoolCooling) {
		t.Fatalf("err = %v, want errKeyPoolCooling", err)
	}
	if attempts != 0 || calls.Load() != 0 {
		t.Fatalf("attempts = %d, calls = %d, want 0 each", attempts, calls.Load())
	}
	if retryAfter, ok := keyPoolRetryAfter(err); !ok || retryAfter <= 0 {
		t.Fatalf("retry hint = %v, %v; want a positive horizon", retryAfter, ok)
	}
}

// TestQuotaCooldownDoesNotShortenQuarantine pins the interaction between the
// two failure classes: a 429 with a one-second hint must not put a rejected
// credential back in rotation early.
func TestQuotaCooldownDoesNotShortenQuarantine(t *testing.T) {
	g := keyPoolGateway(t, "http://127.0.0.1:1", "key-one")
	node := g.zenNodes.nodes[0]
	g.zenNodes.MarkFailure(node, &http.Response{StatusCode: http.StatusUnauthorized}, nil)
	horizon := node.credUntil.Load()
	if horizon == 0 {
		t.Fatal("401 did not quarantine the key")
	}
	short := &http.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{"Retry-After": []string{"1"}}}
	g.zenNodes.MarkFailure(node, short, nil)
	if got := node.credUntil.Load(); got != horizon {
		t.Fatalf("credUntil = %d, want %d unchanged", got, horizon)
	}
	if got := node.cooldownUntil.Load(); got != horizon {
		t.Fatalf("cooldownUntil = %d, want the quarantine horizon %d", got, horizon)
	}
}

// TestCursorSkipsQuarantinedKeys pins that a rejected key is not a candidate
// at any priority, while a healthy sibling still is.
func TestCursorSkipsQuarantinedKeys(t *testing.T) {
	g := keyPoolGateway(t, "http://127.0.0.1:1", "key-one", "key-two")
	quarantined, healthy := g.zenNodes.nodes[0], g.zenNodes.nodes[1]
	g.zenNodes.MarkFailure(quarantined, &http.Response{StatusCode: http.StatusForbidden}, nil)

	cursor := g.zenNodes.Cursor()
	for attempt := 0; attempt < 2; attempt++ {
		if picked := cursor.Next(); picked != healthy {
			t.Fatalf("attempt %d picked %v, want the healthy key", attempt, picked)
		}
	}
	if all, _ := g.zenNodes.AllQuarantined(time.Now()); all {
		t.Fatal("AllQuarantined = true with one healthy key left")
	}
	g.zenNodes.MarkSuccess(quarantined)
	if !quarantined.QuarantinedUntil().IsZero() {
		t.Fatal("MarkSuccess did not clear the quarantine")
	}
}

// TestRegionBlockStopsKeyRotation pins the geo-block rule: a model the
// upstream refuses for the country is not fixed by another key, so the tier
// must stop after the first answer instead of burning every credential.
func TestRegionBlockStopsKeyRotation(t *testing.T) {
	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.WriteHeader(http.StatusForbidden)
		_, _ = io.WriteString(w, `{"error":{"type":"invalid_request_error","message":"This model is not available in your country."}}`)
	}))
	defer upstream.Close()

	g := keyPoolGateway(t, upstream.URL, "key-one", "key-two", "key-three")
	_, err, attempts := keyPoolCall(t, g)
	if !errors.Is(err, errModelUnavailableInRegion) {
		t.Fatalf("err = %v, want errModelUnavailableInRegion", err)
	}
	if attempts != 1 || calls.Load() != 1 {
		t.Fatalf("attempts = %d, calls = %d, want 1 each", attempts, calls.Load())
	}
}

// TestInflightGateRefusesOverflow pins the admission gate: with no queue wait,
// a request that finds every slot taken answers 429 gateway_busy with a
// Retry-After instead of piling another turn onto a saturated gateway.
func TestInflightGateRefusesOverflow(t *testing.T) {
	g := keyPoolGateway(t, "http://127.0.0.1:1", "key-one")
	g.inflight = make(chan struct{}, 1)
	g.queueWait = 0
	g.inflight <- struct{}{}

	called := false
	handler := g.gated(wire.Chat, func(http.ResponseWriter, *http.Request) { called = true })
	recorder := httptest.NewRecorder()
	handler(recorder, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))
	if called {
		t.Fatal("handler ran while the only slot was occupied")
	}
	if recorder.Code != http.StatusTooManyRequests {
		t.Fatalf("status = %d, want 429", recorder.Code)
	}
	if got := recorder.Header().Get("Retry-After"); got != "5" {
		t.Fatalf("Retry-After = %q, want 5", got)
	}

	<-g.inflight
	handler(recorder, httptest.NewRequest(http.MethodPost, "/v1/chat/completions", nil))
	if !called {
		t.Fatal("handler did not run after the slot was released")
	}
}

// TestWallFastFailNeedsAUsableKeyFallback pins the rule that separates a
// spendthrift walk from a correct one: capping the anonymous exit pool to a
// single probe is only safe when an authenticated key can answer. With no key
// (or every key cooling), the exits are the lane and must keep their cap.
func TestWallFastFailNeedsAUsableKeyFallback(t *testing.T) {
	g := keyPoolGateway(t, "http://127.0.0.1:1", "key-one")
	route := modelcatalog.Route{ID: keyPoolTestModel, Tier: config.TierZen, Protocol: wire.Chat, Anonymous: true}

	if g.hasUsableKeyFallback(route) {
		t.Fatal("a route without key tiers reported a usable key fallback")
	}
	route.KeyTiers = []config.Tier{config.TierZen}
	if !g.hasUsableKeyFallback(route) {
		t.Fatal("a fresh key must count as a fallback")
	}
	g.zenNodes.nodes[0].cooldownUntil.Store(time.Now().Add(time.Minute).UnixNano())
	if g.hasUsableKeyFallback(route) {
		t.Fatal("a cooling key must not count as a fallback")
	}
	g.zenNodes.nodes[0].cooldownUntil.Store(0)
	g.zenNodes.MarkFailure(g.zenNodes.nodes[0], &http.Response{StatusCode: http.StatusUnauthorized}, nil)
	if g.hasUsableKeyFallback(route) {
		t.Fatal("a quarantined key must not count as a fallback")
	}
}

// TestQuotaDocumentCoolsTheKey pins the accounting fix: a 200 carrying the
// free tier's quota document must reach the pool as a failure. Treating it as
// a success cleared the cooldown and handed the next request the same document.
func TestQuotaDocumentCoolsTheKey(t *testing.T) {
	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		calls.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"FreeUsageLimitError","message":"Free usage limit reached"}}`)
	}))
	defer upstream.Close()

	g := keyPoolGateway(t, upstream.URL, "key-one")
	// The tier builds the retryable sentinel from the response it keeps; this
	// test only needs the pool accounting, which is what looked like a success.
	_, _, attempts := keyPoolCall(t, g)
	if attempts == 0 {
		t.Fatal("no attempt was made")
	}
	node := g.zenNodes.nodes[0]
	if node.failures.Load() == 0 {
		t.Fatal("quota document left the key's failure count at zero")
	}
	if node.cooldownUntil.Load() <= time.Now().UnixNano() {
		t.Fatal("quota document did not cool the key")
	}
	if calls.Load() == 0 {
		t.Fatal("no upstream call was made")
	}
}
