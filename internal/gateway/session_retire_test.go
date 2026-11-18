package gateway

import (
	"net/http"
	"testing"

	"opencode2api/internal/identity"
)

// TestFreeTierGateClassifier separates the session-shape 403 from the
// network-abuse 403 and from a quota 429, so only the right one retires a
// session.
func TestFreeTierGateClassifier(t *testing.T) {
	gate := `{"error":{"type":"FreeTierError","message":"Free tier can only be used from within OpenCode"}}`
	if !isFreeTierGate(http.StatusForbidden, []byte(gate)) {
		t.Fatal("gate 403 not classified")
	}
	if isNetworkAbuseBlock(http.StatusForbidden, []byte(gate)) {
		t.Fatal("gate 403 mistaken for network abuse")
	}
	if isFreeTierGate(http.StatusTooManyRequests, []byte(gate)) {
		t.Fatal("429 must not retire a session")
	}
	if isFreeTierGate(http.StatusForbidden, []byte(`{"error":"abusive traffic"}`)) {
		t.Fatal("abuse 403 must not retire a session")
	}
}

// TestRetireSessionChangesDerivedID pins the generation bump: a retired
// conversation gets a fresh canonical id while official client ids are intact.
func TestRetireSessionChangesDerivedID(t *testing.T) {
	first := identity.CanonicalSessionID("conversation-alpha")
	second := identity.CanonicalSessionID("conversation-alpha")
	if first != second {
		t.Fatalf("session id is not stable before retirement: %q / %q", first, second)
	}
	if !identity.RetireSession(first) {
		t.Fatal("derived session was not retired")
	}
	third := identity.CanonicalSessionID("conversation-alpha")
	if third == first {
		t.Fatalf("retired session id was reused: %q", third)
	}
	if identity.RetireSession("ses_abcdef012345AbCDeF01234X9z") {
		t.Fatal("official session id must never be retired")
	}
}
