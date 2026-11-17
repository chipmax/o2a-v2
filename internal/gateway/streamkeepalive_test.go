package gateway

import (
	"net/http"
	"strings"
	"sync"
	"testing"
	"time"

	wire "opencode2api/internal/protocol"
)

// recordingWriter is the smallest http.ResponseWriter that can flush, so the
// keepalive wrapper can be exercised without a real server.
type recordingWriter struct {
	mu      sync.Mutex
	header  http.Header
	status  int
	data    strings.Builder
	flushes int
}

func newRecordingWriter() *recordingWriter {
	return &recordingWriter{header: make(http.Header), status: http.StatusOK}
}

func (w *recordingWriter) Header() http.Header { return w.header }

func (w *recordingWriter) Write(p []byte) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.data.Write(p)
}

func (w *recordingWriter) WriteHeader(status int) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.status = status
}

func (w *recordingWriter) Flush() {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.flushes++
}

func (w *recordingWriter) body() string {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.data.String()
}

// TestStreamKeepaliveFillsSilence pins the reason the wrapper exists: a stream
// that has committed its headers but produces nothing must keep proving it is
// alive, and the frame must be one the client's parser ignores.
func TestStreamKeepaliveFillsSilence(t *testing.T) {
	dst := newRecordingWriter()
	keeper := newStreamKeepalive(dst, wire.Chat, 20*time.Millisecond, 90*time.Second)
	defer keeper.Stop()

	time.Sleep(70 * time.Millisecond)
	if got := dst.body(); got != ": keepalive\n\n" && !strings.Contains(got, ": keepalive") {
		t.Fatalf("body = %q, want an SSE comment keepalive frame", got)
	}
	if dst.flushes == 0 {
		t.Fatal("keepalive frame was not flushed to the client")
	}

	before := strings.Count(dst.body(), ": keepalive")
	keeper.Stop()
	time.Sleep(60 * time.Millisecond)
	if after := strings.Count(dst.body(), ": keepalive"); after != before {
		t.Fatalf("keepalive frames kept arriving after Stop: %d -> %d", before, after)
	}
}

// TestStreamKeepaliveAnthropicPing pins the protocol-specific frame: an
// Anthropic client reads named events, so the comment form would not count.
func TestStreamKeepaliveAnthropicPing(t *testing.T) {
	dst := newRecordingWriter()
	keeper := newStreamKeepalive(dst, wire.Anthropic, 20*time.Millisecond, 90*time.Second)
	defer keeper.Stop()

	time.Sleep(50 * time.Millisecond)
	if got := dst.body(); !strings.Contains(got, "event: ping") {
		t.Fatalf("body = %q, want an Anthropic ping event", got)
	}
}

// TestStreamKeepaliveIdleError pins the in-band failure: when the watchdog
// fires the caller must see a named upstream_idle_timeout, not a stream that
// simply stops, and telemetry must be able to tell it from a client cancel.
func TestStreamKeepaliveIdleError(t *testing.T) {
	for _, protocol := range []wire.Protocol{wire.Chat, wire.Anthropic} {
		dst := newRecordingWriter()
		keeper := newStreamKeepalive(dst, protocol, 0, 90*time.Second)
		keeper.WriteIdle()
		keeper.Stop()
		body := dst.body()
		if !strings.Contains(body, "upstream_idle_timeout") {
			t.Fatalf("%s body = %q, want an upstream_idle_timeout error", protocol, body)
		}
		if !keeper.IdleTripped() {
			t.Fatalf("%s: IdleTripped = false after WriteIdle", protocol)
		}
	}
}

// TestStreamKeepaliveNeverSplitsAnEvent pins the frame-safety rule: io.Copy
// hands over arbitrary chunk boundaries, so a keepalive may only be written
// between complete lines. A partial event must stay intact.
func TestStreamKeepaliveNeverSplitsAnEvent(t *testing.T) {
	dst := newRecordingWriter()
	keeper := newStreamKeepalive(dst, wire.Chat, 20*time.Millisecond, 90*time.Second)
	defer keeper.Stop()

	if _, err := keeper.Write([]byte(`data: {"delta":"hel`)); err != nil {
		t.Fatalf("write: %v", err)
	}
	time.Sleep(50 * time.Millisecond)
	if strings.Contains(dst.body(), "keepalive") {
		t.Fatalf("keepalive was written into a partial line: %q", dst.body())
	}
	if _, err := keeper.Write([]byte("lo\"}\n\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	keeper.Stop()
	if got := dst.body(); !strings.Contains(got, `data: {"delta":"hello"}`) {
		t.Fatalf("body = %q, want the event intact", got)
	}
}

// TestStreamKeepaliveDisabledWritesStraightThrough pins the 0 knob: with no
// interval no timer is started, and a write still reaches the client.
func TestStreamKeepaliveDisabledWritesStraightThrough(t *testing.T) {
	dst := newRecordingWriter()
	keeper := newStreamKeepalive(dst, wire.Chat, 0, 0)
	defer keeper.Stop()
	if _, err := keeper.Write([]byte("data: hello\n\n")); err != nil {
		t.Fatalf("write: %v", err)
	}
	if got := dst.body(); got != "data: hello\n\n" {
		t.Fatalf("body = %q", got)
	}
}
