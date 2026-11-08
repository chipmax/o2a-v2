package config

import (
	"testing"
	"time"
)

func TestModelAllowed(t *testing.T) {
	open := ServerKeyMetadata{}
	if !open.ModelAllowed("deepseek-v4.1-flash-free") {
		t.Fatal("empty allow list must admit every model")
	}
	scoped := ServerKeyMetadata{AllowModels: []string{"iceman-free", "sota"}}
	if !scoped.ModelAllowed("iceman-free") || !scoped.ModelAllowed("sota") {
		t.Fatal("listed models must be admitted")
	}
	if scoped.ModelAllowed("glm-5.3-flash-free") {
		t.Fatal("unlisted model was admitted")
	}
}

func TestPolicyLookup(t *testing.T) {
	key := "oc_sk_policy_test_123456"
	cfg := Config{
		ServerKeys: []string{key},
		ServerKeyMetadata: map[string]ServerKeyMetadata{
			Fingerprint(key): {AllowModels: []string{"iceman-free"}, MaxConcurrency: 2, DailyRequests: 100},
		},
	}
	policy := cfg.Policy(key)
	if policy.MaxConcurrency != 2 || policy.DailyRequests != 100 {
		t.Fatalf("Policy(key) = %+v", policy)
	}
	if got := cfg.PolicyByID(Fingerprint(key)); got.MaxConcurrency != 2 {
		t.Fatalf("PolicyByID = %+v", got)
	}
	if got := cfg.PolicyByID("deadbeef"); got.MaxConcurrency != 0 {
		t.Fatalf("unknown id returned a policy: %+v", got)
	}
}

func TestAttemptTimeoutFor(t *testing.T) {
	perf := PerformanceConfig{
		AttemptTimeoutSeconds: 30,
		ModelTimeouts:         map[string]int{"deepseek-v4.1-flash-free": 600},
	}
	requestTimeout := 20 * time.Minute
	if got := perf.AttemptTimeoutFor("deepseek-v4.1-flash-free", requestTimeout); got != 600*time.Second {
		t.Fatalf("model override = %v, want 10m", got)
	}
	if got := perf.AttemptTimeoutFor("glm-5.3-flash-free", requestTimeout); got != 30*time.Second {
		t.Fatalf("default = %v, want 30s", got)
	}
	perf.AttemptTimeoutSeconds = 0
	if got := perf.AttemptTimeoutFor("glm-5.3-flash-free", requestTimeout); got != requestTimeout {
		t.Fatalf("zero override = %v, want the request timeout", got)
	}
}

func TestCloneCopiesAllowModels(t *testing.T) {
	id := "abc"
	original := Config{ServerKeyMetadata: map[string]ServerKeyMetadata{
		id: {AllowModels: []string{"iceman-free"}},
	}}
	clone := Clone(original)
	clone.ServerKeyMetadata[id].AllowModels[0] = "mutated"
	if original.ServerKeyMetadata[id].AllowModels[0] != "iceman-free" {
		t.Fatal("Clone shares the AllowModels backing array")
	}
}
