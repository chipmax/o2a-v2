package admin

import (
	"testing"

	"opencode2api/internal/telemetry"
)

// TestPoolEventIncludesToolAudit keeps the tool-call audit line visible in the
// incident feed. It is INFO level because it is expected traffic, so the old
// warn/error-only filter would otherwise hide exactly the events it describes.
func TestPoolEventIncludesToolAudit(t *testing.T) {
	audit := telemetry.LogEvent{
		Level:   "info",
		Message: "tool-call audit",
		Fields:  map[string]any{"event": "tool_call_audit", "tool_renames": 2},
	}
	if !poolEvent(audit) {
		t.Fatal("tool_call_audit was filtered out of /api/events")
	}
	ordinary := telemetry.LogEvent{Level: "info", Message: "request routed", Component: "http"}
	if poolEvent(ordinary) {
		t.Fatal("ordinary info events must stay out of the incident feed")
	}
}
