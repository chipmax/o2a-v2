package gateway

import (
	"encoding/json"
	"os"
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
	FirstSeen      time.Time `json:"first_seen,omitempty"`
	LastQuotaAt    time.Time `json:"last_quota_at,omitempty"`
	RetryAfterSecs int       `json:"retry_after_seconds,omitempty"`
	State          string    `json:"state"`
	CoolingSeconds int       `json:"cooling_seconds,omitempty"`
	ResetAt        time.Time `json:"reset_at"`
	RatePerMinute  float64   `json:"rate_per_min,omitempty"`
	Projected      int       `json:"projected,omitempty"`
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
// answered and how often it refused. Counters are scoped to the UTC day the
// upstream uses and are persisted so a restart in the same day keeps the
// budget page honest.
type quotaBoard struct {
	mu       sync.Mutex
	entries  map[string]*EgressQuotaEntry
	path     string
	lastSave time.Time
	day      string
}

// egressQuota is process-wide so a hot reload does not erase the board.
var egressQuota = newQuotaBoard()

func newQuotaBoard() *quotaBoard {
	return &quotaBoard{entries: make(map[string]*EgressQuotaEntry), day: utcDay(time.Now())}
}

type quotaBoardFile struct {
	Version int                         `json:"version"`
	Day     string                      `json:"day"`
	Entries map[string]EgressQuotaEntry `json:"entries"`
}

// ConfigureEgressQuota attaches the persistence file and restores today's
// counters. Entries from an earlier UTC day are discarded.
func ConfigureEgressQuota(path string) { egressQuota.configurePath(path) }

// SaveEgressQuota flushes the board; shutdown calls it after listeners close.
func SaveEgressQuota() { egressQuota.save() }

func (b *quotaBoard) configurePath(path string) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.path == path {
		return
	}
	b.path = path
	if path == "" {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var file quotaBoardFile
	if json.Unmarshal(data, &file) != nil || file.Version != 1 {
		return
	}
	today := utcDay(time.Now())
	if file.Day != today {
		b.entries = make(map[string]*EgressQuotaEntry)
		b.day = today
		return
	}
	entries := make(map[string]*EgressQuotaEntry, len(file.Entries))
	for key, entry := range file.Entries {
		entry.State = ""
		entry.CoolingSeconds = 0
		entry.ResetAt = time.Time{}
		entries[key] = &entry
	}
	b.entries = entries
	b.day = today
}

func (b *quotaBoard) save() {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.saveLocked(time.Now())
}

func (b *quotaBoard) saveLocked(now time.Time) {
	if b.path == "" {
		return
	}
	entries := make(map[string]EgressQuotaEntry, len(b.entries))
	for key, entry := range b.entries {
		copied := *entry
		copied.State = ""
		copied.CoolingSeconds = 0
		copied.ResetAt = time.Time{}
		entries[key] = copied
	}
	data, err := json.Marshal(quotaBoardFile{Version: 1, Day: b.day, Entries: entries})
	if err != nil {
		return
	}
	tmp := b.path + ".tmp"
	if os.WriteFile(tmp, data, 0o600) != nil {
		return
	}
	if err := os.Rename(tmp, b.path); err != nil {
		_ = os.Remove(tmp)
		return
	}
	b.lastSave = now
}

func (b *quotaBoard) maybeSaveLocked(now time.Time) {
	if b.path == "" || now.Sub(b.lastSave) < 10*time.Second {
		return
	}
	b.saveLocked(now)
}

func (b *quotaBoard) rollLocked(now time.Time) {
	today := utcDay(now)
	if b.day == today {
		return
	}
	b.entries = make(map[string]*EgressQuotaEntry)
	b.day = today
	b.lastSave = time.Time{}
}

// record folds one attempt's outcome into the board. retryAfter carries the
// upstream's Retry-After for a quota answer, so the dashboard can show when the
// bucket is expected back instead of only that it is spent.
func (b *quotaBoard) record(egress, model string, status int, quotaHit bool, retryAfter time.Duration, now time.Time) {
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
	b.rollLocked(now)
	entry, ok := b.entries[key]
	if !ok {
		entry = &EgressQuotaEntry{Egress: egress, Family: family, FirstSeen: now.UTC()}
		b.entries[key] = entry
	}
	entry.Requests++
	if quotaHit || status == 429 {
		entry.QuotaHits++
		entry.LastQuotaAt = now.UTC()
		if retryAfter > 0 {
			entry.RetryAfterSecs = int(retryAfter.Seconds())
		}
	}
	entry.LastStatus = status
	entry.LastSeen = now.UTC()
	b.maybeSaveLocked(now)
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
		b.rollLocked(now)
		for _, entry := range b.entries {
			copied := *entry
			copied.ResetAt = resetAt
			seenFrom := copied.FirstSeen
			if seenFrom.IsZero() {
				seenFrom = copied.LastSeen
			}
			if !seenFrom.IsZero() && copied.Requests > 0 {
				minutes := now.Sub(seenFrom).Minutes()
				if minutes < 1 {
					minutes = 1
				}
				copied.RatePerMinute = float64(copied.Requests) / minutes
				copied.Projected = int(copied.RatePerMinute * resetAt.Sub(now).Minutes())
			}
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
