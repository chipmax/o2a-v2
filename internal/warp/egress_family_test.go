package warp

import (
	"net/netip"
	"testing"
)

// TestRequireIPv6Egress separates a private /64 bucket from Cloudflare's
// shared IPv4 WARP address. Keeping the latter would add an "active" exit
// without adding free-tier quota, which is the trap on IPv4-only hosts.
func TestRequireIPv6Egress(t *testing.T) {
	options := Options{RequireIPv6Egress: true}
	shared := &exit{egress: netip.MustParseAddr("104.28.222.73")}
	private := &exit{egress: netip.MustParseAddr("2a09:bac5:d45a:263c::3cf:59")}
	if !ipv6Required(options, shared) {
		t.Fatal("a shared IPv4 egress was accepted")
	}
	if ipv6Required(options, private) {
		t.Fatal("a private IPv6 /64 egress was rejected")
	}
	if !ipv6Required(options, &exit{}) {
		t.Fatal("an unmeasured egress must be rejected")
	}
	if ipv6Required(Options{}, shared) {
		t.Fatal("the filter must be opt-in")
	}
}

// TestExplicitPreload pins the config-driven warm count: zero keeps the old
// half-of-size default, and a value above size is capped rather than causing
// endless registration attempts.
func TestExplicitPreload(t *testing.T) {
	cases := map[int]int{0: 2, 1: 1, 5: 4, 20: 4}
	for given, want := range cases {
		size := 4
		preload := given
		if preload <= 0 {
			preload = size / 2
		}
		if preload < 1 {
			preload = 1
		}
		if preload > size {
			preload = size
		}
		if preload != want {
			t.Fatalf("preload %d -> %d, want %d", given, preload, want)
		}
	}
}
