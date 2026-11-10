package config

import (
	"fmt"
	"net/url"
	"strings"
)

// PeerConfig names another o2a-compatible gateway this process falls back to
// once every local lane has failed. It is what makes one listen address (for
// example :6363) serve the whole fleet: the caller needs no router, and the
// peer list is ordered by Tier, round-robined inside a tier, and cooled down on
// 401/403/429/5xx the same way the standalone router did.
type PeerConfig struct {
	// Name identifies the peer in logs, telemetry and the X-Peer header.
	Name string `json:"name"`
	// BaseURL is the peer's API root without /v1, e.g. http://10.0.0.184:8080.
	BaseURL string `json:"base_url"`
	// Key is the peer's own server key. It replaces the caller's credential
	// on the forwarded request, because peers authenticate their own keys.
	Key string `json:"key"`
	// Tier orders peers: lower is tried first. Peers with the same tier are
	// tried round-robin, so a pair of equal machines shares the load.
	Tier int `json:"tier,omitempty"`
	// TimeoutSeconds bounds the wait for a peer's response headers, not an
	// established event stream. Zero uses 180. A long stream therefore keeps
	// flowing even when it outlives this value.
	TimeoutSeconds int `json:"timeout_seconds,omitempty"`
	// CooldownSeconds is how long a peer that answered 401/403/5xx stays out
	// of rotation. A 429 honours its Retry-After instead, clamped to 15 minutes.
	CooldownSeconds int `json:"cooldown_seconds,omitempty"`
	// Models, when set, restricts the peer to these model IDs. Empty means the
	// peer serves everything it knows.
	Models []string `json:"models,omitempty"`
}

// Normalize fills the defaults.
func (p *PeerConfig) Normalize() {
	p.Name = strings.TrimSpace(p.Name)
	p.BaseURL = strings.TrimRight(strings.TrimSpace(p.BaseURL), "/")
	p.Key = strings.TrimSpace(p.Key)
	if p.Tier < 1 {
		p.Tier = 1
	}
	if p.TimeoutSeconds <= 0 {
		p.TimeoutSeconds = 180
	}
	if p.CooldownSeconds <= 0 {
		p.CooldownSeconds = 60
	}
}

// Validate rejects a peer the forwarder cannot use.
func (p PeerConfig) Validate(index int) error {
	field := fmt.Sprintf("peers[%d]", index)
	if p.BaseURL == "" {
		return fmt.Errorf("%s.base_url is required", field)
	}
	parsed, err := url.Parse(p.BaseURL)
	if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" {
		return fmt.Errorf("%s.base_url must be an http(s) URL, got %q", field, p.BaseURL)
	}
	if p.Key == "" {
		return fmt.Errorf("%s.key is required", field)
	}
	if p.Tier < 1 {
		return fmt.Errorf("%s.tier must be at least 1", field)
	}
	if len(p.Models) > 512 {
		return fmt.Errorf("%s.models lists %d entries, at most 512 are supported", field, len(p.Models))
	}
	return nil
}

// PeerAllowed reports whether the peer serves model.
func (p PeerConfig) PeerAllowed(model string) bool {
	if len(p.Models) == 0 {
		return true
	}
	for _, candidate := range p.Models {
		if candidate == model {
			return true
		}
	}
	return false
}
