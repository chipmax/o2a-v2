package warp

import (
	"context"
	"testing"
)

func TestNewPoolStartsEmptyWhenEveryEgressIsDrained(t *testing.T) {
	for _, firstErr := range []error{errDuplicateEgress, errLaneBurnt} {
		t.Run(firstErr.Error(), func(t *testing.T) {
			create := func(context.Context) (*exit, error) { return nil, firstErr }
			pool, err := newPool(context.Background(), nil, nil, nil, 2, 1, 0, 0, newLaneGate(""), create)
			if err != nil {
				t.Fatalf("newPool returned %v; want an empty pool instead", err)
			}
			if pool == nil {
				t.Fatal("newPool returned a nil pool")
			}
			if got := len(pool.active); got != 0 {
				t.Fatalf("active exits = %d, want 0", got)
			}
			pool.Close()
		})
	}
}
