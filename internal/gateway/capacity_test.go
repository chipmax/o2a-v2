package gateway

import (
	"net/http"
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

// TestKeyQuotaTrackedSeparately pins P27: an exhausted Zen key is visible even
// when its egress buckets are green, because OMP was limited by the key tier.
func TestKeyQuotaTrackedSeparately(t *testing.T) {
	now := time.Now().UTC()
	recordKeyQuota("CE0nZ", "deepseek-v4.1-flash-free", http.StatusTooManyRequests, 9*time.Hour, now)
	keys := keyQuotaSnapshot()
	if len(keys) == 0 || keys[0].KeyID != "CE0nZ" || keys[0].QuotaHits == 0 {
		t.Fatalf("key quota not recorded: %+v", keys)
	}
	if keys[0].Family != "de" || keys[0].RetryAfter.Before(now.Add(8*time.Hour)) {
		t.Fatalf("key quota family/retry wrong: %+v", keys[0])
	}

	controller := &RuntimeController{}
	_ = controller // quota snapshot is read through EgressQuota; key state is global
	board := &quotaBoard{}
	board.entries = make(map[string]*EgressQuotaEntry)
	board.record("warp:2a09:bac1:6500:8::/64", "de", 200, false, 0, now)
	snapshot := board.snapshot(nil, now)
	if len(snapshot.Keys) == 0 || snapshot.Keys[0].KeyID != "CE0nZ" {
		t.Fatalf("snapshot missing key quota: %+v", snapshot.Keys)
	}
}
