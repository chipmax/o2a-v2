package gateway

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

func TestCheckAllProbesHealthyRoutes(t *testing.T) {
	var hits atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer target.Close()

	proxy := &proxyTransport{name: "direct", client: target.Client()}
	proxy.healthy.Store(true)
	transports := &proxyTransports{items: []*proxyTransport{proxy}}

	if got := transports.CheckHealth(context.Background(), target.URL, 2*time.Second); len(got) != 0 {
		t.Fatalf("CheckHealth probed a healthy route: got %d results", len(got))
	}
	if got := hits.Load(); got != 0 {
		t.Fatalf("CheckHealth made %d requests, want 0", got)
	}

	results := transports.CheckAll(context.Background(), target.URL, 2*time.Second)
	if len(results) != 1 {
		t.Fatalf("CheckAll returned %d results, want 1", len(results))
	}
	if results[0].err != nil {
		t.Fatalf("CheckAll failed against a reachable target: %v", results[0].err)
	}
	if !proxy.healthy.Load() {
		t.Fatal("healthy route was marked unhealthy")
	}
	if proxy.lastChecked.Load() == 0 {
		t.Fatal("last check timestamp was not recorded")
	}
	if got := hits.Load(); got != 1 {
		t.Fatalf("CheckAll made %d requests, want 1", got)
	}
}
