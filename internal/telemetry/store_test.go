package telemetry

import (
	"database/sql"
	"path/filepath"
	"testing"
	"time"
)

func TestUsageStorePersistsQueuedRequest(t *testing.T) {
	path := filepath.Join(t.TempDir(), "usage.sqlite")
	store, err := OpenUsageStore(path, nil)
	if err != nil {
		t.Fatalf("OpenUsageStore: %v", err)
	}
	when := time.Date(2026, 11, 9, 8, 30, 0, 0, time.UTC)
	store.Append(UpstreamRequest{
		Time: when, RequestID: "req-1", Session: "ses_1", ServerKeyID: "sk",
		Model: "deepseek-v4.1-flash", Tier: "free", KeyID: "anonymous", Channel: "anonymous",
		Anonymous: true, Proxy: "direct", Attempts: 2, Status: 200, DurationMS: 1234, Success: true,
		Outcome: "ok", InputTokens: 10, OutputTokens: 20, CachedTokens: 3, ReasoningTokens: 4, TotalTokens: 37,
	})
	if err := store.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}

	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	defer db.Close()
	var (
		requestID, model, outcome string
		anonymous, attempts       int
		input, output, total      uint64
	)
	err = db.QueryRow(`SELECT request_id, model, outcome, anonymous, attempts,
		input_tokens, output_tokens, total_tokens FROM usage_requests`).
		Scan(&requestID, &model, &outcome, &anonymous, &attempts, &input, &output, &total)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if requestID != "req-1" || model != "deepseek-v4.1-flash" || outcome != "ok" || anonymous != 1 || attempts != 2 {
		t.Fatalf("row = %q %q %q anonymous=%d attempts=%d", requestID, model, outcome, anonymous, attempts)
	}
	if input != 10 || output != 20 || total != 37 {
		t.Fatalf("tokens = %d/%d/%d, want 10/20/37", input, output, total)
	}
}

func TestOpenUsageStoreEmptyPathDisables(t *testing.T) {
	store, err := OpenUsageStore("", nil)
	if err != nil || store != nil {
		t.Fatalf("OpenUsageStore(\"\") = %v, %v; want nil, nil", store, err)
	}
}
