package gateway

import (
	"strings"
	"testing"
)

// TestTraceContextHeadersStableRoot pins the v2 trace fingerprint: the root is
// process-wide and shared across requests, while span and parent are fresh.
func TestTraceContextHeadersStableRoot(t *testing.T) {
	b1, tp1 := traceContextHeaders()
	b2, tp2 := traceContextHeaders()

	parts1, parts2 := strings.Split(b1, "-"), strings.Split(b2, "-")
	if len(parts1) != 4 || len(parts2) != 4 {
		t.Fatalf("b3 format = %q / %q, want 4 parts", b1, b2)
	}
	if parts1[0] != parts2[0] || parts1[0] != traceRootID {
		t.Fatalf("b3 root changed: %q -> %q (want %q)", b1, b2, traceRootID)
	}
	if parts1[1] == parts2[1] || parts1[3] == parts2[3] {
		t.Fatalf("span/parent reused: %q / %q", b1, b2)
	}
	if parts1[2] != "1" {
		t.Fatalf("b3 sampled flag = %q, want 1", parts1[2])
	}
	if !strings.HasPrefix(tp1, "00-"+traceRootID+"-") || !strings.HasPrefix(tp2, "00-"+traceRootID+"-") {
		t.Fatalf("traceparent root mismatch: %q / %q", tp1, tp2)
	}
	if !strings.HasSuffix(tp1, "-01") {
		t.Fatalf("traceparent flags = %q, want -01", tp1)
	}
}
