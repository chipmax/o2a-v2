package config

import (
	"testing"
)

func TestWarpSourceIPv6AcceptsAPrefixList(t *testing.T) {
	warpConfig := WarpConfig{
		Enabled:    true,
		PoolSize:   2,
		Stack:      "ipv6",
		SourceIPv6: " 2603:c024:4526:9797::/64 , 2001:db8:1234:5678::/64 ",
	}
	if err := warpConfig.Normalize(); err != nil {
		t.Fatalf("Normalize rejected a prefix list: %v", err)
	}
	const want = "2603:c024:4526:9797::/64,2001:db8:1234:5678::/64"
	if warpConfig.SourceIPv6 != want {
		t.Fatalf("source_ipv6 = %q, want %q", warpConfig.SourceIPv6, want)
	}
}

func TestWarpSourceIPv6RejectsOneBadPrefixInTheList(t *testing.T) {
	warpConfig := WarpConfig{
		Enabled:    true,
		PoolSize:   2,
		Stack:      "ipv6",
		SourceIPv6: "2603:c024:4526:9797::/64,not-a-prefix",
	}
	if err := warpConfig.Normalize(); err == nil {
		t.Fatal("Normalize accepted a list with a malformed prefix")
	}
}

func TestWarpSourceIPv6RejectsAPrefixLongerThanSlash64(t *testing.T) {
	warpConfig := WarpConfig{
		Enabled:    true,
		PoolSize:   2,
		Stack:      "ipv6",
		SourceIPv6: "2603:c024:4526:9797::/80",
	}
	if err := warpConfig.Normalize(); err == nil {
		t.Fatal("Normalize accepted a prefix longer than /64")
	}
}

func TestWarpSourceIPv6StillRequiresTheIPv6Stack(t *testing.T) {
	warpConfig := WarpConfig{
		Enabled:    true,
		PoolSize:   2,
		Stack:      "ipv4",
		SourceIPv6: "2603:c024:4526:9797::/64",
	}
	if err := warpConfig.Normalize(); err == nil {
		t.Fatal("Normalize accepted an IPv6 source on an IPv4 stack")
	}
}

func TestWarpPreloadAndEgressFilterValidation(t *testing.T) {
	for _, preload := range []int{0, 1, 32} {
		warpConfig := WarpConfig{Enabled: true, PoolSize: 4, Stack: "dual", Preload: preload, RequireIPv6Egress: true}
		if err := warpConfig.Normalize(); err != nil {
			t.Fatalf("Normalize rejected preload %d: %v", preload, err)
		}
	}
	for _, preload := range []int{-1, 33} {
		warpConfig := WarpConfig{Enabled: true, PoolSize: 4, Stack: "dual", Preload: preload}
		if err := warpConfig.Normalize(); err == nil {
			t.Fatalf("Normalize accepted preload %d", preload)
		}
	}
}
