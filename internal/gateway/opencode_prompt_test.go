package gateway

import (
	"strings"
	"testing"
)

func TestOpencodeEnvironmentOmitsHostPaths(t *testing.T) {
	env := opencodeEnvironment("opencode/deepseek-v4.1-flash-free")
	for _, forbidden := range []string{
		"Working directory",
		"Workspace root folder",
		"Is directory a git repo",
		"C:\\",
		"/workspace",
	} {
		if strings.Contains(env, forbidden) {
			t.Fatalf("environment block leaks %q:\n%s", forbidden, env)
		}
	}
	for _, required := range []string{
		"<env>",
		"</env>",
		"deepseek-v4.1-flash-free",
		"Platform:",
		"Today's date:",
	} {
		if !strings.Contains(env, required) {
			t.Fatalf("environment block is missing %q:\n%s", required, env)
		}
	}
}

func TestOpencodeSystemPromptKeepsMarkerAfterEnvironmentChange(t *testing.T) {
	prompt := opencodeSystemPrompt("opencode/test-free", false)
	if !strings.Contains(prompt, opencodePromptMarker) {
		t.Fatal("system prompt lost the marker that prevents duplicate injection")
	}
	if !strings.Contains(prompt, "<env>") {
		t.Fatal("system prompt lost the environment block")
	}
}
