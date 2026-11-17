package warp

import (
	"net/http"
	"strings"
	"testing"
	"time"
)

func TestObserveRefusalClassification(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		rotate bool
		burned bool
	}{
		{
			name:   "lane gate keeps the exit",
			status: http.StatusForbidden,
			body:   `{"type":"error","error":{"type":"FreeTierError","message":"free tier can only be used from within OpenCode"}}`,
		},
		{
			name:   "shape gate wording keeps the exit",
			status: http.StatusForbidden,
			body:   `Forbidden: free tier can only be used from within OpenCode`,
		},
		{
			name:   "ip abuse rotates",
			status: http.StatusForbidden,
			body:   `{"error":{"message":"free models aren't available from your current network because we've detected abusive traffic from this IP address"}}`,
			rotate: true,
			burned: true,
		},
		{
			name:   "quota 429 rotates",
			status: http.StatusTooManyRequests,
			body:   `{"type":"error","error":{"type":"FreeUsageLimitError","message":"Free usage limit reached"}}`,
			rotate: true,
			burned: true,
		},
		{
			name:   "success keeps the exit",
			status: http.StatusOK,
			body:   `{}`,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			e := &exit{}
			rotate, reason := e.observe(test.status, []byte(test.body))
			if rotate != test.rotate {
				t.Fatalf("rotate = %v (%q), want %v", rotate, reason, test.rotate)
			}
			if e.burned != test.burned {
				t.Fatalf("burned = %v, want %v", e.burned, test.burned)
			}
		})
	}
}

func TestExitAge(t *testing.T) {
	now := time.Now()
	e := &exit{startedAt: now.Add(-3 * time.Minute)}
	if got := e.age(now); got != 3*time.Minute {
		t.Fatalf("age = %v, want 3m", got)
	}
	if got := (&exit{}).age(now); got != 0 {
		t.Fatalf("age of unstamped exit = %v, want 0", got)
	}
}

func TestParseTraceRejectsWarpOff(t *testing.T) {
	_, err := parseTrace(strings.NewReader("ip=2a09:bac5:55ff:18be::1\nwarp=off\n"))
	if err == nil {
		t.Fatal("parseTrace accepted a trace with warp=off")
	}
	if !strings.Contains(err.Error(), "warp=off") {
		t.Fatalf("error = %v, want it to name warp=off", err)
	}
}
