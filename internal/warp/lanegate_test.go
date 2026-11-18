package warp

import (
	"net/http"
	"testing"
	"time"
)

func TestIsGateRefusal(t *testing.T) {
	if !isGateRefusal(http.StatusForbidden, []byte(`{"error":{"message":"FreeTierError: free tier can only be used from within OpenCode"}}`)) {
		t.Fatal("403 shape gate not classified")
	}
	if !isGateRefusal(http.StatusForbidden, []byte("free models aren't available from your current network because of abusive traffic")) {
		t.Fatal("403 network abuse not classified")
	}
	if isGateRefusal(http.StatusTooManyRequests, []byte(`{"error":"Free usage limit reached"}`)) {
		t.Fatal("quota 429 must not close the lane")
	}
	if isGateRefusal(http.StatusOK, []byte("freetiererror")) {
		t.Fatal("success must not close the lane")
	}
}

func TestLaneGateStreakDecays(t *testing.T) {
	gate := &laneGate{}
	gate.refusals = 9
	gate.until = time.Now().Add(-laneGatePauseBase - time.Minute)
	_, refusals, gated := gate.state(time.Now())
	if gated || refusals != 0 {
		t.Fatalf("stale streak not decayed: refusals=%d gated=%v", refusals, gated)
	}
}
