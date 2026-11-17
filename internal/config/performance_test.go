package config

import (
	"testing"
	"time"
)

func intPointer(value int) *int { return &value }

// TestPerformanceStreamDefaults pins the absent-vs-zero contract: an omitted
// field keeps the documented default, while an explicit 0 disables the
// feature. A plain int could not tell the two apart.
func TestPerformanceStreamDefaults(t *testing.T) {
	var performance PerformanceConfig
	if got := performance.Heartbeat(); got != 15*time.Second {
		t.Fatalf("Heartbeat = %v, want 15s", got)
	}
	if got := performance.StreamIdleTimeout(); got != 5*time.Minute {
		t.Fatalf("StreamIdleTimeout = %v, want 5m", got)
	}
	if got := performance.CredentialQuarantine(); got != 24*time.Hour {
		t.Fatalf("CredentialQuarantine = %v, want 24h", got)
	}

	disabled := PerformanceConfig{
		HeartbeatSeconds:            intPointer(0),
		IdleTimeoutSeconds:          intPointer(0),
		CredentialQuarantineSeconds: intPointer(0),
	}
	if disabled.Heartbeat() != 0 || disabled.StreamIdleTimeout() != 0 || disabled.CredentialQuarantine() != 0 {
		t.Fatalf("explicit zero did not disable: %v %v %v", disabled.Heartbeat(), disabled.StreamIdleTimeout(), disabled.CredentialQuarantine())
	}
}

// TestPerformanceStreamValidationRejectsOutOfRange keeps a bad knob a load
// error instead of a silently clamped runtime value.
func TestPerformanceStreamValidationRejectsOutOfRange(t *testing.T) {
	cases := map[string]PerformanceConfig{
		"heartbeat":          {HeartbeatSeconds: intPointer(-1)},
		"idle":               {IdleTimeoutSeconds: intPointer(86401)},
		"credential":         {CredentialQuarantineSeconds: intPointer(2592001)},
		"max_inflight":       {MaxInflight: -1},
		"queue_wait_seconds": {QueueWaitSeconds: 301},
		"memory_limit_bytes": {MemoryLimitBytes: -1},
	}
	for name, performance := range cases {
		cfg := minimalConfig()
		cfg.Performance = performance
		if _, err := Normalize("", cfg); err == nil {
			t.Fatalf("%s: validation accepted an out-of-range value", name)
		}
	}
}

// minimalConfig returns a config that passes every other check, so a failure
// in the table above can only come from the performance values.
func minimalConfig() Config {
	return Config{
		Listen:     "127.0.0.1:0",
		ServerKeys: []string{"test-key"},
		ZenKeys:    []string{"zen-key"},
		Prefer:     TierZen,
		Upstream:   UpstreamConfig{Zen: "https://opencode.ai/zen"},
		Retry:      RetryConfig{MaxAttempts: 1, TimeoutSeconds: 30},
		Models:     ModelsConfig{RefreshSeconds: 300},
		Performance: PerformanceConfig{
			MaxIdleConns: 64, MaxIdleConnsPerHost: 16, MaxConnsPerHost: 0,
			IdleConnTimeoutSeconds: 90, ConnectTimeoutSeconds: 5, FailureCooldownSeconds: 15,
		},
	}
}
