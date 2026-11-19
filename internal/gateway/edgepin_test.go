package gateway

import (
	"context"
	"net"
	"net/netip"
	"testing"
	"time"

	"opencode2api/internal/config"
)

func TestEdgePinOnlyRewritesPinnedHost(t *testing.T) {
	pin := newEdgePin([]string{"opencode.ai"}, time.Minute)
	// Seed a fake resolved set so the test needs no network.
	pin.mu.Lock()
	pin.addresses = []netip.Addr{
		netip.MustParseAddr("2606:4700::1111"),
		netip.MustParseAddr("2606:4700::2222"),
	}
	pin.resolvedAt = time.Now()
	pin.mu.Unlock()

	got, err := pin.rewrite(context.Background(), "opencode.ai:443")
	if err != nil {
		t.Fatal(err)
	}
	host, port, err := net.SplitHostPort(got)
	if err != nil || port != "443" {
		t.Fatalf("bad rewrite %q: %v", got, err)
	}
	if host != pin.addresses[0].String() && host != pin.addresses[1].String() {
		t.Fatalf("edge %q is not in the resolved set", host)
	}

	other, err := pin.rewrite(context.Background(), "example.com:443")
	if err != nil || other != "example.com:443" {
		t.Fatalf("non-pinned host was rewritten: %q %v", other, err)
	}
}

func TestEdgePinRetiresEdgeAfterTwoStreamCuts(t *testing.T) {
	pin := newEdgePin([]string{"opencode.ai"}, time.Minute)
	a := netip.MustParseAddr("2606:4700::1111")
	b := netip.MustParseAddr("2606:4700::2222")
	pin.mu.Lock()
	pin.addresses = []netip.Addr{a, b}
	pin.resolvedAt = time.Now()
	pin.mu.Unlock()

	pin.setPreferred(a)
	now := time.Now()
	if pin.ReportStreamReset(a, now) {
		t.Fatal("one stream cut must not retire the edge")
	}
	if !pin.ReportStreamReset(a, now.Add(time.Minute)) {
		t.Fatal("two cuts inside the window must retire the edge")
	}
	if _, retired := pin.retired[a]; !retired {
		t.Fatal("edge was not retired")
	}
	if pin.Preferred().IsValid() {
		t.Fatal("retired edge must stop being preferred")
	}
	// pick must not hand out a retired edge while another is available.
	got := pin.pick([]netip.Addr{a, b}, now.Add(2*time.Minute))
	if got != b {
		t.Fatalf("picked retired edge %s, want %s", got, b)
	}
}

func TestNewEdgePinConfigOptIn(t *testing.T) {
	if got := newEdgePinConfig(config.WarpConfig{RotateEdge: false}, "https://opencode.ai/inference"); got != nil {
		t.Fatal("edge pin must be nil when disabled")
	}
	got := newEdgePinConfig(config.WarpConfig{RotateEdge: true, EdgePinTTL: 60}, "https://opencode.ai/inference")
	if got == nil {
		t.Fatal("edge pin must be built when enabled")
	}
	if !got.allowed["opencode.ai"] {
		t.Fatalf("host not pinned: %v", got.allowed)
	}
}
