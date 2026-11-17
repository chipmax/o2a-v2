package gateway

import (
	"encoding/json"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

// UsageDay is one UTC day of traffic as the dashboard needs it: how much ran,
// how much the upstream refused, and where it left from. The upstream resets a
// free-tier bucket at 00:00 UTC, so days are UTC days too.
type UsageDay struct {
	Date       string         `json:"date"`
	Requests   int            `json:"requests"`
	Success    int            `json:"success"`
	QuotaHits  int            `json:"quota_hits"`
	Forbidden  int            `json:"forbidden"`
	TooMany    int            `json:"too_many"`
	Errors     int            `json:"errors"`
	Models     map[string]int `json:"models,omitempty"`
	Egress     map[string]int `json:"egress,omitempty"`
	Upstreams  map[string]int `json:"upstreams,omitempty"`
	FirstSeen  time.Time      `json:"first_seen,omitempty"`
	LastSeen   time.Time      `json:"last_seen,omitempty"`
	HasTraffic bool           `json:"-"`
}

// UsageHistory is the multi-day ledger behind the heatmap, the /64 economics
// table and the fingerprint canary. It is process-wide (like the quota board)
// so a hot reload keeps it, and it persists across restarts.
type UsageHistory struct {
	mu       sync.Mutex
	days     map[string]*UsageDay
	path     string
	lastSave time.Time
}

var usageHistory = newUsageHistory()

func newUsageHistory() *UsageHistory {
	return &UsageHistory{days: make(map[string]*UsageDay)}
}

type usageHistoryFile struct {
	Version int                  `json:"version"`
	Days    map[string]*UsageDay `json:"days"`
}

// ConfigureUsageHistory attaches the persistence file and loads the kept days.
func ConfigureUsageHistory(path string) { usageHistory.configurePath(path) }

// SaveUsageHistory flushes the ledger; shutdown calls it after listeners close.
func SaveUsageHistory() { usageHistory.save() }

func (h *UsageHistory) configurePath(path string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.path == path {
		return
	}
	h.path = path
	if path == "" {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var file usageHistoryFile
	if json.Unmarshal(data, &file) != nil || file.Version != 1 {
		return
	}
	cutoff := time.Now().UTC().AddDate(0, 0, -400).Format("2006-01-02")
	days := make(map[string]*UsageDay, len(file.Days))
	for key, day := range file.Days {
		if key < cutoff {
			continue
		}
		copied := *day
		if copied.Models == nil {
			copied.Models = map[string]int{}
		}
		if copied.Egress == nil {
			copied.Egress = map[string]int{}
		}
		if copied.Upstreams == nil {
			copied.Upstreams = map[string]int{}
		}
		days[key] = &copied
	}
	h.days = days
}

func (h *UsageHistory) save() {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.saveLocked()
}

func (h *UsageHistory) saveLocked() {
	if h.path == "" {
		return
	}
	data, err := json.Marshal(usageHistoryFile{Version: 1, Days: h.days})
	if err != nil {
		return
	}
	tmp := h.path + ".tmp"
	if os.WriteFile(tmp, data, 0o600) != nil {
		return
	}
	if err := os.Rename(tmp, h.path); err != nil {
		_ = os.Remove(tmp)
		return
	}
	h.lastSave = time.Now()
}

func (h *UsageHistory) maybeSaveLocked() {
	if h.path == "" || time.Since(h.lastSave) < 10*time.Second {
		return
	}
	h.saveLocked()
}

func (h *UsageHistory) dayLocked(now time.Time) *UsageDay {
	key := now.UTC().Format("2006-01-02")
	day, ok := h.days[key]
	if !ok {
		day = &UsageDay{Date: key, Models: map[string]int{}, Egress: map[string]int{}, Upstreams: map[string]int{}}
		h.days[key] = day
	}
	return day
}

// RecordAttempt folds one finished upstream attempt into today's ledger. The
// egress label is the WARP egress or proxy name on the attempt; the canary only
// needs status counts per upstream host, so no request content is kept.
func (h *UsageHistory) RecordAttempt(model, upstream, egress string, status int, success bool, now time.Time) {
	if h == nil {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	day := h.dayLocked(now)
	day.Requests++
	day.HasTraffic = true
	if day.FirstSeen.IsZero() || now.Before(day.FirstSeen) {
		day.FirstSeen = now.UTC()
	}
	day.LastSeen = now.UTC()
	if success {
		day.Success++
	}
	switch {
	case status == 403:
		day.Forbidden++
	case status == 429:
		day.TooMany++
		day.QuotaHits++
	case status >= 500:
		day.Errors++
	}
	if model != "" {
		day.Models[model]++
	}
	if egress != "" {
		day.Egress[egress]++
	}
	if upstream != "" {
		day.Upstreams[upstream]++
	}
	h.maybeSaveLocked()
}

// UsageDayView is one heatmap cell, already reduced to the ratios the UI shows.
type UsageDayView struct {
	Date          string  `json:"date"`
	Requests      int     `json:"requests"`
	Success       int     `json:"success"`
	QuotaHits     int     `json:"quota_hits"`
	Forbidden     int     `json:"forbidden"`
	TooMany       int     `json:"too_many"`
	Errors        int     `json:"errors"`
	ForbiddenRate float64 `json:"forbidden_rate"`
	QuotaHitRate  float64 `json:"quota_hit_rate"`
}

// CanaryView answers "did the upstream change its gate?": a day whose
// forbidden rate jumps far above its usual level is the signal, not a single
// 403.
type CanaryView struct {
	Days          []UsageDayView `json:"days"`
	Today         UsageDayView   `json:"today"`
	ForbiddenRate float64        `json:"forbidden_rate"`
	QuotaHitRate  float64        `json:"quota_hit_rate"`
	Verdict       string         `json:"verdict"`
	Upstreams     map[string]int `json:"upstreams,omitempty"`
}

// Snapshot returns the last days (including today) in ascending order plus a
// canary verdict for today.
func (h *UsageHistory) Snapshot(days int, now time.Time) CanaryView {
	view := CanaryView{Upstreams: map[string]int{}}
	if h == nil {
		return view
	}
	if days < 1 || days > 400 {
		days = 30
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	today := h.dayLocked(now)
	keys := make([]string, 0, len(h.days))
	for key := range h.days {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	if len(keys) > days {
		keys = keys[len(keys)-days:]
	}
	var total, forbidden, quotaHits int
	for _, key := range keys {
		day := h.days[key]
		item := UsageDayView{
			Date: day.Date, Requests: day.Requests, Success: day.Success,
			QuotaHits: day.QuotaHits, Forbidden: day.Forbidden, TooMany: day.TooMany, Errors: day.Errors,
		}
		if day.Requests > 0 {
			item.ForbiddenRate = float64(day.Forbidden) / float64(day.Requests)
			item.QuotaHitRate = float64(day.QuotaHits) / float64(day.Requests)
		}
		view.Days = append(view.Days, item)
		total += day.Requests
		forbidden += day.Forbidden
		quotaHits += day.QuotaHits
	}
	todayView := UsageDayView{
		Date: today.Date, Requests: today.Requests, Success: today.Success,
		QuotaHits: today.QuotaHits, Forbidden: today.Forbidden, TooMany: today.TooMany, Errors: today.Errors,
	}
	if today.Requests > 0 {
		todayView.ForbiddenRate = float64(today.Forbidden) / float64(today.Requests)
		todayView.QuotaHitRate = float64(today.QuotaHits) / float64(today.Requests)
	}
	view.Today = todayView
	if total > 0 {
		view.ForbiddenRate = float64(forbidden) / float64(total)
		view.QuotaHitRate = float64(quotaHits) / float64(total)
	}
	for name, count := range today.Upstreams {
		view.Upstreams[name] = count
	}
	// The gate signal: today far above the period's baseline, or a flood of
	// 403s that no retry could fix. Thresholds are deliberately blunt; the
	// point is an early warning, not a diagnosis.
	switch {
	case todayView.Requests >= 10 && todayView.ForbiddenRate >= 0.5:
		view.Verdict = "blocked"
	case todayView.Requests >= 20 && view.ForbiddenRate > 0 && todayView.ForbiddenRate > view.ForbiddenRate*3:
		view.Verdict = "shift"
	case todayView.Requests > 0 && todayView.QuotaHitRate >= 0.5:
		view.Verdict = "quota"
	default:
		view.Verdict = "normal"
	}
	return view
}

// EgressInsights is the single payload the egress page reads: the live /64
// board (with rate and projection), the multi-day ledger and the canary.
type EgressInsights struct {
	Quota EgressQuotaSnapshot `json:"quota"`
	Usage CanaryView          `json:"usage"`
	Days  int                 `json:"days"`
}

// prefixLabel reduces a proxy/egress label to the /64 quota scope the upstream
// counts. Non-IPv6 labels (proxies, "direct") are returned unchanged.
func prefixLabel(label string) string {
	label = strings.TrimSpace(label)
	label = strings.TrimPrefix(label, "warp:")
	if !strings.Contains(label, ":") {
		return label
	}
	parts := strings.Split(label, ":")
	if len(parts) >= 4 {
		return strings.Join(parts[:4], ":")
	}
	return label
}
