package gateway

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"opencode2api/internal/config"
	wire "opencode2api/internal/protocol"
)

const loadTestKey = "load-test-local-key"

func testLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func loadTestDuration(name string) time.Duration {
	raw := os.Getenv(name)
	if raw == "" {
		return 0
	}
	n, err := strconv.Atoi(raw)
	if err != nil || n <= 0 {
		return 0
	}
	return time.Duration(n) * time.Millisecond
}

// loadGateway builds a minimal gateway whose Zen lane points at upstream.
func loadGateway(t *testing.T, upstream, model string, protocols map[string]string) *Gateway {
	t.Helper()
	cfg := config.Config{
		ServerKeys: []string{loadTestKey},
		ZenKeys:    []string{"test-zen-key"},
		Prefer:     config.TierZen,
		Upstream:   config.UpstreamConfig{Zen: upstream},
		Retry:      config.RetryConfig{MaxAttempts: 1, TimeoutSeconds: 60},
		Models:     config.ModelsConfig{Protocols: protocols, RefreshSeconds: 300},
		Performance: config.PerformanceConfig{
			MaxIdleConns: 256, MaxIdleConnsPerHost: 128,
			ConnectTimeoutSeconds: 2, FailureCooldownSeconds: 1,
		},
	}
	g, err := New(cfg, testLogger(), nil, nil)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	g.catalog.Replace([]string{model}, nil)
	return g
}

func inferenceRequest(t *testing.T, client *http.Client, base, path, model string, body []byte) (*http.Response, []byte) {
	t.Helper()
	resp, data, err := inferenceRequestE(client, base, path, body)
	if err != nil {
		t.Fatalf("request: %v", err)
	}
	return resp, data
}

func inferenceRequestE(client *http.Client, base, path string, body []byte) (*http.Response, []byte, error) {
	req, err := http.NewRequest(http.MethodPost, base+path, bytes.NewReader(body))
	if err != nil {
		return nil, nil, err
	}
	req.Header.Set("Authorization", "Bearer "+loadTestKey)
	req.Header.Set("Content-Type", "application/json")
	resp, err := client.Do(req)
	if err != nil {
		return nil, nil, err
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, nil, err
	}
	return resp, data, nil
}

func chatResponse(model, content string) []byte {
	body, _ := json.Marshal(map[string]any{
		"id": "chatcmpl-load", "object": "chat.completion", "created": 1, "model": model,
		"choices": []any{map[string]any{
			"index": 0, "message": map[string]any{"role": "assistant", "content": content}, "finish_reason": "stop",
		}},
		"usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 2, "total_tokens": 12},
	})
	return body
}

func chatChunk(model, content string) string {
	return fmt.Sprintf("data: {\"id\":\"chatcmpl-load\",\"object\":\"chat.completion.chunk\",\"created\":1,\"model\":%q,\"choices\":[{\"index\":0,\"delta\":{\"role\":\"assistant\",\"content\":%q},\"finish_reason\":null}]}\n\n", model, content)
}

func TestConcurrentChatRequests(t *testing.T) {
	delay := loadTestDuration("O2A_LOAD_DELAY_MS")
	var upstreamCalls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		upstreamCalls.Add(1)
		_, _ = io.Copy(io.Discard, r.Body)
		if delay > 0 {
			time.Sleep(delay)
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(chatResponse("fake-model", "ok"))
	}))
	defer upstream.Close()

	g := loadGateway(t, upstream.URL, "fake-model", map[string]string{"fake-model": string(wire.Chat)})
	server := httptest.NewServer(g.Handler())
	defer server.Close()

	requests := 200
	if raw := os.Getenv("O2A_LOAD_REQUESTS"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			requests = n
		}
	}
	workers := 32
	if raw := os.Getenv("O2A_LOAD_WORKERS"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil && n > 0 {
			workers = n
		}
	}
	body := []byte(`{"model":"fake-model","messages":[{"role":"user","content":"hello"}]}`)
	jobs := make(chan int)
	errs := make(chan string, requests)
	var wg sync.WaitGroup
	client := server.Client()
	client.Timeout = 10 * time.Second
	for range workers {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range jobs {
				resp, data, err := inferenceRequestE(client, server.URL, "/v1/chat/completions", body)
				if err != nil {
					errs <- err.Error()
					continue
				}
				if resp.StatusCode != http.StatusOK {
					errs <- fmt.Sprintf("status %d: %s", resp.StatusCode, data)
					continue
				}
				var document map[string]any
				if err := json.Unmarshal(data, &document); err != nil {
					errs <- fmt.Sprintf("invalid JSON: %v", err)
					continue
				}
				choices, _ := document["choices"].([]any)
				if len(choices) != 1 {
					errs <- fmt.Sprintf("choices = %v", document["choices"])
				}
			}
		}()
	}
	for i := range requests {
		jobs <- i
	}
	close(jobs)
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
	if got := upstreamCalls.Load(); got != int64(requests) {
		t.Fatalf("upstream calls = %d, want %d", got, requests)
	}
}

func TestStreamingChatAndProtocolTranscode(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var payload map[string]any
		_ = json.NewDecoder(r.Body).Decode(&payload)
		stream, _ := payload["stream"].(bool)
		if !stream {
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(chatResponse("fake-model", "ok"))
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		_, _ = io.WriteString(w, chatChunk("fake-model", "o"))
		flusher.Flush()
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
	defer upstream.Close()

	g := loadGateway(t, upstream.URL, "fake-model", map[string]string{"fake-model": string(wire.Chat)})
	server := httptest.NewServer(g.Handler())
	defer server.Close()
	client := server.Client()
	client.Timeout = 10 * time.Second

	cases := []struct {
		name string
		path string
		body []byte
		want string
	}{
		{"chat", "/v1/chat/completions", []byte(`{"model":"fake-model","stream":true,"messages":[{"role":"user","content":"hello"}]}`), "[DONE]"},
		{"anthropic", "/v1/messages", []byte(`{"model":"fake-model","stream":true,"max_tokens":16,"messages":[{"role":"user","content":"hello"}]}`), "content_block_delta"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, data := inferenceRequest(t, client, server.URL, tc.path, "fake-model", tc.body)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, body = %s", resp.StatusCode, data)
			}
			if !bytes.Contains(data, []byte(tc.want)) {
				t.Fatalf("stream does not contain %q: %s", tc.want, data)
			}
		})
	}
}

func TestConcurrentStreaming(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "text/event-stream")
		w.WriteHeader(http.StatusOK)
		flusher := w.(http.Flusher)
		for i := range 4 {
			_, _ = io.WriteString(w, chatChunk("fake-model", fmt.Sprintf("chunk-%d", i)))
			flusher.Flush()
		}
		_, _ = io.WriteString(w, "data: [DONE]\n\n")
		flusher.Flush()
	}))
	defer upstream.Close()

	g := loadGateway(t, upstream.URL, "fake-model", map[string]string{"fake-model": string(wire.Chat)})
	server := httptest.NewServer(g.Handler())
	defer server.Close()
	client := server.Client()
	client.Timeout = 10 * time.Second
	body := []byte(`{"model":"fake-model","stream":true,"messages":[{"role":"user","content":"hello"}]}`)

	const requests = 40
	var wg sync.WaitGroup
	errs := make(chan string, requests)
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range requests / 8 {
				resp, data, err := inferenceRequestE(client, server.URL, "/v1/chat/completions", body)
				if err != nil {
					errs <- err.Error()
					continue
				}
				if resp.StatusCode != http.StatusOK {
					errs <- fmt.Sprintf("status %d: %s", resp.StatusCode, data)
					continue
				}
				if !bytes.Contains(data, []byte("[DONE]")) {
					errs <- "stream ended without [DONE]: " + string(data)
				}
			}
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		t.Error(err)
	}
}

func TestClientCancellationReleasesConcurrencySlot(t *testing.T) {
	resetGlobalQuota(t)
	started := make(chan struct{}, 1)
	upstreamDone := make(chan struct{}, 1)
	var calls atomic.Int64
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		if calls.Add(1) == 1 {
			started <- struct{}{}
			<-r.Context().Done()
			upstreamDone <- struct{}{}
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(chatResponse("fake-model", "ok"))
	}))
	defer upstream.Close()

	g := loadGateway(t, upstream.URL, "fake-model", map[string]string{"fake-model": string(wire.Chat)})
	g.cfg.ServerKeyMetadata = map[string]config.ServerKeyMetadata{
		config.Fingerprint(loadTestKey): {MaxConcurrency: 1},
	}
	gatewayCanceled := make(chan struct{})
	var gatewayCanceledOnce sync.Once
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		context.AfterFunc(r.Context(), func() { gatewayCanceledOnce.Do(func() { close(gatewayCanceled) }) })
		g.Handler().ServeHTTP(w, r)
	}))
	defer server.Close()
	client := server.Client()
	client.Timeout = 10 * time.Second
	body := []byte(`{"model":"fake-model","messages":[{"role":"user","content":"hello"}]}`)

	ctx, cancel := context.WithCancel(context.Background())
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, server.URL+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("Authorization", "Bearer "+loadTestKey)
	req.Header.Set("Content-Type", "application/json")
	done := make(chan struct{})
	go func() {
		defer close(done)
		resp, err := client.Do(req)
		if err == nil {
			_, _ = io.Copy(io.Discard, resp.Body)
			resp.Body.Close()
		}
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("upstream request did not start")
	}
	cancel()
	server.CloseClientConnections()
	<-done
	select {
	case <-gatewayCanceled:
	case <-time.After(2 * time.Second):
		t.Fatal("gateway request context was not canceled")
	}
	select {
	case <-upstreamDone:
	case <-time.After(10 * time.Second):
		t.Fatal("upstream did not observe the cancellation")
	}

	resp, data := inferenceRequest(t, client, server.URL, "/v1/chat/completions", "fake-model", body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("request after cancellation status = %d, body = %s", resp.StatusCode, data)
	}
}

func TestCrossProtocolJSON(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(chatResponse("fake-model", "ok"))
	}))
	defer upstream.Close()

	g := loadGateway(t, upstream.URL, "fake-model", map[string]string{"fake-model": string(wire.Chat)})
	server := httptest.NewServer(g.Handler())
	defer server.Close()
	client := server.Client()
	client.Timeout = 10 * time.Second

	cases := []struct {
		name string
		path string
		body []byte
		want string
	}{
		{"responses", "/v1/responses", []byte(`{"model":"fake-model","input":"hello"}`), "ok"},
		{"anthropic", "/v1/messages", []byte(`{"model":"fake-model","max_tokens":16,"messages":[{"role":"user","content":"hello"}]}`), "ok"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			resp, data := inferenceRequest(t, client, server.URL, tc.path, "fake-model", tc.body)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("status = %d, body = %s", resp.StatusCode, data)
			}
			if !bytes.Contains(data, []byte(tc.want)) {
				t.Fatalf("response does not contain %q: %s", tc.want, data)
			}
		})
	}
}

func TestConcurrentQuotaLimit(t *testing.T) {
	resetGlobalQuota(t)
	const attempts = 16
	releaseUpstream := make(chan struct{})
	var releaseOnce sync.Once
	releaseAll := func() { releaseOnce.Do(func() { close(releaseUpstream) }) }
	var started atomic.Int64
	startedCh := make(chan struct{}, attempts)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		started.Add(1)
		startedCh <- struct{}{}
		<-releaseUpstream
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(chatResponse("fake-model", "ok"))
	}))
	defer upstream.Close()
	defer releaseAll()

	g := loadGateway(t, upstream.URL, "fake-model", map[string]string{"fake-model": string(wire.Chat)})
	id := config.Fingerprint(loadTestKey)
	g.cfg.ServerKeyMetadata = map[string]config.ServerKeyMetadata{id: {MaxConcurrency: 2, DailyRequests: 100}}
	server := httptest.NewServer(g.Handler())
	defer server.Close()
	client := server.Client()
	client.Timeout = 60 * time.Second

	body := []byte(`{"model":"fake-model","messages":[{"role":"user","content":"hello"}]}`)
	limited := make(chan struct{}, attempts)
	requestErrs := make(chan string, attempts)
	var wg sync.WaitGroup
	var okCount, limitedCount atomic.Int64
	for range attempts {
		wg.Add(1)
		go func() {
			defer wg.Done()
			resp, data, err := inferenceRequestE(client, server.URL, "/v1/chat/completions", body)
			if err != nil {
				requestErrs <- fmt.Sprintf("request: %v", err)
				return
			}
			switch resp.StatusCode {
			case http.StatusOK:
				okCount.Add(1)
			case http.StatusTooManyRequests:
				if !bytes.Contains(data, []byte("key_concurrency_limit")) {
					requestErrs <- fmt.Sprintf("unexpected 429 body: %s", data)
				}
				limitedCount.Add(1)
				limited <- struct{}{}
			default:
				requestErrs <- fmt.Sprintf("unexpected status %d: %s", resp.StatusCode, data)
			}
		}()
	}
	// The two admitted requests block in the upstream, so the remaining
	// attempts must be refused while their slots are still held.
	for i := 0; i < attempts-2; i++ {
		select {
		case <-limited:
		case <-time.After(5 * time.Second):
			releaseAll()
			wg.Wait()
			t.Fatalf("only %d requests were limited; concurrent slots leaked", i)
		}
	}
	for range 2 {
		select {
		case <-startedCh:
		case <-time.After(5 * time.Second):
			releaseAll()
			wg.Wait()
			t.Fatalf("only %d admitted requests reached the upstream", started.Load())
		}
	}
	if got := started.Load(); got != 2 {
		releaseAll()
		wg.Wait()
		t.Fatalf("upstream requests = %d, want 2 concurrent requests", got)
	}
	releaseAll()
	wg.Wait()
	close(requestErrs)
	for message := range requestErrs {
		t.Error(message)
	}
	if got := okCount.Load(); got != 2 {
		t.Fatalf("successful requests = %d, want 2", got)
	}
	if got := limitedCount.Load(); got != attempts-2 {
		t.Fatalf("limited requests = %d, want %d", got, attempts-2)
	}
}

func TestContextLimitErrorHeader(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"error":{"message":"maximum context length is 131072 tokens"}}`)
	}))
	defer upstream.Close()

	g := loadGateway(t, upstream.URL, "fake-model", map[string]string{"fake-model": string(wire.Chat)})
	server := httptest.NewServer(g.Handler())
	defer server.Close()
	client := server.Client()
	client.Timeout = 10 * time.Second
	resp, data := inferenceRequest(t, client, server.URL, "/v1/chat/completions", "fake-model", []byte(`{"model":"fake-model","messages":[{"role":"user","content":"hello"}]}`))
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("status = %d, body = %s", resp.StatusCode, data)
	}
	if got := resp.Header.Get("X-Context-Limit"); got != "124000" {
		t.Fatalf("X-Context-Limit = %q, want 124000", got)
	}
}

func TestCatalogModelAvailableAfterReplace(t *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = io.Copy(io.Discard, r.Body)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(chatResponse("fake-model", "ok"))
	}))
	defer upstream.Close()
	g := loadGateway(t, upstream.URL, "fake-model", map[string]string{"fake-model": string(wire.Chat)})
	if !g.catalog.Supported("fake-model") {
		t.Fatal("catalog did not expose the test model")
	}
	route, err := g.catalog.Route("fake-model", true, false, false)
	if err != nil {
		t.Fatalf("route: %v", err)
	}
	if route.Tier != config.TierZen || route.Protocol != wire.Chat {
		t.Fatalf("route = %+v", route)
	}
}
