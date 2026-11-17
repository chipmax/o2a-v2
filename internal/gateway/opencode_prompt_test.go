package gateway

import (
	"encoding/json"
	"os"
	"strings"
	"testing"

	"opencode2api/internal/config"
	wire "opencode2api/internal/protocol"
)

func TestOpencodeEnvironmentKeepsGateShapeWithoutHostPaths(t *testing.T) {
	env := opencodeEnvironment("opencode/deepseek-v4.1-flash-free", nil)
	for _, required := range []string{
		"<env>",
		"</env>",
		"Working directory:",
		"deepseek-v4.1-flash-free",
		"Platform:",
		"Today's date:",
	} {
		if !strings.Contains(env, required) {
			t.Fatalf("environment block is missing %q:\n%s", required, env)
		}
	}
	for _, forbidden := range []string{
		"C:\\",
		"System32",
		"/home/",
		"/root",
		"/Users/",
	} {
		if strings.Contains(env, forbidden) {
			t.Fatalf("environment block leaks %q:\n%s", forbidden, env)
		}
	}
	if wd, err := os.Getwd(); err == nil && len(wd) > 1 && strings.Contains(env, wd) {
		t.Fatalf("environment block leaks the working directory %q:\n%s", wd, env)
	}
}

func TestOpencodeSystemPromptKeepsMarkerAfterEnvironmentChange(t *testing.T) {
	prompt := opencodeSystemPrompt("opencode/test-free", false, nil)
	if !strings.Contains(prompt, opencodePromptMarker) {
		t.Fatal("system prompt lost the marker that prevents duplicate injection")
	}
	if !strings.Contains(prompt, "<env>") {
		t.Fatal("system prompt lost the environment block")
	}
}

// TestPromptEnvironmentOverride keeps the gate's one complete <env> pair while
// replacing host values with the configured client environment.
// TestInjectedPromptKeepsExactlyOneEnvPair covers the upstream quirk measured
// by pi-freeflow: only the first <env> pair is read. A client-supplied stale
// block must be removed rather than left above the gateway's complete one.
func TestInjectedPromptKeepsExactlyOneEnvPair(t *testing.T) {
	cases := map[string]map[string]any{
		"chat": {
			"model": "m",
			"messages": []any{
				map[string]any{"role": "system", "content": "stale\n<env>\n  Working directory: /old\n</env>"},
				map[string]any{"role": "user", "content": "hi"},
			},
		},
		"responses": {
			"model":        "m",
			"instructions": "stale <env>partial</env> tail",
		},
		"anthropic": {
			"model":  "m",
			"system": "stale <env>\n  Working directory: /old\n</env> tail",
		},
	}
	for name, payload := range cases {
		t.Run(name, func(t *testing.T) {
			protocolValue := map[string]wire.Protocol{"chat": wire.Chat, "responses": wire.Responses, "anthropic": wire.Anthropic}[name]
			if !injectOfficialPrompt(payload, protocolValue, false, nil) {
				t.Fatal("prompt injection did not run")
			}
			text, _ := json.Marshal(payload)
			// encoding/json escapes angle brackets; count the decoded text.
			decoded := strings.NewReplacer(`\u003c`, "<", `\u003e`, ">").Replace(string(text))
			if got := strings.Count(decoded, "<env>"); got != 1 {
				t.Fatalf("got %d env blocks, want 1:\n%s", got, decoded)
			}
			if got := strings.Count(decoded, "</env>"); got != 1 {
				t.Fatalf("got %d closing env tags, want 1:\n%s", got, decoded)
			}
			if !strings.Contains(decoded, "Working directory:") {
				t.Fatal("complete block lost its load-bearing line")
			}
		})
	}
}

func TestPromptEnvironmentOverride(t *testing.T) {
	no := false
	environment := &config.PromptEnvironmentConfig{
		WorkingDirectory: "C:/work/repo",
		WorkspaceRoot:    "C:/work",
		Platform:         "win32",
		IsGitRepo:        &no,
	}
	env := opencodeEnvironment("opencode/test-free", environment)
	for _, required := range []string{
		"  Working directory: C:/work/repo",
		"  Workspace root folder: C:/work",
		"  Is directory a git repo: no",
		"  Platform: win32",
	} {
		if !strings.Contains(env, required) {
			t.Fatalf("environment block is missing %q:\n%s", required, env)
		}
	}
	if strings.Count(env, "<env>") != 1 || strings.Count(env, "</env>") != 1 {
		t.Fatalf("environment block must stay a single pair:\n%s", env)
	}
	if shell := v2ShellDescription("on Linux using bash", "win32"); !strings.Contains(shell, "on Windows using powershell") {
		t.Fatalf("shell description did not follow the configured platform: %s", shell)
	}
}
