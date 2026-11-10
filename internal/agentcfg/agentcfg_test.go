package agentcfg

import (
	"encoding/json"
	"strings"
	"testing"
)

func testModels() []Model {
	return []Model{
		{ID: "zeta", Context: 1000000, Output: 512000, Reasoning: true, Input: []string{"text", "image"}},
		{ID: "alpha", Context: 200000, Output: 32000},
	}
}

func TestRenderOMP(t *testing.T) {
	text, err := Render(KindOMP, "http://127.0.0.1:8090/v1", "oc_sk_test", testModels())
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(text, Marker(KindOMP)) {
		t.Fatalf("marker missing:\n%s", text)
	}
	for _, want := range []string{
		"baseUrl: http://127.0.0.1:8090/v1",
		"apiKey: oc_sk_test",
		"contextWindow: 1000000",
		"maxTokens: 512000",
		"input: [text, image]",
	} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in\n%s", want, text)
		}
	}
	if strings.Index(text, "alpha") > strings.Index(text, "zeta") {
		t.Error("models are not sorted")
	}
}

func TestRenderOpenCodeMerges(t *testing.T) {
	existing := []byte(`{"model":"keep/me","provider":{"keep":{"npm":"x"}}}`)
	out, err := RenderOpenCode(existing, "http://gw/v1", "key", testModels())
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(out, &document); err != nil {
		t.Fatal(err)
	}
	if document["model"] != "keep/me" {
		t.Error("existing top-level model was not preserved")
	}
	providers := document["provider"].(map[string]any)
	if _, ok := providers["keep"]; !ok {
		t.Error("existing provider was dropped")
	}
	o2a, ok := providers["o2a-v2"].(map[string]any)
	if !ok {
		t.Fatal("o2a-v2 provider missing")
	}
	if o2a["npm"] != "@ai-sdk/openai-compatible" {
		t.Errorf("unexpected npm %v", o2a["npm"])
	}
	models := o2a["models"].(map[string]any)
	zeta := models["zeta"].(map[string]any)
	if zeta["attachment"] != true {
		t.Error("text+image model should be an attachment model")
	}
}

func TestRenderDSH(t *testing.T) {
	text, err := Render(KindDSH, "http://gw/v1", "key", testModels())
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"- id: llm-pi-ai", "authorization: Bearer key", "contextWindow: 200000"} {
		if !strings.Contains(text, want) {
			t.Errorf("missing %q in\n%s", want, text)
		}
	}
}
