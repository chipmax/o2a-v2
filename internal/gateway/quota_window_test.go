package gateway

import (
	"testing"
	"time"
)

func TestQuotaResetAfterCountsToUTCMidnight(t *testing.T) {
	now := time.Date(2026, 11, 8, 20, 0, 0, 0, time.UTC)
	if got, want := quotaResetAfter(now), 4*time.Hour; got != want {
		t.Fatalf("quotaResetAfter(%s) = %s, want %s", now, got, want)
	}
	local := time.Date(2026, 11, 9, 2, 0, 0, 0, time.FixedZone("UTC+7", 7*3600))
	if got, want := quotaResetAfter(local), 5*time.Hour; got != want {
		t.Fatalf("quotaResetAfter(%s) = %s, want %s", local, got, want)
	}
}

func TestQuotaResetAfterClampsTheWindow(t *testing.T) {
	late := time.Date(2026, 11, 8, 23, 59, 0, 0, time.UTC)
	if got := quotaResetAfter(late); got != freeTierQuotaWindowMin {
		t.Fatalf("one minute before reset = %s, want %s", got, freeTierQuotaWindowMin)
	}
	midnight := time.Date(2026, 11, 9, 0, 0, 0, 0, time.UTC)
	if got := quotaResetAfter(midnight); got != freeTierQuotaWindowMax {
		t.Fatalf("at reset = %s, want %s", got, freeTierQuotaWindowMax)
	}
}
