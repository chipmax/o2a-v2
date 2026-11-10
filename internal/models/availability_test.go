package models

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestPassiveObservationPersistsTransitionsNotEveryRequest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "availability.json")
	store, err := AvailabilityStore(path)
	if err != nil {
		t.Fatalf("AvailabilityStore: %v", err)
	}
	first := time.Now().UTC().Truncate(time.Second)
	if err := store.RecordPassiveObservation("m", true, 200, "", "zen", first); err != nil {
		t.Fatalf("first observation: %v", err)
	}
	persisted := readAvailability(t, path)
	if got := persisted["m"].CheckedAt; !got.Equal(first) {
		t.Fatalf("first persisted checked_at = %v, want %v", got, first)
	}

	second := first.Add(time.Second)
	if err := store.RecordPassiveObservation("m", true, 200, "", "zen", second); err != nil {
		t.Fatalf("second observation: %v", err)
	}
	if got := store.Get("m").CheckedAt; !got.Equal(second) {
		t.Fatalf("in-memory checked_at = %v, want %v", got, second)
	}
	persisted = readAvailability(t, path)
	if got := persisted["m"].CheckedAt; !got.Equal(first) {
		t.Fatalf("debounced persisted checked_at = %v, want %v", got, first)
	}

	disabled := second.Add(time.Second)
	if err := store.RecordPassiveObservation("m", false, 404, `{"error":{"code":"model_not_found"}}`, "zen", disabled); err != nil {
		t.Fatalf("disable observation: %v", err)
	}
	persisted = readAvailability(t, path)
	if !persisted["m"].Disabled {
		t.Fatal("disable transition was not persisted immediately")
	}
}

func readAvailability(t *testing.T, path string) map[string]Availability {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read availability: %v", err)
	}
	var items map[string]Availability
	if err := json.Unmarshal(data, &items); err != nil {
		t.Fatalf("decode availability: %v", err)
	}
	return items
}
