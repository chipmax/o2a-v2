package gateway

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"opencode2api/internal/config"
	modelcatalog "opencode2api/internal/models"
	wire "opencode2api/internal/protocol"
	"opencode2api/internal/rotation"
)

func resetGlobalQuota(t *testing.T) {
	t.Helper()
	quota.mu.Lock()
	quota.state = map[string]*keyQuotaState{}
	quota.path = ""
	quota.lastSave = time.Time{}
	quota.mu.Unlock()
}

func TestQuotaUnlimitedIsNoop(t *testing.T) {
	q := &keyQuota{state: map[string]*keyQuotaState{}}
	release, refusal := q.acquire("id", config.ServerKeyMetadata{})
	if refusal != nil {
		t.Fatalf("unlimited key refused: %+v", refusal)
	}
	release()
	release()
}

func TestQuotaConcurrencyLimit(t *testing.T) {
	q := &keyQuota{state: map[string]*keyQuotaState{}}
	meta := config.ServerKeyMetadata{MaxConcurrency: 1}
	release, refusal := q.acquire("id", meta)
	if refusal != nil {
		t.Fatalf("first request refused: %+v", refusal)
	}
	if _, refusal := q.acquire("id", meta); refusal == nil {
		t.Fatal("second concurrent request was admitted")
	} else if refusal.code != "key_concurrency_limit" {
		t.Fatalf("unexpected code %q", refusal.code)
	}
	release()
	if _, refusal := q.acquire("id", meta); refusal != nil {
		t.Fatalf("slot was not released: %+v", refusal)
	}
}

func TestQuotaDailyLimit(t *testing.T) {
	q := &keyQuota{state: map[string]*keyQuotaState{}}
	meta := config.ServerKeyMetadata{DailyRequests: 2}
	for i := 0; i < 2; i++ {
		release, refusal := q.acquire("id", meta)
		if refusal != nil {
			t.Fatalf("request %d refused early: %+v", i+1, refusal)
		}
		release()
	}
	if _, refusal := q.acquire("id", meta); refusal == nil {
		t.Fatal("third request was admitted past the daily limit")
	} else if refusal.code != "key_daily_limit" || refusal.retryAfter <= 0 {
		t.Fatalf("unexpected refusal %+v", refusal)
	}

	// A stored counter from an earlier UTC day starts fresh.
	q.mu.Lock()
	q.state["id"].day = "2000-01-01"
	q.mu.Unlock()
	release, refusal := q.acquire("id", meta)
	if refusal != nil {
		t.Fatalf("new day still limited: %+v", refusal)
	}
	release()
}

func TestQuotaRefundReturnsDailySlot(t *testing.T) {
	q := &keyQuota{state: map[string]*keyQuotaState{}}
	meta := config.ServerKeyMetadata{DailyRequests: 1}
	release, refusal := q.acquire("id", meta)
	if refusal != nil {
		t.Fatalf("first request refused: %+v", refusal)
	}
	release()
	q.refund("id")
	release, refusal = q.acquire("id", meta)
	if refusal != nil {
		t.Fatalf("refunded slot was not reusable: %+v", refusal)
	}
	release()
}

func TestListingModelsDoesNotConsumeQuota(t *testing.T) {
	resetGlobalQuota(t)
	key := "quota-listing-test-key"
	cfg := config.Config{
		ServerKeys: []string{key},
		ServerKeyMetadata: map[string]config.ServerKeyMetadata{
			config.Fingerprint(key): {DailyRequests: 1},
		},
	}
	g := &Gateway{cfg: cfg, logger: slog.Default()}
	calls := 0
	handler := g.authenticate(func(w http.ResponseWriter, _ *http.Request) {
		calls++
		w.WriteHeader(http.StatusNoContent)
	})
	for i := range 3 {
		req := httptest.NewRequest(http.MethodGet, "/v1/models", nil)
		req.Header.Set("x-api-key", key)
		rec := httptest.NewRecorder()
		handler(rec, req)
		if rec.Code != http.StatusNoContent {
			t.Fatalf("request %d status = %d, want 204", i+1, rec.Code)
		}
	}
	if calls != 3 {
		t.Fatalf("handler calls = %d, want 3", calls)
	}
	release, refusal := quota.acquire(config.Fingerprint(key), cfg.Policy(key))
	if refusal != nil {
		t.Fatalf("listing consumed the daily budget: %+v", refusal)
	}
	release()
}

func TestRejectedInferenceDoesNotConsumeQuota(t *testing.T) {
	resetGlobalQuota(t)
	key := "quota-reject-test-key"
	cfg := config.Config{
		ServerKeys: []string{key},
		ServerKeyMetadata: map[string]config.ServerKeyMetadata{
			config.Fingerprint(key): {DailyRequests: 1},
		},
	}
	g := &Gateway{cfg: cfg, logger: slog.Default()}
	handler := g.authenticate(g.handleInference(wire.Chat))
	for range 2 {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader("not json"))
		req.Header.Set("x-api-key", key)
		rec := httptest.NewRecorder()
		handler(rec, req)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("status = %d, want 400", rec.Code)
		}
	}
	release, refusal := quota.acquire(config.Fingerprint(key), cfg.Policy(key))
	if refusal != nil {
		t.Fatalf("rejected bodies consumed the daily budget: %+v", refusal)
	}
	release()
}

func TestUnknownModelPeerRejectionDoesNotConsumeQuota(t *testing.T) {
	resetGlobalQuota(t)
	peer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, `{"error":{"message":"model not found"}}`)
	}))
	defer peer.Close()
	key := "quota-unknown-peer-test-key"
	cfg := config.Config{
		ServerKeys: []string{key},
		ServerKeyMetadata: map[string]config.ServerKeyMetadata{
			config.Fingerprint(key): {DailyRequests: 1},
		},
	}
	catalog := modelcatalog.Catalog(config.TierZen, nil)
	catalog.Replace([]string{"known-model"}, nil)
	g := &Gateway{
		cfg:     cfg,
		logger:  slog.Default(),
		catalog: catalog,
		peers:   newPeerPool([]config.PeerConfig{{Name: "p", BaseURL: peer.URL, Key: "k", Tier: 1}}),
	}
	g.rotation, _ = rotation.New("", config.RotationConfig{})
	handler := g.authenticate(g.handleInference(wire.Chat))
	for i := range 2 {
		req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{"model":"unknown-model","messages":[]}`))
		req.Header.Set("x-api-key", key)
		rec := httptest.NewRecorder()
		handler(rec, req)
		if rec.Code != http.StatusNotFound {
			t.Fatalf("request %d status = %d, want 404", i+1, rec.Code)
		}
	}
	release, refusal := quota.acquire(config.Fingerprint(key), cfg.Policy(key))
	if refusal != nil {
		t.Fatalf("peer model rejection consumed the daily budget: %+v", refusal)
	}
	release()
}

func TestQuotaStateSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "quota.json")
	meta := config.ServerKeyMetadata{DailyRequests: 2}

	first := &keyQuota{state: map[string]*keyQuotaState{}}
	first.configurePath(path)
	release, refusal := first.acquire("key-id", meta)
	if refusal != nil {
		t.Fatalf("first process refused: %+v", refusal)
	}
	release()
	first.save()

	second := &keyQuota{state: map[string]*keyQuotaState{}}
	second.configurePath(path)
	second.mu.Lock()
	used := second.state["key-id"].used
	second.mu.Unlock()
	if used != 1 {
		t.Fatalf("restored usage = %d, want 1", used)
	}
	release, refusal = second.acquire("key-id", meta)
	if refusal != nil {
		t.Fatalf("second process lost the restored budget: %+v", refusal)
	}
	release()
	if _, refusal := second.acquire("key-id", meta); refusal == nil {
		t.Fatal("restored daily budget was not enforced")
	}
}

func response(status int, contentType, body string) *http.Response {
	header := http.Header{}
	if contentType != "" {
		header.Set("Content-Type", contentType)
	}
	return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader(body))}
}

func TestIsQuotaErrorDocument(t *testing.T) {
	cases := []struct {
		name     string
		resp     *http.Response
		quota    bool
		bodyKept bool
	}{
		{
			name:     "200 with free usage error",
			resp:     response(200, "application/json", `{"type":"error","error":{"type":"FreeUsageLimitError","message":"Free usage limit reached"}}`),
			quota:    true,
			bodyKept: true,
		},
		{
			name:     "200 with free tier error",
			resp:     response(200, "application/json; charset=utf-8", `{"error":{"type":"FreeTierError","message":"free tier"}}`),
			quota:    true,
			bodyKept: true,
		},
		{
			name:     "200 completion",
			resp:     response(200, "application/json", `{"choices":[{"message":{"content":"hi"}}]}`),
			bodyKept: true,
		},
		{
			name:     "429 status is not a document",
			resp:     response(429, "application/json", `{"type":"error","error":{"type":"FreeUsageLimitError"}}`),
			bodyKept: true,
		},
		{
			name:     "event stream is never peeked",
			resp:     response(200, "text/event-stream", `{"type":"error","error":{"type":"FreeUsageLimitError"}}`),
			bodyKept: true,
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			body, _ := io.ReadAll(test.resp.Body)
			test.resp.Body = io.NopCloser(strings.NewReader(string(body)))
			if got := isQuotaErrorDocument(test.resp); got != test.quota {
				t.Fatalf("isQuotaErrorDocument = %v, want %v", got, test.quota)
			}
			after, _ := io.ReadAll(test.resp.Body)
			if test.bodyKept && string(after) != string(body) {
				t.Fatalf("body was consumed: got %q, want %q", after, body)
			}
		})
	}
}
