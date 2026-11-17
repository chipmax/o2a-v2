package gateway

import (
	"io"
	"net/http"
	"strings"
	"testing"
)

// TestBufferRetryResponseKeepsTheErrorDocument pins the retry contract: the
// socket is released but the status, Retry-After and error body all survive so
// a later transport failure cannot bury a 429 with a generic 502.
func TestBufferRetryResponseKeepsTheErrorDocument(t *testing.T) {
	body := `{"type":"error","error":{"type":"FreeUsageLimitError"}}`
	resp := &http.Response{
		StatusCode: http.StatusTooManyRequests,
		Header:     http.Header{"Retry-After": []string{"18912"}},
		Body:       io.NopCloser(strings.NewReader(body)),
	}
	bufferRetryResponse(resp)
	if resp.StatusCode != http.StatusTooManyRequests || resp.Header.Get("Retry-After") != "18912" {
		t.Fatal("buffering changed the status or Retry-After")
	}
	got, err := io.ReadAll(resp.Body)
	if err != nil || string(got) != body {
		t.Fatalf("buffered body = %q, err = %v", got, err)
	}
	// A second read must still work: callers may peek and then forward.
	if _, err := io.ReadAll(resp.Body); err != nil {
		t.Fatalf("buffered body is not replayable: %v", err)
	}
}
