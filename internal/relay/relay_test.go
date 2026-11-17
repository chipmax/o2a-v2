package relay

import (
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

// recorder is a stand-in relay: it answers each forwarded request with the next
// response in a script and remembers the framing headers it received.
type recorder struct {
	mu       sync.Mutex
	script   []scripted
	forwards []http.Header
	bodies   []string
	controls []string
}

type scripted struct {
	status int
	egress string
	rotate bool
	body   string
}

func (r *recorder) ServeHTTP(w http.ResponseWriter, req *http.Request) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if req.Header.Get(HeaderControl) != "" {
		r.controls = append(r.controls, req.Header.Get(HeaderControl))
		w.WriteHeader(http.StatusNoContent)
		return
	}
	r.forwards = append(r.forwards, req.Header.Clone())
	body, _ := io.ReadAll(req.Body)
	r.bodies = append(r.bodies, string(body))
	step := scripted{status: http.StatusOK, body: `{"ok":true}`}
	if len(r.script) > 0 {
		step = r.script[0]
		if len(r.script) > 1 {
			r.script = r.script[1:]
		}
	}
	if step.egress != "" {
		w.Header().Set(HeaderEgress, step.egress)
	}
	if step.rotate {
		w.Header().Set(HeaderRotate, "1")
	}
	w.WriteHeader(step.status)
	io.WriteString(w, step.body)
}

func (r *recorder) reset() {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.forwards = nil
	r.bodies = nil
	r.controls = nil
}

func newTransport(t *testing.T, endpoint string, now func() time.Time) *Transport {
	t.Helper()
	u, err := url.Parse(endpoint)
	if err != nil {
		t.Fatalf("parse endpoint: %v", err)
	}
	transport, err := New(Options{Endpoint: u, Token: "tok", Timeout: 5 * time.Second, Now: now})
	if err != nil {
		t.Fatalf("new transport: %v", err)
	}
	return transport
}

func send(t *testing.T, transport *Transport, body string) (*http.Response, error) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, "https://opencode.ai/inference/v1/chat/completions",
		strings.NewReader(body))
	if err != nil {
		t.Fatalf("new request: %v", err)
	}
	req.Header.Set("content-type", "application/json")
	return transport.RoundTrip(req)
}

func TestForwardFramesRequest(t *testing.T) {
	rec := &recorder{script: []scripted{{status: http.StatusOK, egress: "9.9.9.9", body: `{"answer":1}`}}}
	server := httptest.NewServer(rec)
	defer server.Close()

	transport := newTransport(t, server.URL, time.Now)
	resp, err := send(t, transport, `{"model":"x"}`)
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	defer resp.Body.Close()
	data, _ := io.ReadAll(resp.Body)
	if string(data) != `{"answer":1}` {
		t.Fatalf("body = %q", data)
	}
	if len(rec.forwards) != 1 {
		t.Fatalf("forwards = %d", len(rec.forwards))
	}
	header := rec.forwards[0]
	if got := header.Get(HeaderTarget); got != "https://opencode.ai" {
		t.Fatalf("target = %q", got)
	}
	if got := header.Get(HeaderPath); got != "/inference/v1/chat/completions" {
		t.Fatalf("path = %q", got)
	}
	if got := header.Get(HeaderKey); got != "tok" {
		t.Fatalf("key = %q", got)
	}
	if got := rec.bodies[0]; got != `{"model":"x"}` {
		t.Fatalf("forwarded body = %q", got)
	}
}

func TestRotatesOnConflict(t *testing.T) {
	rec := &recorder{script: []scripted{
		{status: http.StatusConflict, egress: "1.2.3.4", rotate: true, body: "occupied"},
		{status: http.StatusOK, egress: "5.6.7.8", body: `{"ok":true}`},
	}}
	server := httptest.NewServer(rec)
	defer server.Close()

	transport := newTransport(t, server.URL, time.Now)
	resp, err := send(t, transport, `{}`)
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if len(rec.forwards) != 2 {
		t.Fatalf("forwards = %d", len(rec.forwards))
	}
	if len(rec.controls) != 1 || rec.controls[0] != "recycle" {
		t.Fatalf("controls = %v", rec.controls)
	}
	if got := rec.forwards[1].Get(HeaderAvoid); !strings.Contains(got, "1.2.3.4") {
		t.Fatalf("avoid = %q", got)
	}
	if transport.SpentCount() != 1 {
		t.Fatalf("spent = %d", transport.SpentCount())
	}
}

func TestRecyclesSpentExit(t *testing.T) {
	rec := &recorder{script: []scripted{
		{status: http.StatusTooManyRequests, egress: "2.2.2.2",
			body: `{"error":{"type":"FreeUsageLimitError","message":"Free usage limit reached"}}`},
		{status: http.StatusOK, egress: "3.3.3.3", body: `{"ok":true}`},
	}}
	server := httptest.NewServer(rec)
	defer server.Close()

	transport := newTransport(t, server.URL, time.Now)
	resp, err := send(t, transport, `{}`)
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if got := rec.forwards[1].Get(HeaderAvoid); !strings.Contains(got, "2.2.2.2") {
		t.Fatalf("avoid = %q", got)
	}
	if transport.SpentCount() != 1 {
		t.Fatalf("spent = %d", transport.SpentCount())
	}
}

func TestExhaustsAfterRotationBudget(t *testing.T) {
	rec := &recorder{script: spentScript("4.4.4")}
	server := httptest.NewServer(rec)
	defer server.Close()

	transport := newTransport(t, server.URL, time.Now)
	resp, err := send(t, transport, `{}`)
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	data, _ := io.ReadAll(resp.Body)
	if !freeLimitText.Match(data) {
		t.Fatalf("body = %q", data)
	}
	if len(rec.forwards) != maxRotations+1 {
		t.Fatalf("forwards = %d, want %d", len(rec.forwards), maxRotations+1)
	}
	if transport.SpentCount() != maxRotations+1 {
		t.Fatalf("spent = %d", transport.SpentCount())
	}
}

func TestRetriesServerError(t *testing.T) {
	rec := &recorder{script: []scripted{
		{status: http.StatusBadGateway, body: "bad"},
		{status: http.StatusOK, body: `{"ok":true}`},
	}}
	server := httptest.NewServer(rec)
	defer server.Close()

	transport := newTransport(t, server.URL, time.Now)
	resp, err := send(t, transport, `{}`)
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if len(rec.forwards) != 2 {
		t.Fatalf("forwards = %d", len(rec.forwards))
	}
}

func TestPassesThroughOtherRateLimit(t *testing.T) {
	rec := &recorder{script: []scripted{
		{status: http.StatusTooManyRequests, egress: "8.8.8.8", body: `{"error":{"message":"slow down"}}`},
	}}
	server := httptest.NewServer(rec)
	defer server.Close()

	transport := newTransport(t, server.URL, time.Now)
	resp, err := send(t, transport, `{}`)
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	data, _ := io.ReadAll(resp.Body)
	if !strings.Contains(string(data), "slow down") {
		t.Fatalf("body = %q", data)
	}
	if len(rec.forwards) != 1 {
		t.Fatalf("forwards = %d", len(rec.forwards))
	}
	if transport.SpentCount() != 0 {
		t.Fatalf("spent = %d", transport.SpentCount())
	}
}

func TestReplaysBodyOnRetry(t *testing.T) {
	rec := &recorder{script: []scripted{
		{status: http.StatusConflict, egress: "7.7.7.7", rotate: true, body: "occupied"},
		{status: http.StatusOK, body: `{"ok":true}`},
	}}
	server := httptest.NewServer(rec)
	defer server.Close()

	transport := newTransport(t, server.URL, time.Now)
	resp, err := send(t, transport, `{"model":"m","messages":[{"role":"user","content":"hi"}]}`)
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	defer resp.Body.Close()
	if len(rec.bodies) != 2 || rec.bodies[0] != rec.bodies[1] {
		t.Fatalf("bodies = %q", rec.bodies)
	}
	if !strings.Contains(rec.bodies[1], `"content":"hi"`) {
		t.Fatalf("body = %q", rec.bodies[1])
	}
}

func TestRepeatedSpentExitStopsEarly(t *testing.T) {
	rec := &recorder{script: []scripted{
		{status: http.StatusTooManyRequests, egress: "4.1.1.1", body: `FreeUsageLimit reached`},
	}}
	server := httptest.NewServer(rec)
	defer server.Close()

	transport := newTransport(t, server.URL, time.Now)
	resp, err := send(t, transport, `{}`)
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("status = %d", resp.StatusCode)
	}
	if len(rec.forwards) != 2 {
		t.Fatalf("forwards = %d, want 2 (mark, then recognise the same exit)", len(rec.forwards))
	}
	if transport.SpentCount() != 1 {
		t.Fatalf("spent = %d", transport.SpentCount())
	}
}

// spentScript answers with one spent address per attempt, all distinct, so a
// request really walks its whole rotation budget.
func spentScript(prefix string) []scripted {
	script := make([]scripted, 0, maxRotations+1)
	for i := 0; i <= maxRotations; i++ {
		script = append(script, scripted{status: http.StatusTooManyRequests, egress: fmt.Sprintf("%s.%d", prefix, i+1), body: `FreeUsageLimit reached`})
	}
	return script
}

func TestQuietWindowAfterExhaustion(t *testing.T) {
	rec := &recorder{script: spentScript("9.1.1")}
	server := httptest.NewServer(rec)
	defer server.Close()

	clock := time.Date(2026, 11, 11, 3, 0, 0, 0, time.UTC)
	transport := newTransport(t, server.URL, func() time.Time { return clock })
	first, err := send(t, transport, `{}`)
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	first.Body.Close()
	if len(rec.forwards) != maxRotations+1 {
		t.Fatalf("first walk = %d", len(rec.forwards))
	}
	rec.reset()
	second, err := send(t, transport, `{}`)
	if err != nil {
		t.Fatalf("quiet round trip: %v", err)
	}
	second.Body.Close()
	if second.StatusCode != http.StatusTooManyRequests {
		t.Fatalf("quiet status = %d", second.StatusCode)
	}
	if len(rec.forwards) != 0 {
		t.Fatalf("quiet window forwarded %d requests", len(rec.forwards))
	}
	clock = clock.Add(quietWindow + time.Second)
	third, err := send(t, transport, `{}`)
	if err != nil {
		t.Fatalf("post-window round trip: %v", err)
	}
	third.Body.Close()
	if len(rec.forwards) == 0 {
		t.Fatal("the window must expire and the relay must be tried again")
	}
}

func TestQuietWindowClearsOnSuccess(t *testing.T) {
	rec := &recorder{script: []scripted{
		{status: http.StatusTooManyRequests, egress: "9.2.2.1", body: `FreeUsageLimit reached`},
		{status: http.StatusTooManyRequests, egress: "9.2.2.2", body: `FreeUsageLimit reached`},
		{status: http.StatusTooManyRequests, egress: "9.2.2.3", body: `FreeUsageLimit reached`},
	}}
	server := httptest.NewServer(rec)
	defer server.Close()

	transport := newTransport(t, server.URL, time.Now)
	resp, err := send(t, transport, `{}`)
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	resp.Body.Close()
	rec.reset()
	rec.script = []scripted{{status: http.StatusOK, egress: "9.2.2.9", body: `{"ok":true}`}}
	clock := time.Now().Add(quietWindow + time.Second)
	transport.now = func() time.Time { return clock }
	resp, err = send(t, transport, `{}`)
	if err != nil {
		t.Fatalf("recovery round trip: %v", err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK || len(rec.forwards) != 1 {
		t.Fatalf("status=%d forwards=%d", resp.StatusCode, len(rec.forwards))
	}
	rec.reset()
	resp, err = send(t, transport, `{}`)
	if err != nil {
		t.Fatalf("after success: %v", err)
	}
	resp.Body.Close()
	if len(rec.forwards) != 1 {
		t.Fatalf("a success must clear the quiet window, forwards=%d", len(rec.forwards))
	}
}

func TestAvoidExpiresAtUTCMidnight(t *testing.T) {
	rec := &recorder{script: []scripted{
		{status: http.StatusTooManyRequests, egress: "6.6.6.6", body: `FreeUsageLimit reached`},
		{status: http.StatusTooManyRequests, egress: "6.6.6.7", body: `FreeUsageLimit reached`},
		{status: http.StatusTooManyRequests, egress: "6.6.6.8", body: `FreeUsageLimit reached`},
	}}
	server := httptest.NewServer(rec)
	defer server.Close()

	clock := time.Date(2026, 11, 11, 23, 0, 0, 0, time.UTC)
	transport := newTransport(t, server.URL, func() time.Time { return clock })
	resp, err := send(t, transport, `{}`)
	if err != nil {
		t.Fatalf("round trip: %v", err)
	}
	resp.Body.Close()
	if transport.SpentCount() != 3 {
		t.Fatalf("spent = %d", transport.SpentCount())
	}
	clock = clock.Add(2 * time.Hour)
	if transport.SpentCount() != 0 {
		t.Fatalf("spent after reset = %d", transport.SpentCount())
	}
	if avoid := transport.avoid(); avoid != "" {
		t.Fatalf("avoid after reset = %q", avoid)
	}
}
