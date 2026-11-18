package gateway

import (
	"net/http"
	"testing"
	"time"
)

func quotaWallResponse(t *testing.T, seconds string) *http.Response {
	t.Helper()
	return &http.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{"Retry-After": []string{seconds}}}
}

// TestNextSkipsQuotaWalledKey pins v1.8.7: a key walled by a long Retry-After
// must not be handed traffic just because its short cooldown expired. When the
// entire pool is walled the cursor still returns the earliest-reopening key so
// prechecks and listings keep working.
func TestNextSkipsQuotaWalledKey(t *testing.T) {
	pool := &upstreamNodes{
		nodes:    []*upstreamNode{{key: "walled-1800"}, {key: "walled-7200"}, {key: "ready"}},
		cooldown: time.Minute,
	}
	future := time.Now().Add(2 * time.Hour).UnixNano()
	pool.nodes[0].quotaUntil.Store(future)
	pool.nodes[0].cooldownUntil.Store(time.Now().Add(-time.Second).UnixNano())
	pool.nodes[1].quotaUntil.Store(future + int64(time.Hour))
	pool.nodes[1].cooldownUntil.Store(time.Now().Add(-time.Second).UnixNano())

	cursor := pool.Cursor()
	first := cursor.Next()
	if first == nil || first.key != "ready" {
		t.Fatalf("first cursor key = %v, want ready", first)
	}

	// Fully walled pool: still returns a key, preferring the earliest horizon.
	pool.nodes[2].quotaUntil.Store(future + 2*int64(time.Hour))
	pool.nodes[2].cooldownUntil.Store(time.Now().Add(-time.Second).UnixNano())
	walledCursor := pool.Cursor()
	walled := walledCursor.Next()
	if walled == nil || walled.key != "walled-1800" {
		t.Fatalf("all-walled cursor key = %v, want earliest walled-1800", walled)
	}
}

// TestMarkListingSuccessKeepsQuotaWall ensures a successful /v1/models listing
// cannot reopen a wall: the response is not an inference.
func TestMarkListingSuccessKeepsQuotaWall(t *testing.T) {
	pool := &upstreamNodes{nodes: []*upstreamNode{{key: "k"}}, cooldown: time.Minute}
	pool.MarkFailure(pool.nodes[0], quotaWallResponse(t, "3600"), nil)
	if pool.nodes[0].quotaUntil.Load() == 0 {
		t.Fatal("429 did not set quotaUntil")
	}
	pool.MarkListingSuccess(pool.nodes[0])
	if pool.nodes[0].quotaUntil.Load() == 0 {
		t.Fatal("listing cleared the quota wall")
	}
	pool.MarkSuccess(pool.nodes[0])
	if pool.nodes[0].quotaUntil.Load() != 0 {
		t.Fatal("real success did not clear the quota wall")
	}
}
