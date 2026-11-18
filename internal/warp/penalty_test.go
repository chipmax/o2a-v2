package warp

import (
	"context"
	"errors"
	"net/netip"
	"testing"
	"time"
)

func penaltyPool(first *exit, create func(context.Context) (*exit, error)) *Pool {
	return &Pool{
		ctx: context.Background(), size: 2, preload: 1,
		active: []*exit{first}, reserved: map[string]bool{}, cooling: map[string]time.Time{},
		changed: make(chan struct{}), newExit: create,
	}
}

func waitActive(t *testing.T, pool *Pool, want int) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		pool.mu.Lock()
		got := len(pool.active)
		pool.mu.Unlock()
		if got == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("active count never reached %d", want)
}

func TestQuotaPenaltyIsNotRestoredWhenReplacementFails(t *testing.T) {
	bad := &exit{id: "quota", egress: netip.MustParseAddr("2a09:bac5:d45a:263c::1"), stack: StackIPv6}
	pool := penaltyPool(bad, func(context.Context) (*exit, error) { return nil, errors.New("registrar down") })
	pool.penalizeAfter(bad, "HTTP 429", 24*time.Hour, 0)
	waitActive(t, pool, 0)
}

func TestTransportPenaltyIsRestoredWhenReplacementFails(t *testing.T) {
	bad := &exit{id: "transport", egress: netip.MustParseAddr("2a09:bac5:d45b:2646::1"), stack: StackIPv6}
	pool := penaltyPool(bad, func(context.Context) (*exit, error) { return nil, errors.New("registrar down") })
	pool.penalizeAfter(bad, "dial failed", time.Minute, 0)
	waitActive(t, pool, 1)
	pool.mu.Lock()
	got := pool.active[0]
	pool.mu.Unlock()
	if got != bad || bad.quotaPenalty {
		t.Fatalf("transport exit not restored cleanly: %v quota=%v", got, bad.quotaPenalty)
	}
}
