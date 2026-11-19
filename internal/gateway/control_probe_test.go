package gateway

import (
	"net/http"
	"testing"
	"time"
)

func TestClassify429(t *testing.T) {
	long := 18 * time.Hour // counts down to 00:00 UTC
	cases := []struct {
		name    string
		retry   time.Duration
		control int
		want    refusalKind
	}{
		{"short retry is a burst", 30 * time.Second, 0, refusalBurst},
		{"long retry, no probe keeps legacy rotate", long, 0, refusalSpentBlock},
		{"long retry, control also 429 is spent", long, http.StatusTooManyRequests, refusalSpentBlock},
		{"long retry, control 200 is a shape refusal", long, http.StatusOK, refusalShape},
		{"short retry with control 200 still shape", 30 * time.Second, http.StatusOK, refusalShape},
		{"no retry and no control stays unknown", 0, 0, refusalUnknown},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := classify429(tc.retry, tc.control); got != tc.want {
				t.Fatalf("classify429(%s, %d) = %v, want %v", tc.retry, tc.control, got, tc.want)
			}
		})
	}
}
