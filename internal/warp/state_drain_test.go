package warp

import (
	"path/filepath"
	"testing"
	"time"
)

// TestDrainMarkerSurvivesRestart pins the egress marker: a /64 spent by a
// previous process must stay in the cooling map after a restart, otherwise a
// fresh registration can land on the same spent bucket.
func TestDrainMarkerSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "warp-state.json")
	store := newRegistrationStore(path, 4)
	store.remember(registration{
		ID: "dev-1", PrivateKey: "k", AddressV6: "2606:4700:110::1/128", PeerPublicKey: "peer",
	})

	now := time.Now().UTC().Truncate(time.Second)
	store.markDrain("dev-1", "2a09:bac5:d45a:263c", "429 spare limit reached", now)
	if got := store.DrainedKeys(now.Add(time.Minute), time.Hour); len(got) != 1 {
		t.Fatalf("DrainedKeys = %v, want one marked prefix", got)
	}

	// A fresh store reading the same file is what a restart does.
	reopened := newRegistrationStore(path, 4)
	got := reopened.DrainedKeys(now.Add(time.Minute), time.Hour)
	if until, ok := got["2a09:bac5:d45a:263c"]; !ok || !until.After(now) {
		t.Fatalf("marker lost across restart: %v", got)
	}
	if expired := reopened.DrainedKeys(now.Add(2*time.Hour), time.Hour); len(expired) != 0 {
		t.Fatalf("expired marker still present: %v", expired)
	}
}
