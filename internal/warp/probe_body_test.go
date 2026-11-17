package warp

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestLaneProbeBodyCarriesTheGateShape(t *testing.T) {
	body := laneProbeBody(LaneProbe{Model: "deepseek-v4.1-flash-free"})
	var payload struct {
		Model    string `json:"model"`
		Stream   bool   `json:"stream"`
		Messages []struct {
			Role    string `json:"role"`
			Content string `json:"content"`
		} `json:"messages"`
		Tools []json.RawMessage `json:"tools"`
	}
	if err := json.Unmarshal([]byte(body), &payload); err != nil {
		t.Fatalf("probe body is not valid JSON: %v\n%s", err, body)
	}
	if payload.Model == "" || !payload.Stream {
		t.Fatalf("probe body = %+v, want a streaming model request", payload)
	}
	if len(payload.Messages) != 2 || payload.Messages[0].Role != "system" {
		t.Fatalf("probe messages = %+v, want a leading system message", payload.Messages)
	}
	if !strings.Contains(payload.Messages[0].Content, "<env>") || !strings.Contains(payload.Messages[0].Content, "Working directory:") {
		t.Fatalf("probe system prompt lacks the env block the gate requires: %q", payload.Messages[0].Content)
	}
	if len(payload.Tools) == 0 {
		t.Fatal("probe body has no tools; the gate refuses an empty tools array")
	}
}

func TestLaneProbeBodyUsesTheCallerPrompt(t *testing.T) {
	body := laneProbeBody(LaneProbe{Model: "m", SystemPrompt: "You are opencode, an interactive CLI tool\n<env>\nWorking directory: /a\n</env>"})
	if !strings.Contains(body, "interactive CLI tool") {
		t.Fatalf("probe body ignored the caller prompt:\n%s", body)
	}
}
