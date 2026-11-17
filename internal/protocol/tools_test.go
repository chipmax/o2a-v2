package protocol

import (
	"strings"
	"testing"
)

// TestClientToolWireNameUsesOrdinaryNamespace pins the upstream-compatible
// prefix. The free tier refuses a tool name containing the old "clint_" literal
// while serving an ordinary namespace; a regression here turns every
// tool-bearing free-tier request into a 429 that looks like spent quota.
func TestClientToolWireNameUsesOrdinaryNamespace(t *testing.T) {
	used := map[string]bool{}
	wireName := ClientToolWireName("lookup_thing", used)
	if !strings.HasPrefix(wireName, ClientToolPrefix) {
		t.Fatalf("wire name %q does not start with %q", wireName, ClientToolPrefix)
	}
	if ClientToolPrefix == "clint_" {
		t.Fatal("the refused clint_ prefix came back")
	}
	if strings.Contains(strings.ToLower(wireName), "lookup") {
		t.Fatalf("client tool name leaked onto the wire: %q", wireName)
	}
	// Each request starts with a fresh used map; the digest must be stable
	// across requests (within a request, the suffix loop avoids collisions).
	if second := ClientToolWireName("lookup_thing", map[string]bool{}); second != wireName {
		t.Fatalf("the same client tool got two wire names: %q and %q", wireName, second)
	}
}
