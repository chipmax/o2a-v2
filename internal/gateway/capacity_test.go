package gateway

import (
	"testing"
	"time"

	"opencode2api/internal/warp"
)

// TestCapacityEstimateCountsBucketsNotExits pins the rough estimator: two
// exits in one /64 are one bucket, and a local quota hit marks it spent.
func TestCapacityEstimateCountsBucketsNotExits(t *testing.T) {
	now := time.Now().UTC()
	controller := &RuntimeController{}
	empty := controller.CapacityEstimate()
	if empty.IPsObserved != 0 || empty.IPsUsable != 0 || empty.Note == "" {
		t.Fatalf("empty estimate = %+v", empty)
	}

	board := &quotaBoard{}
	board.entries = make(map[string]*EgressQuotaEntry)
	board.record("warp:2a09:bac5:d45a:263c::1/64", "de", 200, false, 0, now)
	board.record("warp:2a09:bac5:d45a:263c::2/64", "de", 200, false, 0, now)
	board.record("warp:2a09:bac5:d45b:2646::1/64", "de", 429, true, 3600*time.Second, now)
	states := []warp.ExitState{{Egress: "warp:2a09:bac5:d45a:263c::1/64"}, {Egress: "warp:2a09:bac5:d45b:2646::1/64"}}
	snapshot := board.snapshot(states, now)

	seen, spent := make(map[string]bool), make(map[string]bool)
	requests, hits := 0, 0
	for _, entry := range snapshot.Exits {
		key := prefixLabel(entry.Egress)
		if key == "" {
			continue
		}
		seen[key] = true
		if entry.QuotaHits > 0 {
			spent[key] = true
		}
		requests += entry.Requests
		hits += entry.QuotaHits
	}
	if len(seen) != 2 || len(spent) != 1 {
		t.Fatalf("buckets = %d observed / %d spent, want 2 / 1", len(seen), len(spent))
	}
	if requests != 3 || hits != 1 {
		t.Fatalf("counters = requests %d hits %d, want 3 / 1", requests, hits)
	}
}
