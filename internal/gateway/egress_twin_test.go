package gateway

import (
	"log/slog"
	"testing"
	"time"

	"opencode2api/internal/config"
)

func TestTransportPoolIPv4Twin(t *testing.T) {
	cfg := config.PerformanceConfig{MaxIdleConns: 16, MaxIdleConnsPerHost: 8, ConnectTimeoutSeconds: 5}
	p, err := transportPool([]string{"direct"}, cfg, config.EgressConfig{IPv4Twins: true}, 30*time.Second, slog.Default())
	if err != nil {
		t.Fatalf("transportPool: %v", err)
	}
	if len(p.items) != 2 {
		t.Fatalf("want direct + twin, got %d routes", len(p.items))
	}
	if p.items[0].ipv4 || !p.items[1].ipv4 {
		t.Fatal("second route must be the tcp4 twin")
	}
	if p.items[0].name != "direct" || p.items[1].name != "direct-v4" {
		t.Fatalf("names = %q, %q", p.items[0].name, p.items[1].name)
	}
}

func TestTransportPoolIPv4First(t *testing.T) {
	cfg := config.PerformanceConfig{MaxIdleConns: 16, MaxIdleConnsPerHost: 8, ConnectTimeoutSeconds: 5}
	p, err := transportPool([]string{"direct", "socks5h://127.0.0.1:13001"}, cfg,
		config.EgressConfig{IPv4Twins: true, IPv4First: true}, 30*time.Second, slog.Default())
	if err != nil {
		t.Fatalf("transportPool: %v", err)
	}
	if len(p.items) != 3 || !p.items[0].ipv4 {
		t.Fatalf("ipv4_first must put the twin first: %+v", p.items)
	}
	for index, item := range p.items {
		if item.index != index {
			t.Fatalf("route index not reassignmed after reorder: %d != %d", item.index, index)
		}
	}
}
