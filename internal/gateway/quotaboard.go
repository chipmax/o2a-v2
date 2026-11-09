package gateway

import (
	"sort"
	"sync"
	"time"

	"opencode2api/internal/warp"
)

// EgressQuotaEntry is one free-tier bucket as the gateway has observed it. The
// upstream counts per egress /64 and per model family (the first two characters
// of the model ID), so that pair — not the process or the key — is the unit a
// dashboard has to show.
type EgressQuotaEntry struct {
	Egress         string    `json:"egress"`
	Family         string    `json:"family,omitempty"`
	Requests       int       `json:"requests"`
	QuotaHits      int       `json:"quota_hits"`
	LastStatus     int       `json:"last_status,omitempty"`
	LastSeen       time.Time `json:"last_seen,omitempty"`
	State          string    `json:"state"`
	CoolingSeconds int       `json:"cooling_seconds,omitempty"`
	ResetAt        time.Time `json:"reset_at"`
}

// EgressQuotaSnapshot is what the WebUI budget page renders.
type EgressQuotaSnapshot struct {
	ResetAt  time.Time          `json:"reset_at"`
	Families []string           `json:"families"`
	Exits    []EgressQuotaEntry `json:"exits"`
	Cooling  int                `json:"cooling"`
	Active   int                `json:"active"`
	Observed int                `json:"observed"`
}

// quotaBoard remembers, per egress and model family, how often the free tier
// answered and how often it refused. It is in-memory on purpose: the counters
// reset at the same UTC midnight the upstream uses.
type quotaBoard struct {
	mu      sync.Mutex
	entries map[string]*EgressQuotaEntry
}

func newQuotaBoard() *quotaBoard {
	return &quotaBoard{entries: make(map[string]*EgressQuotaEntry)}
}

// record folds one attempt's outcome into the board.
func (b *quotaBoard) record(egress, model string, status int, quotaHit bool, now time.Time) {
	if b == nil || egress == "" {
		return
	}
	family := model
	if len(family) > 2 {
		family = family[:2]
	}
	key := egress + "|" + family
	b.mu.Lock()
	defer b.mu.Unlock()
	entry, ok := b.entries[key]
	if !ok {
		entry = &EgressQuotaEntry{Egress: egress, Family: family}
		b.entries[key] = entry
	}
	entry.Requests++
	if quotaHit || status == 429 {
		entry.QuotaHits++
	}
	entry.LastStatus = status
	entry.LastSeen = now.UTC()
}

// snapshot merges the counters with the pool's live exit states: an exit the
// pool is cooling is "cooling" even when its last attempt looked fine, because
// the pool retires an exit faster than a counter sees it.
func (b *quotaBoard) snapshot(states []warp.ExitState, now time.Time) EgressQuotaSnapshot {
	resetAt := now.UTC().Truncate(24 * time.Hour).Add(24 * time.Hour)
	snapshot := EgressQuotaSnapshot{ResetAt: resetAt}
	cooling := make(map[string]time.Time, len(states))
	active := make(map[string]bool, len(states))
	for _, state := range states {
		if state.Cooling {
			cooling[state.EgressKey] = state.CoolingUntil
			continue
		}
		active[state.EgressKey] = true
	}
	seen := make(map[string]bool)
	families := map[string]bool{}
	if b != nil {
		b.mu.Lock()
		for _, entry := range b.entries {
			copied := *entry
			copied.ResetAt = resetAt
			copied.State = "idle"
			if until, ok := cooling[entry.Egress]; ok {
				copied.State = "cooling"
				copied.CoolingSeconds = int(until.Sub(now).Seconds())
			} else if active[entry.Egress] {
				copied.State = "active"
			}
			snapshot.Exits = append(snapshot.Exits, copied)
			seen[entry.Egress] = true
			families[entry.Family] = true
		}
		b.mu.Unlock()
	}
	// Cooling exits that never answered still deserve a row: that is where the
	// spent budget is.
	for egress, until := range cooling {
		if !seen[egress] {
			snapshot.Exits = append(snapshot.Exits, EgressQuotaEntry{
				Egress: egress, State: "cooling",
				CoolingSeconds: int(until.Sub(now).Seconds()), ResetAt: resetAt,
			})
		}
	}
	for egress := range active {
		if !seen[egress] {
			snapshot.Exits = append(snapshot.Exits, EgressQuotaEntry{Egress: egress, State: "active", ResetAt: resetAt})
		}
	}
	sort.Slice(snapshot.Exits, func(i, j int) bool {
		a, c := snapshot.Exits[i], snapshot.Exits[j]
		if a.Egress != c.Egress {
			return a.Egress < c.Egress
		}
		return a.Family < c.Family
	})
	for family := range families {
		snapshot.Families = append(snapshot.Families, family)
	}
	sort.Strings(snapshot.Families)
	snapshot.Cooling = len(cooling)
	snapshot.Active = len(active)
	snapshot.Observed = len(snapshot.Exits)
	return snapshot
}
