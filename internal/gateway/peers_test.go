package gateway

import (
	"net/http"
	"testing"
	"time"

	"opencode2api/internal/config"
)

func testPeer(name string, tier int) config.PeerConfig {
	return config.PeerConfig{Name: name, BaseURL: "http://127.0.0.1:1", Key: "k", Tier: tier, CooldownSeconds: 60}
}

func TestPeerPoolOrdersByTierAndRotatesInsideOne(t *testing.T) {
	pool := newPeerPool([]config.PeerConfig{
		testPeer("third", 3),
		testPeer("a", 1),
		testPeer("b", 1),
	})
	first := pool.ordered("m")
	if len(first) != 3 || first[0].Tier != 1 || first[1].Tier != 1 || first[2].Name != "third" {
		t.Fatalf("ordered = %v, want the two tier-1 peers first", first)
	}
	firstNames := first[0].Name + first[1].Name
	second := pool.ordered("m")
	if second[0].Name+second[1].Name == firstNames {
		t.Fatalf("tier-1 round robin repeated the same leader: %q then %q", firstNames, second[0].Name+second[1].Name)
	}
}

func TestPeerPoolCoolsAndFallsBackToColdPeers(t *testing.T) {
	pool := newPeerPool([]config.PeerConfig{testPeer("only", 1)})
	header := http.Header{}
	header.Set("Retry-After", "120")
	delay := pool.cool(pool.peers[0], http.StatusTooManyRequests, header)
	if delay != 2*time.Minute {
		t.Fatalf("cool delay = %s, want the 2m Retry-After", delay)
	}
	ordered := pool.ordered("m")
	if len(ordered) != 1 || ordered[0].Name != "only" {
		t.Fatalf("a cooling peer must stay as a last resort, got %v", ordered)
	}
	pool.clear(pool.peers[0])
	if snapshot := pool.peerSnapshot(); snapshot[0]["cooling_seconds"] != nil {
		t.Fatalf("clear left a cooldown: %v", snapshot[0])
	}
}

func TestPeerPoolHonoursModelAllowList(t *testing.T) {
	peer := testPeer("restricted", 1)
	peer.Models = []string{"deepseek-v4.1-flash-free"}
	pool := newPeerPool([]config.PeerConfig{peer})
	if got := pool.ordered("other-model"); len(got) != 0 {
		t.Fatalf("ordered returned %d peers for a model outside the allow list", len(got))
	}
	if got := pool.ordered("deepseek-v4.1-flash-free"); len(got) != 1 {
		t.Fatalf("ordered returned %d peers for an allowed model", len(got))
	}
}

func TestPeerFallbackCodesCoverPeerExhaustion(t *testing.T) {
	for _, status := range []int{401, 403, 429, 500, 502, 503, 504} {
		if !peerFallbackCodes[status] {
			t.Fatalf("status %d should fall through to the next peer", status)
		}
	}
	if peerFallbackCodes[400] || peerFallbackCodes[404] {
		t.Fatal("a request-level rejection must reach the client")
	}
}
