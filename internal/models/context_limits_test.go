package models

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"opencode2api/internal/config"
)

func TestAdvertisedRaisesCatalogFromSuccess(t *testing.T) {
	store, err := OpenContextLimits("")
	if err != nil {
		t.Fatalf("OpenContextLimits: %v", err)
	}
	if got := store.Advertised("m", config.TierGo, 200_000, 0); got != 200_000 {
		t.Fatalf("initial advertised = %d, want 200000", got)
	}
	store.ObserveSuccess("m", config.TierGo, 330_055, 1_500_000)
	if got := store.Advertised("m", config.TierGo, 200_000, 0); got != 313_000 {
		t.Fatalf("advertised after success = %d, want 313000", got)
	}
	if got := store.Advertised("m", config.TierZen, 200_000, 0); got != 200_000 {
		t.Fatalf("tier leaked: zen advertised = %d, want 200000", got)
	}
}

func TestAdvertisedSeedWinsOverSmallerObservation(t *testing.T) {
	store, _ := OpenContextLimits("")
	store.ObserveSuccess("m", config.TierGo, 305_000, 1_300_000)
	if got := store.Advertised("m", config.TierGo, 200_000, 320_000); got != 320_000 {
		t.Fatalf("advertised = %d, want seed 320000", got)
	}
}

func TestAdvertisedExplicitCeilingLowers(t *testing.T) {
	store, _ := OpenContextLimits("")
	estimate, applied := store.ObserveRejection("m", config.TierGo, 0,
		[]byte(`{"error":{"message":"maximum context length is 300000 tokens"}}`), 320_000)
	if estimate != 0 || !applied {
		t.Fatalf("ObserveRejection = (%d, %v), want (0, true)", estimate, applied)
	}
	if got := store.Advertised("m", config.TierGo, 200_000, 320_000); got != 285_000 {
		t.Fatalf("advertised = %d, want 285000", got)
	}
}

func TestRejectionInsideProvenWindowLowers(t *testing.T) {
	store, _ := OpenContextLimits("")
	store.ObserveSuccess("m", config.TierGo, 330_055, 1_500_000)
	estimate, applied := store.ObserveRejection("m", config.TierGo, 1_411_600,
		[]byte(`{"error":{"message":"prompt is too long"}}`), 320_000)
	if estimate != 319_922 || !applied {
		t.Fatalf("ObserveRejection = (%d, %v), want (319922, true)", estimate, applied)
	}
	if got := store.Observed("m", config.TierGo); got != 0 {
		t.Fatalf("observed = %d, want cleared", got)
	}
	if got := store.Advertised("m", config.TierGo, 200_000, 320_000); got != 287_000 {
		t.Fatalf("advertised = %d, want 287000", got)
	}
}

func TestRejectionAboveProvenWindowIsIgnored(t *testing.T) {
	store, _ := OpenContextLimits("")
	store.ObserveSuccess("m", config.TierGo, 330_055, 1_500_000)
	estimate, applied := store.ObserveRejection("m", config.TierGo, 1_500_000,
		[]byte(`{"error":{"message":"prompt is too long"}}`), 320_000)
	if estimate != 339_957 || applied {
		t.Fatalf("ObserveRejection = (%d, %v), want (339957, false)", estimate, applied)
	}
	if got := store.Advertised("m", config.TierGo, 200_000, 320_000); got != 320_000 {
		t.Fatalf("advertised = %d, want 320000", got)
	}
}

func TestUnverifiedSeedLowersAfterRejection(t *testing.T) {
	store, _ := OpenContextLimits("")
	store.ObserveSuccess("m", config.TierGo, 100_000, 400_000)
	estimate, applied := store.ObserveRejection("m", config.TierGo, 1_282_000,
		[]byte(`{"error":{"message":"prompt is too long"}}`), 320_000)
	if estimate != 330_115 || !applied {
		t.Fatalf("ObserveRejection = (%d, %v), want (330115, true)", estimate, applied)
	}
	if got := store.Advertised("m", config.TierGo, 200_000, 320_000); got != 297_000 {
		t.Fatalf("advertised = %d, want 297000", got)
	}
}

func TestSuccessNearRejectionClearsIt(t *testing.T) {
	store, _ := OpenContextLimits("")
	store.ObserveSuccess("m", config.TierGo, 100_000, 400_000)
	store.ObserveRejection("m", config.TierGo, 1_282_000,
		[]byte(`{"error":{"message":"prompt is too long"}}`), 320_000)
	store.ObserveSuccess("m", config.TierGo, 330_000, 1_500_000)
	if got := store.Observed("m", config.TierGo); got != 330_000 {
		t.Fatalf("observed = %d, want 330000", got)
	}
	if got := store.Advertised("m", config.TierGo, 200_000, 320_000); got != 320_000 {
		t.Fatalf("advertised = %d, want 320000", got)
	}
}

func TestRejectionWithoutCalibrationDoesNothing(t *testing.T) {
	store, _ := OpenContextLimits("")
	estimate, applied := store.ObserveRejection("m", config.TierGo, 1_000_000,
		[]byte(`{"error":{"message":"prompt is too long"}}`), 320_000)
	if estimate != 0 || applied {
		t.Fatalf("ObserveRejection = (%d, %v), want (0, false)", estimate, applied)
	}
}

func TestContextLimitsPersist(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	store, err := OpenContextLimits(path)
	if err != nil {
		t.Fatalf("OpenContextLimits: %v", err)
	}
	store.ObserveSuccess("m", config.TierGo, 330_055, 1_500_000)
	store.Save()

	restored, err := OpenContextLimits(path)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	if got := restored.Observed("m", config.TierGo); got != 330_055 {
		t.Fatalf("observed after reopen = %d, want 330055", got)
	}
	estimate, applied := restored.ObserveRejection("m", config.TierGo, 1_411_600,
		[]byte(`{"error":{"message":"prompt is too long"}}`), 320_000)
	if estimate != 319_922 || !applied {
		t.Fatalf("calibration did not survive restart: (%d, %v)", estimate, applied)
	}
}

func TestContextLimitsCorruptFileIsRecoverable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.json")
	if err := os.WriteFile(path, []byte("{not json"), 0o600); err != nil {
		t.Fatal(err)
	}
	store, err := OpenContextLimits(path)
	if err == nil {
		t.Fatal("expected an error for a corrupt file")
	}
	if store == nil {
		t.Fatal("expected a usable empty store")
	}
	if got := store.Advertised("m", config.TierGo, 200_000, 0); got != 200_000 {
		t.Fatalf("advertised = %d, want 200000", got)
	}
}

func TestParseContextLimit(t *testing.T) {
	cases := []struct {
		body string
		want int
	}{
		{"maximum context length is 128000 tokens", 128_000},
		{"context window of 1,000,000 tokens", 1_000_000},
		{"maximum context length is 200k tokens", 200_000},
		{"prompt is too long: 331001 tokens > 330000 maximum", 330_000},
		{"prompt is too long: 347790 tokens > 320000 maximum", 320_000},
		{"bad request", 0},
	}
	for _, tc := range cases {
		if got := parseContextLimit([]byte(tc.body)); got != tc.want {
			t.Errorf("parseContextLimit(%q) = %d, want %d", tc.body, got, tc.want)
		}
	}
}

func TestContextLimitsConcurrentAccess(t *testing.T) {
	store, _ := OpenContextLimits("")
	var wg sync.WaitGroup
	for worker := range 32 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 250 {
				prompt := 100_000 + worker*1_000 + i
				store.ObserveSuccess("m", config.TierGo, prompt, 400_000)
				store.ObserveRejection("m", config.TierGo, 400_000,
					[]byte("maximum context length is 300000 tokens"), 320_000)
				_ = store.Advertised("m", config.TierGo, 200_000, 320_000)
				_ = store.Observed("m", config.TierGo)
			}
		}()
	}
	wg.Wait()
	// A rejection that lands inside a proven window deliberately clears the
	// old success, so the final value depends on goroutine interleaving. Record
	// one last large success to assert the concurrent board is still writable
	// without pinning an interleaving-dependent observation.
	store.ObserveSuccess("m", config.TierGo, 400_000, 400_000)
	if got := store.Observed("m", config.TierGo); got < 400_000 {
		t.Fatalf("observed = %d, want at least 400000", got)
	}
}

func TestNilContextLimits(t *testing.T) {
	var store *ContextLimits
	if got := store.Advertised("m", config.TierGo, 200_000, 0); got != 200_000 {
		t.Fatalf("advertised = %d, want 200000", got)
	}
	store.ObserveSuccess("m", config.TierGo, 300_000, 1_000_000)
	store.Save()
}

func TestSplitContextKey(t *testing.T) {
	tier, model, ok := splitContextKey("go:provider/model:free")
	if !ok || tier != config.TierGo || model != "provider/model:free" {
		t.Fatalf("splitContextKey = (%q, %q, %v)", tier, model, ok)
	}
	if _, _, ok := splitContextKey("other:m"); ok {
		t.Fatal("unknown tier accepted")
	}
	if _, _, ok := splitContextKey(strings.Repeat(":", 1)); ok {
		t.Fatal("empty key accepted")
	}
}
