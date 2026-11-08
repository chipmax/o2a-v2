package gateway

import (
	"io"
	"net/http"
	"strings"
	"testing"

	"opencode2api/internal/config"
)

func TestQuotaUnlimitedIsNoop(t *testing.T) {
	q := &keyQuota{state: map[string]*keyQuotaState{}}
	release, refusal := q.acquire("id", config.ServerKeyMetadata{})
	if refusal != nil {
		t.Fatalf("unlimited key refused: %+v", refusal)
	}
	release()
	release()
}

func TestQuotaConcurrencyLimit(t *testing.T) {
	q := &keyQuota{state: map[string]*keyQuotaState{}}
	meta := config.ServerKeyMetadata{MaxConcurrency: 1}
	release, refusal := q.acquire("id", meta)
	if refusal != nil {
		t.Fatalf("first request refused: %+v", refusal)
	}
	if _, refusal := q.acquire("id", meta); refusal == nil {
		t.Fatal("second concurrent request was admitted")
	} else if refusal.code != "key_concurrency_limit" {
		t.Fatalf("unexpected code %q", refusal.code)
	}
	release()
	if _, refusal := q.acquire("id", meta); refusal != nil {
		t.Fatalf("slot was not released: %+v", refusal)
	}
}

func TestQuotaDailyLimit(t *testing.T) {
	q := &keyQuota{state: map[string]*keyQuotaState{}}
	meta := config.ServerKeyMetadata{DailyRequests: 2}
	for i := 0; i < 2; i++ {
		release, refusal := q.acquire("id", meta)
		if refusal != nil {
			t.Fatalf("request %d refused early: %+v", i+1, refusal)
		}
		release()
	}
	if _, refusal := q.acquire("id", meta); refusal == nil {
		t.Fatal("third request was admitted past the daily limit")
	} else if refusal.code != "key_daily_limit" || refusal.retryAfter <= 0 {
		t.Fatalf("unexpected refusal %+v", refusal)
	}

	// A stored counter from an earlier UTC day starts fresh.
	q.mu.Lock()
	q.state["id"].day = "2000-01-01"
	q.mu.Unlock()
	release, refusal := q.acquire("id", meta)
	if refusal != nil {
		t.Fatalf("new day still limited: %+v", refusal)
	}
	release()
}

func response(status int, contentType, body string) *http.Response {
	header := http.Header{}
	if contentType != "" {
		header.Set("Content-Type", contentType)
	}
	return &http.Response{StatusCode: status, Header: header, Body: io.NopCloser(strings.NewReader(body))}
}

func TestIsQuotaErrorDocument(t *testing.T) {
	cases := []struct {
		name     string
		resp     *http.Response
		quota    bool
		bodyKept bool
	}{
		{
			name:     "200 with free usage error",
			resp:     response(200, "application/json", `{"type":"error","error":{"type":"FreeUsageLimitError","message":"Free usage limit reached"}}`),
			quota:    true,
			bodyKept: true,
		},
		{
			name:     "200 with free tier error",
			resp:     response(200, "application/json; charset=utf-8", `{"error":{"type":"FreeTierError","message":"free tier"}}`),
			quota:    true,
			bodyKept: true,
		},
		{
			name:     "200 completion",
			resp:     response(200, "application/json", `{"choices":[{"message":{"content":"hi"}}]}`),
			bodyKept: true,
		},
		{
			name:     "429 status is not a document",
			resp:     response(429, "application/json", `{"type":"error","error":{"type":"FreeUsageLimitError"}}`),
			bodyKept: true,
		},
		{
			name:     "event stream is never peeked",
			resp:     response(200, "text/event-stream", `{"type":"error","error":{"type":"FreeUsageLimitError"}}`),
			bodyKept: true,
		},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			body, _ := io.ReadAll(test.resp.Body)
			test.resp.Body = io.NopCloser(strings.NewReader(string(body)))
			if got := isQuotaErrorDocument(test.resp); got != test.quota {
				t.Fatalf("isQuotaErrorDocument = %v, want %v", got, test.quota)
			}
			after, _ := io.ReadAll(test.resp.Body)
			if test.bodyKept && string(after) != string(body) {
				t.Fatalf("body was consumed: got %q, want %q", after, body)
			}
		})
	}
}
