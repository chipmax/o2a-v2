package warp

import (
	"testing"
	"time"
)

func TestNextUTCReset(t *testing.T) {
	now := time.Date(2026, 11, 19, 15, 30, 0, 0, time.UTC)
	got := nextUTCReset(now)
	want := time.Date(2026, 11, 20, 0, 0, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("nextUTCReset = %s, want %s", got, want)
	}
}

func TestBurnedPauseBlocksNewRegistrations(t *testing.T) {
	p := &Pool{burn: newBurnState()}
	p.burn.burnedPausedUntil = time.Now().Add(time.Hour)
	if until := p.BurnedPaused(time.Now()); until.IsZero() {
		t.Fatal("burned pause must be visible")
	}
	if until := p.BurnedPaused(time.Now().Add(2 * time.Hour)); !until.IsZero() {
		t.Fatalf("expired burn pause still reported: %s", until)
	}
}

func TestSpentLifetimeSurvivesCoolingExpiry(t *testing.T) {
	p := &Pool{
		burn:     newBurnState(),
		cooling:  map[string]time.Time{},
		reserved: map[string]bool{},
	}
	now := time.Now()
	key := "2a09:bac5::/64"
	p.burn.spentLifetime[key] = now.Add(8 * time.Hour)
	// Simulate the usable check from createExit after cooling expired.
	spent := p.burn.spentLifetime[key].After(now)
	usable := !p.reserved[key] && !p.cooling[key].After(now) && !spent
	if usable {
		t.Fatal("spent /64 became usable after short cooling expired")
	}
	// After the lifetime entry expires (at UTC reset) it becomes usable again.
	after := now.Add(9 * time.Hour)
	spent = p.burn.spentLifetime[key].After(after)
	usable = !p.reserved[key] && !p.cooling[key].After(after) && !spent
	if !usable {
		t.Fatal("spent /64 was not reusable after its lifetime entry expired")
	}
}
