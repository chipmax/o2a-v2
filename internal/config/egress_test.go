package config

import (
	"strings"
	"testing"
)

func egressTestConfig() Config {
	return Config{
		Listen:     "127.0.0.1:8080",
		ServerKeys: []string{"test-key"},
		Anonymous:  true,
		Prefer:     TierGo,
		Upstream: UpstreamConfig{
			Zen: "https://opencode.ai/zen", Go: "https://opencode.ai/zen/go",
			Anonymous: "https://opencode.ai/inference",
		},
		Retry:  RetryConfig{MaxAttempts: 3, TimeoutSeconds: 300},
		Models: ModelsConfig{RefreshSeconds: 300},
		Performance: PerformanceConfig{
			MaxIdleConns: 16, MaxIdleConnsPerHost: 8, IdleConnTimeoutSeconds: 30,
			ConnectTimeoutSeconds: 5, FailureCooldownSeconds: 15,
		},
		Logging: LoggingConfig{Level: "info", RingSize: 2000},
		WebUI:   WebUIConfig{Enabled: false},
	}
}

func TestRequireEgressGate(t *testing.T) {
	cfg := egressTestConfig()
	cfg.Egress.RequireEgress = true
	if _, err := Normalize("/tmp/egress-test.json", cfg); err == nil || !strings.Contains(err.Error(), "no egress configured") {
		t.Fatalf("direct-only config should be refused, got %v", err)
	}

	cfg.Egress.AllowDirect = true
	if _, err := Normalize("/tmp/egress-test.json", cfg); err != nil {
		t.Fatalf("allow_direct should pass: %v", err)
	}

	cfg.Egress.AllowDirect = false
	cfg.Proxies = []string{"socks5h://127.0.0.1:1080"}
	if _, err := Normalize("/tmp/egress-test.json", cfg); err != nil {
		t.Fatalf("configured proxy should pass: %v", err)
	}

	// Default config with no egress flags must keep its old behaviour.
	if _, err := Normalize("/tmp/egress-test.json", egressTestConfig()); err != nil {
		t.Fatalf("default config regressed: %v", err)
	}
}
