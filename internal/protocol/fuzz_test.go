package protocol

import (
	"encoding/json"
	"strings"
	"testing"
)

func FuzzConvertRequest(f *testing.F) {
	seeds := [][]byte{
		[]byte(`{}`),
		[]byte(`{"model":"m","messages":[{"role":"user","content":"hi"}]}`),
		[]byte(`{"model":"m","input":"hi","stream":true}`),
		[]byte(`{"model":"m","max_tokens":16,"messages":[{"role":"user","content":[{"type":"text","text":"hi"}]}]}`),
		[]byte(`{"model":"m","messages":[{"role":"user","content":[{"type":"image_url","image_url":{"url":"data:image/png;base64,AA=="}}]}],"tools":[{"type":"function","function":{"name":"f","parameters":{"type":"object"}}}]}`),
	}
	for _, seed := range seeds {
		f.Add(seed)
	}
	protocols := []Protocol{Chat, Responses, Anthropic}
	f.Fuzz(func(t *testing.T, data []byte) {
		var payload map[string]any
		if json.Unmarshal(data, &payload) != nil {
			return
		}
		for _, from := range protocols {
			for _, to := range protocols {
				_, _ = ConvertRequest(from, to, payload)
			}
		}
	})
}

func FuzzCollapseStream(f *testing.F) {
	seeds := []string{
		"",
		"data: [DONE]\n\n",
		"event: message_start\ndata: {\"type\":\"message_start\"}\n\n",
		"data: {\"id\":\"x\",\"choices\":[{\"delta\":{\"content\":\"hi\"}}]}\n\ndata: [DONE]\n\n",
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"hi\"}\n\n",
	}
	for _, seed := range seeds {
		f.Add(seed)
	}
	protocols := []Protocol{Chat, Responses, Anthropic}
	f.Fuzz(func(t *testing.T, data string) {
		for _, from := range protocols {
			_, _ = CollapseStream(strings.NewReader(data), from, "m")
		}
	})
}

func FuzzResponseUsage(f *testing.F) {
	seeds := [][]byte{
		[]byte(`{}`),
		[]byte(`{"usage":{"prompt_tokens":1,"completion_tokens":2,"total_tokens":3}}`),
		[]byte(`{"usage":{"input_tokens":1,"output_tokens":2}}`),
		[]byte(`{"usage":{"input_tokens":1,"output_tokens":2},"content":[]}`),
	}
	for _, seed := range seeds {
		f.Add(seed)
	}
	protocols := []Protocol{Chat, Responses, Anthropic}
	f.Fuzz(func(t *testing.T, data []byte) {
		for _, p := range protocols {
			_, _ = ResponseUsage(p, data)
		}
	})
}
