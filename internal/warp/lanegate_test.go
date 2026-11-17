package warp

import (
	"path/filepath"
	"testing"
	"time"
)

// TestLaneGatePersistsAcrossRestart covers the case that made a restart mint a
// device the previous process had just learned was refused: the pause must
// survive, and an admission must clear it on disk too.
func TestLaneGatePersistsAcrossRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.lane")
	gate := newLaneGate(path)
	now := time.Now()
	for index := 0; index < laneGateFreeRefusals+1; index++ {
		gate.refused(now)
	}
	if gate.until.IsZero() {
		t.Fatal("expected a pause after the refusal streak")
	}

	restored := newLaneGate(path)
	if restored.refusals != gate.refusals || !restored.until.Equal(gate.until) {
		t.Fatalf("restored %d/%s, want %d/%s", restored.refusals, restored.until, gate.refusals, gate.until)
	}

	restored.admitted()
	cleared := newLaneGate(path)
	if cleared.refusals != 0 || !cleared.until.IsZero() {
		t.Fatalf("admission did not clear the saved gate: %d/%s", cleared.refusals, cleared.until)
	}
}

// TestLaneGateExpiredPauseKeepsCount checks that an expired pause no longer
// blocks registration but its refusal count is still reported.
func TestLaneGateExpiredPauseKeepsCount(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state.lane")
	gate := newLaneGate(path)
	gate.refusals = 3
	gate.until = time.Now().Add(-time.Minute)
	gate.saveLocked()

	restored := newLaneGate(path)
	if restored.refusals != 3 {
		t.Fatalf("refusals = %d, want 3", restored.refusals)
	}
	if restored.until.After(time.Now()) {
		t.Fatalf("expired pause was restored as future: %s", restored.until)
	}
}
