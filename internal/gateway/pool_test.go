package gateway

import (
	"net/http"
	"testing"
	"time"
)

func TestAnonymousMarkFailureHonoursRetryAfter(t *testing.T) {
	proxy := &proxyTransport{name: "direct"}
	node := &anonymousNode{proxy: proxy}
	nodes := &anonymousNodes{cooldown: 15 * time.Second, nodes: []*anonymousNode{node}}

	resp := &http.Response{StatusCode: http.StatusTooManyRequests, Header: http.Header{"Retry-After": {"3600"}}}
	nodes.MarkFailure(node, resp, nil)

	remaining := time.Until(time.Unix(0, node.cooldownUntil.Load()))
	if remaining < 55*time.Minute {
		t.Fatalf("cooldown = %v, want about one hour", remaining)
	}
	if !node.quotaCooldown.Load() {
		t.Fatal("a Retry-After cooldown must be flagged as a quota cooldown")
	}
	if cleared := nodes.RestoreProxy(proxy); cleared != 0 {
		t.Fatalf("RestoreProxy cleared %d nodes whose quota is still spent", cleared)
	}
	if node.cooldownUntil.Load() == 0 {
		t.Fatal("RestoreProxy reopened an exhausted exit")
	}

	nodes.MarkSuccess(node)
	if node.cooldownUntil.Load() != 0 || node.quotaCooldown.Load() {
		t.Fatal("MarkSuccess did not clear the cooldown")
	}
}

func TestAnonymousRestoreProxyClearsOutageCooldowns(t *testing.T) {
	proxy := &proxyTransport{name: "direct"}
	node := &anonymousNode{proxy: proxy}
	nodes := &anonymousNodes{cooldown: 15 * time.Second, nodes: []*anonymousNode{node}}

	nodes.MarkFailure(node, nil, errTestUpstream)
	if node.quotaCooldown.Load() {
		t.Fatal("a transport failure must not be flagged as a quota cooldown")
	}
	if cleared := nodes.RestoreProxy(proxy); cleared != 1 {
		t.Fatalf("RestoreProxy cleared %d nodes, want 1", cleared)
	}
	if node.cooldownUntil.Load() != 0 {
		t.Fatal("RestoreProxy left the outage cooldown in place")
	}
}

var errTestUpstream = &testError{"upstream failed"}

type testError struct{ message string }

func (e *testError) Error() string { return e.message }
