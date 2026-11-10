package gateway

import (
	"path/filepath"
	"testing"
	"time"
)

func TestEgressQuotaBoardSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "egress.json")
	now := time.Now().UTC()
	first := newQuotaBoard()
	first.configurePath(path)
	first.record("2001:db8::/64", "deepseek-v4.1-flash-free", 200, false, now)

	second := newQuotaBoard()
	second.configurePath(path)
	snapshot := second.snapshot(nil, now)
	if len(snapshot.Exits) != 1 || snapshot.Exits[0].Requests != 1 {
		t.Fatalf("restored board = %+v, want one request", snapshot.Exits)
	}
	if snapshot.Exits[0].Egress != "2001:db8::/64" || snapshot.Exits[0].Family != "de" {
		t.Fatalf("restored entry = %+v", snapshot.Exits[0])
	}
}

func TestEgressQuotaBoardResetsAtUTCMidnight(t *testing.T) {
	board := newQuotaBoard()
	board.day = "2000-01-01"
	board.record("2001:db8::/64", "deepseek-v4.1-flash-free", 429, true, time.Now().UTC())

	snapshot := board.snapshot(nil, time.Now().UTC())
	if len(snapshot.Exits) != 1 {
		t.Fatalf("exits = %d, want the new day's entry only", len(snapshot.Exits))
	}
	if snapshot.Exits[0].Requests != 1 || snapshot.Exits[0].QuotaHits != 1 {
		t.Fatalf("new-day counters = %+v, want the old-day counters discarded", snapshot.Exits[0])
	}
}
