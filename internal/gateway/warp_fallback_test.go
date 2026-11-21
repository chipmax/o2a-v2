package gateway

import (
	"context"
	"errors"
	"net/http"
	"testing"
	"time"

	"opencode2api/internal/config"
)

func TestWarpFallbackEligible(t *testing.T) {
	g := &Gateway{}
	canceled, cancel := context.WithCancel(context.Background())
	cancel()
	cases := []struct {
		name string
		ctx  context.Context
		resp *http.Response
		err  error
		want bool
	}{
		{name: "success", ctx: context.Background(), resp: &http.Response{StatusCode: http.StatusOK}},
		{name: "rate limited", ctx: context.Background(), resp: &http.Response{StatusCode: http.StatusTooManyRequests}, want: true},
		{name: "upstream error", ctx: context.Background(), resp: &http.Response{StatusCode: http.StatusBadGateway}, want: true},
		{name: "client error", ctx: context.Background(), resp: &http.Response{StatusCode: http.StatusBadRequest}},
		{name: "quota", ctx: context.Background(), err: errFreeTierQuotaSpent, want: true},
		{name: "ip blocked", ctx: context.Background(), err: errFreeTierIPBlocked, want: true},
		{name: "unavailable", ctx: context.Background(), err: errWARPUnavailable, want: true},
		{name: "deterministic", ctx: context.Background(), err: errDeterministicClientRejection},
		{name: "request canceled", ctx: canceled, resp: &http.Response{StatusCode: http.StatusTooManyRequests}},
		{name: "no response", ctx: context.Background(), want: true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := g.warpFallbackEligible(tc.ctx, tc.resp, tc.err); got != tc.want {
				t.Fatalf("eligible = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestWarpFallbackDelay(t *testing.T) {
	g := &Gateway{cfg: config.Config{Warp: config.WarpConfig{
		PauseAfterQuotaSeconds:  900,
		FallbackCooldownSeconds: 60,
	}}}
	if got := g.warpFallbackDelay(errFreeTierQuotaSpent, 0); got != 15*time.Minute {
		t.Fatalf("quota delay = %s, want 15m", got)
	}
	if got := g.warpFallbackDelay(errors.New("transport"), 0); got != time.Minute {
		t.Fatalf("transport delay = %s, want 1m", got)
	}
	if got := g.warpFallbackDelay(errors.New("transport"), 2*time.Minute); got != 2*time.Minute {
		t.Fatalf("retry-after delay = %s, want 2m", got)
	}
	if got := g.warpFallbackDelay(errors.New("transport"), 2*time.Hour); got != time.Hour {
		t.Fatalf("retry-after cap = %s, want 1h", got)
	}
}
