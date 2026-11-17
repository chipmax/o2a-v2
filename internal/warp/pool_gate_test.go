package warp

import (
	"strings"
	"testing"
	"time"
)

func TestLaneGateAllowsTheFirstRefusals(t *testing.T) {
	var gate laneGate
	now := time.Unix(1_700_000_000, 0)
	for want := 1; want <= laneGateFreeRefusals; want++ {
		refusals, pause := gate.refused(now)
		if refusals != want {
			t.Fatalf("refusals = %d, want %d", refusals, want)
		}
		if pause != 0 {
			t.Fatalf("refusal %d paused registrations for %s, want none", want, pause)
		}
		if until, _, gated := gate.state(now); gated {
			t.Fatalf("gate closed at %s after only %d refusals", until, want)
		}
	}
}

func TestLaneGateEscalatesAndCaps(t *testing.T) {
	var gate laneGate
	now := time.Unix(1_700_000_000, 0)
	for range laneGateFreeRefusals {
		gate.refused(now)
	}
	want := []time.Duration{
		5 * time.Minute,
		10 * time.Minute,
		20 * time.Minute,
		40 * time.Minute,
		time.Hour,
		time.Hour,
	}
	for index, expected := range want {
		_, pause := gate.refused(now)
		if pause != expected {
			t.Fatalf("escalation step %d = %s, want %s", index+1, pause, expected)
		}
	}
	until, refusals, gated := gate.state(now)
	if !gated || refusals != laneGateFreeRefusals+len(want) {
		t.Fatalf("gate state = (%s, %d, %v), want closed after %d refusals", until, refusals, gated, laneGateFreeRefusals+len(want))
	}
	if _, _, gated := gate.state(until.Add(time.Second)); gated {
		t.Fatal("gate still closed after its pause ended")
	}
}

func TestLaneGateAdmissionReopensIt(t *testing.T) {
	var gate laneGate
	now := time.Unix(1_700_000_000, 0)
	for range laneGateFreeRefusals + 1 {
		gate.refused(now)
	}
	if _, _, gated := gate.state(now); !gated {
		t.Fatal("gate did not close after the refusals")
	}
	gate.admitted()
	if until, refusals, gated := gate.state(now); gated || refusals != 0 || !until.IsZero() {
		t.Fatalf("gate after an admission = (%s, %d, %v), want open and reset", until, refusals, gated)
	}
}

func TestLaneGatedErrorPacesTheRefill(t *testing.T) {
	err := &laneGatedError{until: time.Now().Add(90 * time.Second), refusals: 4}
	if pause := registrationPause(err); pause <= 0 || pause > 2*time.Minute {
		t.Fatalf("registrationPause = %s, want about 90s", pause)
	}
	if message := err.Error(); !strings.Contains(message, "free lane") || !strings.Contains(message, "4") {
		t.Fatalf("error message = %q, want the refusal count and the lane", message)
	}
}
