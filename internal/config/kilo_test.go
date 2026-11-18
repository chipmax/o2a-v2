package config

import "testing"

func TestKiloNormalizeAndAllowlist(t *testing.T) {
	k := KiloConfig{Models: []string{" kilo-auto/free ", "bogus-model", "kilo-auto/free"}}
	if err := k.Normalize(); err == nil {
		t.Fatal("non-free model should be rejected")
	}
	k.Models = []string{" kilo-auto/free ", "kilo-auto/free", "baidu/ernie-5.5-flash-preview:free"}
	if err := k.Normalize(); err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if !k.Enabled() || !k.HasModel("kilo-auto/free") || k.HasModel("deepseek-v4.1-flash-free") {
		t.Fatalf("allowlist wrong: %+v", k.Models)
	}
	if k.BaseURL != defaultKiloBaseURL || k.CooldownSeconds != 90 || k.QuotaNamespace != "kilo" {
		t.Fatalf("defaults wrong: %+v", k)
	}
}

func TestKiloDisabledByEmptyAllowlist(t *testing.T) {
	k := KiloConfig{}
	if err := k.Normalize(); err != nil {
		t.Fatalf("Normalize: %v", err)
	}
	if k.Enabled() {
		t.Fatal("empty allowlist must disable the lane")
	}
}
