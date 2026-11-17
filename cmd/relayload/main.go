// Command relayload drives one route with concurrent requests and reports the
// status mix and latency percentiles. It is a load harness, not part of the
// gateway: point it at a relay (target kilo/opencode) or at a gateway endpoint.
//
//	go run ./cmd/relayload -mode relay -endpoint http://127.0.0.1:4318 -token t -target kilo -c 40 -n 200
//	go run ./cmd/relayload -mode gateway -url http://127.0.0.1:8123 -key k -model deepseek-v4.1-flash-free -c 40 -n 200
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"sync"
	"sync/atomic"
	"time"

	"opencode2api/internal/relay"
)

type result struct {
	status int
	ms     int64
	err    string
}

func main() {
	mode := flag.String("mode", "relay", "relay or gateway")
	endpoint := flag.String("endpoint", "http://127.0.0.1:4318", "relay base URL")
	token := flag.String("token", "", "relay token")
	target := flag.String("target", "kilo", "relay upstream: kilo or opencode")
	gwURL := flag.String("url", "http://127.0.0.1:8123", "gateway base URL")
	key := flag.String("key", "", "gateway server key")
	modelID := flag.String("model", "deepseek-v4.1-flash-free", "model to request")
	concurrency := flag.Int("c", 20, "concurrent workers")
	total := flag.Int("n", 60, "total requests")
	maxTokens := flag.Int("max", 16, "max_tokens per request")
	timeout := flag.Duration("timeout", 90*time.Second, "per-request timeout")
	flag.Parse()

	client := http.DefaultClient
	var base string
	body := map[string]any{
		"model":      *modelID,
		"max_tokens": *maxTokens,
		"messages":   []map[string]string{{"role": "user", "content": "reply with the single word pong"}},
	}
	headers := http.Header{"Content-Type": {"application/json"}}
	switch *mode {
	case "relay":
		u, err := url.Parse(*endpoint)
		if err != nil {
			fail("endpoint: %v", err)
		}
		transport, err := relay.New(relay.Options{Endpoint: u, Token: *token, Timeout: *timeout})
		if err != nil {
			fail("transport: %v", err)
		}
		client = &http.Client{Transport: transport}
		if *target == "kilo" {
			body["model"] = "kilo-auto/free"
			base = "https://api.kilo.ai/api/gateway/chat/completions"
		} else {
			base = "https://opencode.ai/inference/v1/chat/completions"
			headers.Set("user-agent", "opencode/latest/2.1.1/cli")
			headers.Set("x-opencode-client", "cli")
		}
	case "gateway":
		base = *gwURL + "/v1/chat/completions"
		headers.Set("Authorization", "Bearer "+*key)
	default:
		fail("unknown mode %q", *mode)
	}
	payload, _ := json.Marshal(body)

	started := time.Now()
	results := make([]result, 0, *total)
	var mu sync.Mutex
	var next int64
	var workers sync.WaitGroup
	sem := make(chan struct{}, *concurrency)
	for i := 0; i < *total; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			atomic.AddInt64(&next, 1)
			ctx, cancel := context.WithTimeout(context.Background(), *timeout)
			defer cancel()
			req, err := http.NewRequestWithContext(ctx, http.MethodPost, base, bytes.NewReader(payload))
			if err != nil {
				mu.Lock()
				results = append(results, result{err: err.Error()})
				mu.Unlock()
				return
			}
			req.Header = headers.Clone()
			began := time.Now()
			resp, err := client.Do(req)
			entry := result{ms: time.Since(began).Milliseconds()}
			if err != nil {
				entry.err = err.Error()
			} else {
				entry.status = resp.StatusCode
				io.CopyN(io.Discard, resp.Body, 1<<16)
				resp.Body.Close()
			}
			mu.Lock()
			results = append(results, entry)
			mu.Unlock()
		}()
	}
	workers.Wait()
	elapsed := time.Since(started)

	byStatus := map[string]int{}
	latencies := make([]int64, 0, len(results))
	for _, entry := range results {
		label := fmt.Sprintf("%d", entry.status)
		if entry.err != "" {
			label = "error: " + truncate(entry.err, 60)
		}
		byStatus[label]++
		latencies = append(latencies, entry.ms)
	}
	sort.Slice(latencies, func(i, j int) bool { return latencies[i] < latencies[j] })
	fmt.Printf("requests=%d concurrency=%d elapsed=%s rps=%.1f\n", len(results), *concurrency, elapsed.Round(time.Millisecond), float64(len(results))/elapsed.Seconds())
	fmt.Printf("latency ms: p50=%d p90=%d p99=%d max=%d\n", pct(latencies, 0.50), pct(latencies, 0.90), pct(latencies, 0.99), latencies[len(latencies)-1])
	for label, count := range byStatus {
		fmt.Printf("  %-24s %d\n", label, count)
	}
}

func pct(sorted []int64, q float64) int64 {
	if len(sorted) == 0 {
		return 0
	}
	index := int(float64(len(sorted)-1) * q)
	return sorted[index]
}

func truncate(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit]
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
