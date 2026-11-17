package warp

import (
	"context"
	"net/netip"
	"testing"
)

func TestSampleSourceUsesEveryPrefix(t *testing.T) {
	first := netip.MustParsePrefix("2001:db8:1::/64")
	second := netip.MustParsePrefix("2001:db8:2::/56")
	opts := Options{Sources: []netip.Prefix{first, second}}

	hit := map[string]bool{}
	for i := 0; i < 256; i++ {
		address, err := opts.sampleSource()
		if err != nil {
			t.Fatalf("sampleSource: %v", err)
		}
		switch {
		case first.Contains(address):
			hit["first"] = true
		case second.Contains(address):
			hit["second"] = true
		default:
			t.Fatalf("sampled %s outside every configured prefix", address)
		}
	}
	if !hit["first"] || !hit["second"] {
		t.Fatalf("sampling never used one of the prefixes: %v", hit)
	}
}

func TestSampleSourceFallsBackToTheSinglePrefix(t *testing.T) {
	only := netip.MustParsePrefix("2001:db8:1::/64")
	address, err := Options{SourceIPv6: only}.sampleSource()
	if err != nil {
		t.Fatalf("sampleSource: %v", err)
	}
	if !only.Contains(address) {
		t.Fatalf("sampled %s outside %s", address, only)
	}
}

func TestSampleSourceWithoutAnyPrefixReturnsNothing(t *testing.T) {
	address, err := Options{}.sampleSource()
	if err != nil {
		t.Fatalf("sampleSource: %v", err)
	}
	if address.IsValid() {
		t.Fatalf("sampled %s without a configured prefix", address)
	}
}

func TestNewWithOptionsRejectsABadSourcePrefix(t *testing.T) {
	_, err := NewWithOptions(context.Background(), nil, Options{Size: 1, Stack: StackIPv6, Sources: []netip.Prefix{netip.MustParsePrefix("2001:db8::/96")}})
	if err == nil {
		t.Fatal("NewWithOptions accepted a /96 source")
	}
}
