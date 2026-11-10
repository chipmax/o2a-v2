package gateway

import (
	"testing"
	"time"
)

func TestUsageHistorySnapshotAndCanary(t *testing.T) {
	history := newUsageHistory()
	now := time.Date(2026, 11, 10, 12, 0, 0, 0, time.UTC)
	history.RecordAttempt("model-a", "zen/anonymous", "2603:c024:4526:9797::/64", 200, true, now)
	history.RecordAttempt("model-a", "zen/anonymous", "2603:c024:4526:9797::/64", 429, false, now)
	history.RecordAttempt("model-b", "go/key", "", 403, false, now)
	for index := 0; index < 10; index++ {
		history.RecordAttempt("model-b", "go/key", "", 403, false, now)
	}

	view := history.Snapshot(30, now)
	if len(view.Days) != 1 {
		t.Fatalf("days = %d, want 1", len(view.Days))
	}
	day := view.Days[0]
	if day.Requests != 13 || day.Success != 1 || day.QuotaHits != 1 || day.Forbidden != 11 {
		t.Errorf("unexpected day counters: %+v", day)
	}
	if view.Today.Requests != 13 {
		t.Errorf("today requests = %d", view.Today.Requests)
	}
	if view.Verdict != "blocked" {
		t.Errorf("verdict = %q, want blocked", view.Verdict)
	}
}

func TestUsageHistoryPersistsAcrossConfigure(t *testing.T) {
	dir := t.TempDir()
	path := dir + "/usage.json"
	first := newUsageHistory()
	first.configurePath(path)
	now := time.Now().UTC()
	first.RecordAttempt("m", "zen/anonymous", "2603:1:2:3::/64", 200, true, now)
	first.save()

	second := newUsageHistory()
	second.configurePath(path)
	view := second.Snapshot(30, now)
	if view.Today.Requests != 1 {
		t.Fatalf("restored requests = %d, want 1", view.Today.Requests)
	}
}

func TestPrefixLabel(t *testing.T) {
	cases := map[string]string{
		"warp:2603:c024:4526:9797:aaaa:bbbb:cccc:dddd": "2603:c024:4526:9797",
		"2603:c024:4526:9797::/64":                     "2603:c024:4526:9797",
		"http://127.0.0.1:8080":                        "http://127.0.0.1:8080",
		"direct":                                       "direct",
	}
	for input, want := range cases {
		if got := prefixLabel(input); got != want {
			t.Errorf("prefixLabel(%q) = %q, want %q", input, got, want)
		}
	}
}
