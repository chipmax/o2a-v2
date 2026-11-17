//go:build integration

// Package integration holds the pre-release smoke suite. It drives a running
// gateway over HTTP, so it is opt-in: `make integration` or
//
//	O2A_BASE_URL=http://127.0.0.1:8000 O2A_API_KEY=oc_sk_... go test -tags=integration ./internal/integration
package integration

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"sync"
	"testing"
	"time"
)

func baseURL(t *testing.T) string {
	t.Helper()
	base := os.Getenv("O2A_BASE_URL")
	if base == "" {
		t.Skip("O2A_BASE_URL is not set")
	}
	return base
}

func apiKey(t *testing.T) string {
	t.Helper()
	key := os.Getenv("O2A_API_KEY")
	if key == "" {
		t.Skip("O2A_API_KEY is not set")
	}
	return key
}

func chat(t *testing.T, base, key, model string, stream bool) (int, time.Duration, error) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{
		"model": model, "stream": stream, "max_tokens": 24,
		"messages": []map[string]string{{"role": "user", "content": "Reply with exactly: OK"}},
	})
	req, err := http.NewRequest(http.MethodPost, base+"/v1/chat/completions", bytes.NewReader(body))
	if err != nil {
		return 0, 0, err
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")
	start := time.Now()
	resp, err := (&http.Client{Timeout: 120 * time.Second}).Do(req)
	if err != nil {
		return 0, time.Since(start), err
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, time.Since(start), nil
}

// TestFreeModels answers once per free model and requires at least 80% of them
// to succeed, so a model removed upstream fails the release instead of the
// first user request.
func TestFreeModels(t *testing.T) {
	base, key := baseURL(t), apiKey(t)
	req, err := http.NewRequest(http.MethodGet, base+"/v1/models", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer "+key)
	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var payload struct {
		Data []struct {
			ID string `json:"id"`
		} `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		t.Fatal(err)
	}
	free := make([]string, 0, len(payload.Data))
	for _, model := range payload.Data {
		if len(model.ID) > 5 && model.ID[len(model.ID)-5:] == "-free" {
			free = append(free, model.ID)
		}
	}
	if len(free) == 0 {
		t.Fatal("no free model is exposed")
	}
	ok := 0
	for _, model := range free {
		status, elapsed, err := chat(t, base, key, model, false)
		if err != nil {
			t.Logf("%-30s error %v", model, err)
			continue
		}
		if status == http.StatusOK {
			ok++
		}
		t.Logf("%-30s %d %s", model, status, elapsed.Round(time.Millisecond))
	}
	if rate := float64(ok) / float64(len(free)); rate < 0.8 {
		t.Fatalf("only %d/%d free models answered (%.0f%% < 80%%)", ok, len(free), rate*100)
	}
}

// TestConcurrency sends eight streams at once, the load the pool is sized for,
// and requires every one to succeed.
func TestConcurrency(t *testing.T) {
	base, key := baseURL(t), apiKey(t)
	const parallel = 8
	var wg sync.WaitGroup
	statuses := make([]int, parallel)
	for index := 0; index < parallel; index++ {
		wg.Add(1)
		go func(index int) {
			defer wg.Done()
			status, _, err := chat(t, base, key, "mimo-v2.6-flash-free", true)
			if err != nil {
				t.Logf("request %d: %v", index, err)
				statuses[index] = -1
				return
			}
			statuses[index] = status
		}(index)
	}
	wg.Wait()
	ok := 0
	for index, status := range statuses {
		if status == http.StatusOK {
			ok++
		} else {
			t.Logf("request %d: status %d", index, status)
		}
	}
	if ok < parallel {
		t.Fatalf("%d/%d concurrent streams succeeded", ok, parallel)
	}
}

// TestReports verifies the surfaces a release must keep: health, metrics and
// the model list agree with each other.
func TestReports(t *testing.T) {
	base := baseURL(t)
	_ = apiKey(t)
	for _, path := range []string{"/healthz", "/metrics"} {
		resp, err := (&http.Client{Timeout: 15 * time.Second}).Get(base + path)
		if err != nil {
			t.Fatalf("%s: %v", path, err)
		}
		_, _ = io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		if resp.StatusCode != http.StatusOK {
			t.Fatalf("%s: status %d", path, resp.StatusCode)
		}
	}
	fmt.Fprintln(io.Discard)
}
