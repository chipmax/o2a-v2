package rotation

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"opencode2api/internal/config"
)

func TestRecordThrottlesDiskWritesAndSaveFlushes(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rotation.json")
	m, err := New(path, config.RotationConfig{
		SOTA:               config.RotationGroup{Alias: "sota", Order: []string{"de-a"}},
		Sweet:              config.RotationGroup{Alias: "sweet"},
		FirstOutputSeconds: 42,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	readAttempts := func() []Attempt {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("read state: %v", err)
		}
		var state Snapshot
		if err := json.Unmarshal(data, &state); err != nil {
			t.Fatalf("parse state: %v", err)
		}
		return state.Attempts
	}
	if got := m.FirstOutputSeconds(); got != 42 {
		t.Fatalf("FirstOutputSeconds = %d, want 42", got)
	}
	m.mu.Lock()
	m.lastSave = time.Now()
	m.mu.Unlock()
	m.Record(Attempt{Request: "r1", Alias: "sota", Model: "de-a", Number: 1, Outcome: "success"})
	if attempts := readAttempts(); len(attempts) != 0 {
		t.Fatalf("record inside the throttle window wrote %d attempts to disk", len(attempts))
	}
	m.Save()
	if attempts := readAttempts(); len(attempts) != 1 {
		t.Fatalf("Save persisted %d attempts, want 1", len(attempts))
	}
	m.mu.Lock()
	m.lastSave = time.Now().Add(-rotationSaveInterval - time.Second)
	m.mu.Unlock()
	m.Record(Attempt{Request: "r2", Alias: "sota", Model: "de-a", Number: 2, Outcome: "success"})
	if attempts := readAttempts(); len(attempts) != 2 {
		t.Fatalf("record after the throttle window persisted %d attempts, want 2", len(attempts))
	}
	if got := m.PersistenceError(); got != "" {
		t.Fatalf("PersistenceError = %q, want empty", got)
	}
}

func TestQuotaPauseCoversSiblingModels(t *testing.T) {
	m, err := New("", config.RotationConfig{
		SOTA:               config.RotationGroup{Alias: "sota", Order: []string{"de-a", "de-b", "glm-c"}},
		Sweet:              config.RotationGroup{Alias: "sweet"},
		FirstOutputSeconds: 30,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	m.Sync(map[string]bool{"de-a": true, "de-b": true, "glm-c": true})

	token, ok := m.Select("sota", nil)
	if !ok || token.Model != "de-a" {
		t.Fatalf("first selection = %q, ok=%v, want de-a", token.Model, ok)
	}
	m.FailQuota(token, time.Hour, "rate_limited")

	snapshot := m.Snapshot()
	if snapshot.Groups[0].Paused["de-a"].IsZero() || snapshot.Groups[0].Paused["de-b"].IsZero() {
		t.Fatal("quota failure did not pause both models in the family")
	}
	if !snapshot.Groups[0].Paused["glm-c"].IsZero() {
		t.Fatal("quota failure paused a model from another family")
	}
	if snapshot.Groups[0].Current != "glm-c" {
		t.Fatalf("current = %q, want glm-c after family pause", snapshot.Groups[0].Current)
	}
	next, ok := m.Select("sota", nil)
	if !ok || next.Model != "glm-c" {
		t.Fatalf("next selection = %q, ok=%v, want glm-c", next.Model, ok)
	}
}

func TestSyncKeepsCuratedSOTAAndDiscoversSweet(t *testing.T) {
	m, err := New("", config.RotationConfig{
		SOTA:               config.RotationGroup{Alias: "sota", Order: []string{"sota-a", "gone"}},
		Sweet:              config.RotationGroup{Alias: "sweet"},
		FirstOutputSeconds: 30,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	m.Sync(map[string]bool{"sota-a": true, "new-free": true})

	snapshot := m.Snapshot()
	if len(snapshot.Groups) != 2 {
		t.Fatalf("groups = %d, want 2", len(snapshot.Groups))
	}
	sota, sweet := snapshot.Groups[0], snapshot.Groups[1]
	if !sota.Available["sota-a"] {
		t.Fatal("curated sota model is not available")
	}
	if sota.Available["gone"] {
		t.Fatal("a model missing from the catalog is still available")
	}
	if sota.Available["new-free"] {
		t.Fatal("sota silently gained a model outside its curated order")
	}
	if !sweet.Available["new-free"] {
		t.Fatal("sweet did not discover the new catalog model")
	}
	if sweet.Available["sota-a"] {
		t.Fatal("sweet duplicated a model curated into sota")
	}
}
