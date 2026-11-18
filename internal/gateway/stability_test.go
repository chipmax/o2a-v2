package gateway

import (
	"net/http"
	"testing"
	"time"
)

func TestSpentForFamilyAndBudget(t *testing.T) {
	now := time.Now().UTC()
	b := &quotaBoard{}
	b.entries = make(map[string]*EgressQuotaEntry)
	b.record("direct", "deepseek-v4.1-flash-free", http.StatusTooManyRequests, true, 9*time.Hour, now)
	b.record("warp:2a09:bac1:6500:8::/64", "glm-5.3-flash-free", http.StatusOK, false, 0, now)

	if !b.spentFor("direct", "de", now) {
		t.Fatal("spent direct was not detected")
	}
	if b.spentFor("direct", "gl", now) {
		t.Fatal("quota hit leaked to another model family")
	}
	if b.familyHasBudget("de", now) {
		t.Fatal("family de has no unspent bucket")
	}
	if !b.familyHasBudget("gl", now) {
		t.Fatal("family gl still has a bucket")
	}
	if !b.familyHasBudget("ne", now) {
		t.Fatal("unknown family must not fast-fail")
	}
}

func TestAnonymousCursorSkipsSpentExit(t *testing.T) {
	pool := &anonymousNodes{nodes: []*anonymousNode{
		{proxy: &proxyTransport{name: "direct"}},
		{proxy: &proxyTransport{name: "socks5h://127.0.0.1:13001"}},
	}}
	board := &quotaBoard{}
	board.entries = make(map[string]*EgressQuotaEntry)
	board.record("direct", "deepseek-v4.1-flash-free", http.StatusTooManyRequests, true, 9*time.Hour, time.Now().UTC())

	cursor := pool.CursorFor("session")
	cursor.family = "de"
	cursor.quota = board
	picked := cursor.Next()
	if picked == nil || picked.proxy.name == "direct" {
		t.Fatalf("cursor picked spent direct: %+v", picked)
	}
}

func TestMarkKeyQuotaSpent(t *testing.T) {
	pool := &upstreamNodes{nodes: []*upstreamNode{{key: "k"}}}
	pool.MarkKeyQuotaSpent(pool.nodes[0], 6*time.Hour)
	if pool.nodes[0].quotaUntil.Load() <= time.Now().UnixNano() {
		t.Fatal("key quota horizon was not set")
	}
	// A live credential quarantine outlasts the quota wall.
	pool.nodes[0].credUntil.Store(time.Now().Add(12 * time.Hour).UnixNano())
	pool.MarkKeyQuotaSpent(pool.nodes[0], time.Hour)
	if pool.nodes[0].quotaUntil.Load() < pool.nodes[0].credUntil.Load() {
		t.Fatal("quota wall shortened credential quarantine")
	}
}
