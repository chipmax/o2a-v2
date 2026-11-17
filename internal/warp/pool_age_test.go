package warp

import (
	"testing"
	"time"
)

func TestAgeSweepFor(t *testing.T) {
	tests := []struct {
		name string
		age  time.Duration
		want time.Duration
	}{
		{name: "disabled", age: 0, want: 0},
		{name: "ten minutes", age: 10 * time.Minute, want: ageSweepInterval},
		{name: "one minute", age: time.Minute, want: 30 * time.Second},
		{name: "two seconds floors at one", age: 2 * time.Second, want: time.Second},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if got := ageSweepFor(test.age); got != test.want {
				t.Fatalf("ageSweepFor(%v) = %v, want %v", test.age, got, test.want)
			}
		})
	}
}

func TestNextRotationDelaySpacesVoluntaryRotations(t *testing.T) {
	p := &Pool{rotateCooldown: 90 * time.Second}
	if got := p.nextRotationDelay(); got != 0 {
		t.Fatalf("first delay = %v, want 0", got)
	}
	second := p.nextRotationDelay()
	if second < 80*time.Second || second > 90*time.Second {
		t.Fatalf("second delay = %v, want about 90s", second)
	}
	if got := (&Pool{}).nextRotationDelay(); got != 0 {
		t.Fatalf("delay without cooldown = %v, want 0", got)
	}
}
