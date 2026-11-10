package gateway

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"opencode2api/internal/config"
	modelcatalog "opencode2api/internal/models"
)

func testPeer(name string, tier int) config.PeerConfig {
	return config.PeerConfig{Name: name, BaseURL: "http://127.0.0.1:1", Key: "k", Tier: tier, CooldownSeconds: 60}
}

func TestPeerPoolOrdersByTierAndRotatesInsideOne(t *testing.T) {
	pool := newPeerPool([]config.PeerConfig{
		testPeer("third", 3),
		testPeer("a", 1),
		testPeer("b", 1),
	})
	first := pool.ordered("m")
	if len(first) != 3 || first[0].Tier != 1 || first[1].Tier != 1 || first[2].Name != "third" {
		t.Fatalf("ordered = %v, want the two tier-1 peers first", first)
	}
	firstNames := first[0].Name + first[1].Name
	second := pool.ordered("m")
	if second[0].Name+second[1].Name == firstNames {
		t.Fatalf("tier-1 round robin repeated the same leader: %q then %q", firstNames, second[0].Name+second[1].Name)
	}
}

func TestPeerPoolRotationKeepsTierOrder(t *testing.T) {
	pool := newPeerPool([]config.PeerConfig{testPeer("a", 1), testPeer("b", 2)})
	for i := 0; i < 6; i++ {
		ordered := pool.ordered("m")
		if len(ordered) != 2 {
			t.Fatalf("call %d returned %d peers", i, len(ordered))
		}
		if ordered[0].Tier != 1 || ordered[1].Tier != 2 {
			t.Fatalf("call %d broke tier order: %v", i, ordered)
		}
	}
}

func TestPeerPoolCoolsAndFallsBackToColdPeers(t *testing.T) {
	pool := newPeerPool([]config.PeerConfig{testPeer("only", 1)})
	header := http.Header{}
	header.Set("Retry-After", "120")
	delay := pool.cool(pool.peers[0], http.StatusTooManyRequests, header)
	if delay != 2*time.Minute {
		t.Fatalf("cool delay = %s, want the 2m Retry-After", delay)
	}
	ordered := pool.ordered("m")
	if len(ordered) != 1 || ordered[0].Name != "only" {
		t.Fatalf("a cooling peer must stay as a last resort, got %v", ordered)
	}
	pool.clear(pool.peers[0])
	if snapshot := pool.peerSnapshot(); snapshot[0]["cooling_seconds"] != nil {
		t.Fatalf("clear left a cooldown: %v", snapshot[0])
	}
}

func TestPeerPoolHonoursModelAllowList(t *testing.T) {
	peer := testPeer("restricted", 1)
	peer.Models = []string{"deepseek-v4.1-flash-free"}
	pool := newPeerPool([]config.PeerConfig{peer})
	if got := pool.ordered("other-model"); len(got) != 0 {
		t.Fatalf("ordered returned %d peers for a model outside the allow list", len(got))
	}
	if got := pool.ordered("deepseek-v4.1-flash-free"); len(got) != 1 {
		t.Fatalf("ordered returned %d peers for an allowed model", len(got))
	}
}

func TestPeerStreamOutlivesHeaderTimeout(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		_, _ = io.WriteString(w, "data: a\n\n")
		flusher.Flush()
		time.Sleep(1200 * time.Millisecond)
		_, _ = io.WriteString(w, "data: b\n\n")
		flusher.Flush()
	}))
	defer server.Close()

	peer := config.PeerConfig{Name: "slow", BaseURL: server.URL, Key: "k", Tier: 1, TimeoutSeconds: 1}
	g := &Gateway{peers: newPeerPool([]config.PeerConfig{peer}), logger: slog.Default()}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
	if !g.tryPeers(context.Background(), rec, req, []byte(`{}`), "m", config.TierZen, "rid") {
		t.Fatal("peer stream was not accepted")
	}
	body := rec.Body.String()
	if !strings.Contains(body, "data: a") || !strings.Contains(body, "data: b") {
		t.Fatalf("stream was cut at the header timeout: %q", body)
	}
}

func TestPeerCancellationDoesNotCoolPeer(t *testing.T) {
	started := make(chan struct{})
	finish := make(chan struct{})
	server := httptest.NewServer(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		close(started)
		select {
		case <-r.Context().Done():
		case <-finish:
		}
	}))
	defer server.Close()

	pool := newPeerPool([]config.PeerConfig{{Name: "p", BaseURL: server.URL, Key: "k", Tier: 1, CooldownSeconds: 60}})
	g := &Gateway{peers: pool, logger: slog.Default()}
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		<-started
		cancel()
	}()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`)).WithContext(ctx)
	if g.tryPeers(ctx, httptest.NewRecorder(), req, []byte(`{}`), "m", config.TierZen, "rid") {
		t.Fatal("canceled peer request was reported as served")
	}
	close(finish)
	pool.mu.Lock()
	cooling := len(pool.cooling)
	pool.mu.Unlock()
	if cooling != 0 {
		t.Fatalf("client cancellation cooled %d peer(s)", cooling)
	}
}

func TestPeerContextLimitUpdatesHeaderAndBoard(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"message":"prompt is too long: 331001 tokens > 330000 maximum","type":"invalid_request_error"}}`)
	}))
	defer server.Close()

	store, _ := modelcatalog.OpenContextLimits("")
	g := &Gateway{
		cfg: config.Config{Models: config.ModelsConfig{ContextOverrides: map[string]config.ModelLimits{
			"m": {Context: 320_000},
		}}},
		catalog:       modelcatalog.Catalog(config.TierZen, nil),
		contextLimits: store,
		logger:        slog.Default(),
		peers:         newPeerPool([]config.PeerConfig{{Name: "ctx", BaseURL: server.URL, Key: "k", Tier: 1}}),
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
	if !g.tryPeers(context.Background(), rec, req, []byte(`{}`), "m", config.TierZen, "rid") {
		t.Fatal("peer rejection was not forwarded")
	}
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("status = %d, want 400", rec.Code)
	}
	if got := rec.Header().Get("X-Context-Limit"); got != "313000" {
		t.Fatalf("X-Context-Limit = %q, want 313000", got)
	}
	if got := store.Advertised("m", config.TierZen, 0, 320_000); got != 313_000 {
		t.Fatalf("advertised = %d, want 313000", got)
	}
}

func TestPeerSuccessFeedsContextBoard(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"x","choices":[],"usage":{"prompt_tokens":100000,"completion_tokens":1,"total_tokens":100001}}`)
	}))
	defer server.Close()

	store, _ := modelcatalog.OpenContextLimits("")
	g := &Gateway{
		catalog:       modelcatalog.Catalog(config.TierZen, nil),
		contextLimits: store,
		logger:        slog.Default(),
		peers:         newPeerPool([]config.PeerConfig{{Name: "usage", BaseURL: server.URL, Key: "k", Tier: 1}}),
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
	if !g.tryPeers(context.Background(), rec, req, []byte(strings.Repeat("x", 400_000)), "m", config.TierZen, "rid") {
		t.Fatal("peer success was not forwarded")
	}
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200", rec.Code)
	}
	if got := store.Observed("m", config.TierZen); got != 100_000 {
		t.Fatalf("observed = %d, want 100000", got)
	}
	if !strings.Contains(rec.Body.String(), `"prompt_tokens":100000`) {
		t.Fatalf("peer body was not forwarded: %q", rec.Body.String())
	}
}

func TestPeerStreamFeedsContextBoard(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(w, "data: {\"id\":\"x\",\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n")
		_, _ = io.WriteString(w, "data: {\"id\":\"x\",\"choices\":[],\"usage\":{\"prompt_tokens\":50000,\"completion_tokens\":1,\"total_tokens\":50001}}\n\n")
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
	}))
	defer server.Close()

	store, _ := modelcatalog.OpenContextLimits("")
	g := &Gateway{
		catalog:       modelcatalog.Catalog(config.TierZen, nil),
		contextLimits: store,
		logger:        slog.Default(),
		peers:         newPeerPool([]config.PeerConfig{{Name: "stream", BaseURL: server.URL, Key: "k", Tier: 1}}),
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
	if !g.tryPeers(context.Background(), rec, req, []byte(strings.Repeat("x", 200_000)), "m", config.TierZen, "rid") {
		t.Fatal("peer stream was not forwarded")
	}
	if !strings.Contains(rec.Body.String(), "data: [DONE]") {
		t.Fatalf("peer stream was not forwarded: %q", rec.Body.String())
	}
	if got := store.Observed("m", config.TierZen); got != 50_000 {
		t.Fatalf("observed = %d, want 50000", got)
	}
}

func TestPeerSoftReplyDropsStaleContentLength(t *testing.T) {
	body := "unknown model " + strings.Repeat("x", 70_000)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.Header().Set("Content-Length", strconv.Itoa(len(body)))
		w.WriteHeader(http.StatusNotFound)
		_, _ = io.WriteString(w, body)
	}))
	defer server.Close()

	g := &Gateway{
		logger: slog.Default(),
		peers:  newPeerPool([]config.PeerConfig{{Name: "soft", BaseURL: server.URL, Key: "k", Tier: 1}}),
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
	if !g.tryPeers(context.Background(), rec, req, []byte(`{}`), "m", config.TierZen, "rid") {
		t.Fatal("soft peer reply was not forwarded")
	}
	if got := rec.Header().Get("Content-Length"); got != "" {
		t.Fatalf("stale Content-Length %q survived the buffered soft reply", got)
	}
	if !strings.Contains(rec.Body.String(), "unknown model") {
		t.Fatalf("soft peer body was not forwarded: %q", rec.Body.String())
	}
}

type trackingReadCloser struct {
	io.Reader
	closed *bool
}

func (r trackingReadCloser) Close() error {
	*r.closed = true
	return nil
}

func TestPeerSoftMarkerKeepsOriginalBodyClosable(t *testing.T) {
	closed := false
	resp := &http.Response{Body: trackingReadCloser{Reader: strings.NewReader("unknown model: retired"), closed: &closed}}
	if !peerSoftMarker(resp) {
		t.Fatal("model marker was not detected")
	}
	if body, err := io.ReadAll(resp.Body); err != nil || string(body) != "unknown model: retired" {
		t.Fatalf("restored body = %q, err = %v", body, err)
	}
	if err := resp.Body.Close(); err != nil {
		t.Fatal(err)
	}
	if !closed {
		t.Fatal("closing the restored body did not close the original peer response")
	}
}

func TestPeerTransportFailureFallsBackToCaller(t *testing.T) {
	g := &Gateway{
		logger: slog.Default(),
		peers:  newPeerPool([]config.PeerConfig{{Name: "dead", BaseURL: "http://127.0.0.1:1", Key: "k", Tier: 1, TimeoutSeconds: 1}}),
	}
	rec := httptest.NewRecorder()
	req := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(`{}`))
	if g.tryPeers(context.Background(), rec, req, []byte(`{}`), "m", config.TierZen, "rid") {
		t.Fatal("dead peer was reported as serving the request")
	}
	if rec.Body.Len() != 0 {
		t.Fatalf("dead peer wrote a response: %q", rec.Body.String())
	}
}

func TestPeerFallbackCodesCoverPeerExhaustion(t *testing.T) {
	for _, status := range []int{401, 403, 429, 500, 502, 503, 504} {
		if !peerFallbackCodes[status] {
			t.Fatalf("status %d should fall through to the next peer", status)
		}
	}
	if peerFallbackCodes[400] || peerFallbackCodes[404] {
		t.Fatal("a request-level rejection must reach the client")
	}
}
