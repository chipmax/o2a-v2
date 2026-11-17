// Command relayprobe exercises one relay route the way the gateway does and
// measures what a relay deployment actually gives you: which exit addresses its
// instances report, how the free lane answers, and whether a burst spreads over
// several addresses.
//
//	go run ./cmd/relayprobe -endpoint https://<project>.vercel.app -token <token>
//	go run ./cmd/relayprobe -endpoint https://<project>.vercel.app -token <token> -pings 10 -burst 12
package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"

	"opencode2api/internal/relay"
)

const freeBody = `{"model":"deepseek-v4.1-flash-free","messages":[{"role":"user","content":"say pong only"}],"max_tokens":16}`

func main() {
	endpoint := flag.String("endpoint", "", "relay base URL, e.g. https://relay.vercel.app")
	token := flag.String("token", "", "relay token (x-relay-key)")
	pings := flag.Int("pings", 4, "control pings, to see how instances and addresses are assigned")
	burst := flag.Int("burst", 0, "concurrent free-lane requests; each answer carries the address that served it")
	chat := flag.Bool("chat", true, "also try one Kilo and one free-lane request")
	timeout := flag.Duration("timeout", 90*time.Second, "per-request timeout")
	flag.Parse()
	if *endpoint == "" {
		fail("usage: relayprobe -endpoint <relay-url> -token <token> [-pings N] [-burst N]")
	}

	base := strings.TrimRight(*endpoint, "/")
	ping := &http.Client{Timeout: 20 * time.Second}
	seen := map[string][]string{}
	for i := 0; i < *pings; i++ {
		req, _ := http.NewRequest(http.MethodPost, base+"/", nil)
		req.Header.Set(relay.HeaderKey, *token)
		req.Header.Set(relay.HeaderControl, "ping")
		resp, err := ping.Do(req)
		if err != nil {
			fmt.Printf("ping %d: %v\n", i+1, err)
			continue
		}
		body, _ := io.ReadAll(io.LimitReader(resp.Body, 512))
		resp.Body.Close()
		egress, instance := resp.Header.Get(relay.HeaderEgress), resp.Header.Get(relay.HeaderInstance)
		if egress == "" {
			egress = field(string(body), "egress")
		}
		if instance == "" {
			instance = field(string(body), "instance")
		}
		if resp.StatusCode != http.StatusOK {
			fmt.Printf("ping %d: %d %s\n", i+1, resp.StatusCode, strings.TrimSpace(string(body)))
			continue
		}
		seen[egress] = append(seen[egress], instance)
	}
	if len(seen) > 0 {
		fmt.Printf("ping: %d addresses over %d requests\n", len(seen), *pings)
		for _, egress := range sortedKeys(seen) {
			fmt.Printf("  %-16s %d instance(s): %s\n", egress, len(seen[egress]), strings.Join(trimAll(seen[egress], 3), ", "))
		}
	}

	parsed, err := url.Parse(base)
	if err != nil {
		fail("endpoint: %v", err)
	}
	transport, err := relay.New(relay.Options{Endpoint: parsed, Token: *token, Timeout: *timeout,
		Logger: slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelWarn}))})
	if err != nil {
		fail("transport: %v", err)
	}
	client := &http.Client{Transport: transport}

	if *chat {
		probe(client, transport, "https://api.kilo.ai/api/gateway/chat/completions",
			`{"model":"kilo-auto/free","messages":[{"role":"user","content":"say pong only"}],"max_tokens":16}`)
		probe(client, transport, "https://opencode.ai/inference/v1/chat/completions", freeBody)
	}
	if *burst > 0 {
		addresses := map[string]int{}
		statuses := map[int]int{}
		var mu sync.Mutex
		var wg sync.WaitGroup
		started := time.Now()
		for i := 0; i < *burst; i++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				resp, err := do(client, freeBody, "https://opencode.ai/inference/v1/chat/completions")
				mu.Lock()
				defer mu.Unlock()
				if err != nil {
					statuses[0]++
					addresses["error"]++
					return
				}
				resp.Body.Close()
				statuses[resp.StatusCode]++
				addresses[orUnknown(resp.Header.Get(relay.HeaderEgress))]++
			}()
		}
		wg.Wait()
		fmt.Printf("burst: %d requests in %s, %d address(es)\n", *burst, time.Since(started).Round(time.Millisecond), len(addresses))
		for _, egress := range sortedKeys(addresses) {
			fmt.Printf("  %-16s %d\n", egress, addresses[egress])
		}
		for _, status := range sortedStatuses(statuses) {
			fmt.Printf("  status %-3d %d\n", status, statuses[status])
		}
	}
}

func probe(client *http.Client, transport *relay.Transport, target, body string) {
	start := time.Now()
	resp, err := do(client, body, target)
	if err != nil {
		fmt.Printf("%s -> error: %v\n", target, err)
		return
	}
	payload, _ := io.ReadAll(io.LimitReader(resp.Body, 200))
	resp.Body.Close()
	fmt.Printf("%s -> %d in %s egress=%q spent=%d retry_after=%q\n  %s\n",
		target, resp.StatusCode, time.Since(start).Round(time.Millisecond),
		resp.Header.Get(relay.HeaderEgress), transport.SpentCount(), resp.Header.Get("Retry-After"), strings.TrimSpace(string(payload)))
}

func do(client *http.Client, body, target string) (*http.Response, error) {
	req, err := http.NewRequest(http.MethodPost, target, bytes.NewReader([]byte(body)))
	if err != nil {
		return nil, err
	}
	req.Header.Set("content-type", "application/json")
	req.Header.Set("user-agent", "opencode/latest/2.0.32/cli")
	req.Header.Set("x-opencode-client", "cli")
	return client.Do(req)
}

func field(body, key string) string {
	needle := `"` + key + `":"`
	index := strings.Index(body, needle)
	if index < 0 {
		return ""
	}
	rest := body[index+len(needle):]
	if end := strings.IndexByte(rest, '"'); end >= 0 {
		return rest[:end]
	}
	return ""
}

func orUnknown(value string) string {
	if strings.TrimSpace(value) == "" {
		return "(none reported)"
	}
	return value
}

func sortedKeys[V any](m map[string]V) []string {
	keys := make([]string, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	return keys
}

func sortedStatuses(m map[int]int) []int {
	keys := make([]int, 0, len(m))
	for key := range m {
		keys = append(keys, key)
	}
	sort.Ints(keys)
	return keys
}

func trimAll(values []string, limit int) []string {
	out := make([]string, 0, min(len(values), limit))
	for i, value := range values {
		if i >= limit {
			out = append(out, "…")
			break
		}
		out = append(out, value[:min(len(value), 12)])
	}
	return out
}

func fail(format string, args ...any) {
	fmt.Fprintf(os.Stderr, format+"\n", args...)
	os.Exit(1)
}
