// Package relay sends free-tier traffic through a Polygate-style HTTPS relay.
//
// The OpenCode free tier accepts a client only when its TLS ClientHello lists
// Bun's cipher suites in Bun's order, and its daily allowance belongs to the
// exit address. A relay is a serverless function that dials the upstream with
// that handshake from its own address, so the gateway keeps speaking plain Go
// TLS to the relay while the free tier sees a Bun client on a fresh egress.
//
// A relay route is configured as a proxy entry with the relay scheme:
//
//	relay://<relay token>@<relay host>[/path]
//
// Transport forwards each request with the relay's own framing (x-relay-target,
// x-relay-path, x-relay-key), reports the spent exit addresses through
// x-relay-avoid, and recycles an instance whose exit is already spent before
// retrying. A final 429 is answered with the free-limit document the gateway
// already classifies as an exhausted allowance, so the lane falls through to
// the next exit or tier instead of retrying the same address.
package relay

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

// Wire header names understood by the relay template (see resources/relays in
// Polygate and deploy/egress-selfhost/relay for the copy used here).
const (
	HeaderTarget   = "x-relay-target"
	HeaderPath     = "x-relay-path"
	HeaderKey      = "x-relay-key"
	HeaderAvoid    = "x-relay-avoid"
	HeaderControl  = "x-relay-control"
	HeaderEgress   = "x-relay-egress"
	HeaderInstance = "x-relay-instance"
	HeaderRotate   = "x-relay-rotate"
	HeaderRuntime  = "x-relay-runtime"
)

const (
	// maxRotations bounds the extra attempts one request may spend recycling
	// instances. Every rotation also costs the relay one function instance, so
	// the budget stays small: three instances per request at most.
	// maxRotations bounds how many times one request asks the relay for another
	// instance. A Vercel project hands out a different address per instance, so
	// a few rotations can find one that is not spent yet; Polygate uses 4.
	maxRotations = 4
	// avoidLimit bounds the x-relay-avoid list; relays refuse oversized headers
	// and an exit older than the list is spent anyway.
	avoidLimit = 64
	// sniffLimit is how much of a 429 body is read to recognise the free-limit
	// document. Everything read is put back for the caller.
	sniffLimit = 64 << 10
	// maxSnapshot bounds a replayed request body.
	maxSnapshot = 64 << 20
	// controlTimeout bounds the out-of-band recycle request.
	controlTimeout = 3 * time.Second
	// quietWindow is how long a relay answers locally after its rotation budget
	// ran out. A burst of clients would otherwise each walk the same spent
	// instances before failing, turning one exhausted relay into a storm of
	// round trips.
	quietWindow = 30 * time.Second
)

var freeLimitText = regexp.MustCompile(`(?i)FreeUsageLimit|Free usage limit`)

// Options configures one relay deployment.
type Options struct {
	// Endpoint is the relay base URL, without userinfo (the token is separate).
	Endpoint *url.URL
	// Token is the relay's access token (POLYGATE_RELAY_TOKEN).
	Token string
	// Timeout bounds the wait for response headers on one attempt.
	Timeout time.Duration
	// Logger receives rotation and exhaustion events.
	Logger *slog.Logger
	// Now is the clock, replaceable in tests.
	Now func() time.Time
}

// Transport is an http.RoundTripper that forwards every request through one
// relay deployment.
type Transport struct {
	endpoint *url.URL
	token    string
	client   *http.Client
	logger   *slog.Logger
	now      func() time.Time

	mu    sync.Mutex
	spent map[string]time.Time
	quiet atomic.Int64
}

// New builds the transport for one relay deployment.
func New(opts Options) (*Transport, error) {
	if opts.Endpoint == nil || opts.Endpoint.Host == "" {
		return nil, errors.New("relay: endpoint URL is required")
	}
	endpoint := *opts.Endpoint
	endpoint.User = nil
	switch strings.ToLower(endpoint.Scheme) {
	case "":
		endpoint.Scheme = "https"
	case "relay":
		endpoint.Scheme = "https"
	case "relay+http":
		endpoint.Scheme = "http"
	}
	logger := opts.Logger
	if logger == nil {
		logger = slog.Default()
	}
	now := opts.Now
	if now == nil {
		now = time.Now
	}
	transport := &http.Transport{
		ForceAttemptHTTP2:     true,
		MaxIdleConns:          32,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       90 * time.Second,
		TLSHandshakeTimeout:   10 * time.Second,
		ResponseHeaderTimeout: opts.Timeout,
	}
	return &Transport{
		endpoint: &endpoint,
		token:    opts.Token,
		client:   &http.Client{Transport: transport},
		logger:   logger,
		now:      now,
		spent:    make(map[string]time.Time),
	}, nil
}

// RoundTrip forwards req through the relay, recycling an instance whose exit
// address is already spent. The request body is snapshotted once, so a retry
// resends exactly the bytes the caller wrote.
func (t *Transport) RoundTrip(req *http.Request) (*http.Response, error) {
	if req.URL == nil || req.URL.Host == "" {
		return nil, errors.New("relay: request URL is required")
	}
	if t.now().UnixNano() < t.quiet.Load() {
		return t.exhausted(req), nil
	}
	snapshot, err := snapshotBody(req)
	if err != nil {
		return nil, err
	}
	ctx := req.Context()
	target := req.URL.Scheme + "://" + req.URL.Host
	path := req.URL.RequestURI()

	var lastErr error
	for attempt := 0; attempt <= maxRotations; attempt++ {
		out, err := t.forward(ctx, req, snapshot, target, path)
		if err != nil {
			lastErr = err
			t.logger.Warn("relay attempt failed", "component", "relay", "event", "relay_attempt_failed",
				"endpoint", t.endpoint.Host, "attempt", attempt+1, "error", err)
			if ctx.Err() != nil {
				return nil, ctx.Err()
			}
			continue
		}
		out.Request = req
		egress := out.Header.Get(HeaderEgress)

		if out.StatusCode == http.StatusConflict && out.Header.Get(HeaderRotate) == "1" {
			drain(out)
			if t.spentNow(egress) {
				// The relay keeps handing out an exit this transport already
				// retired: another attempt would only recycle the same one.
				return t.exhausted(req), nil
			}
			t.markSpent(egress, "relay asked to rotate")
			t.recycle(egress)
			continue
		}
		if out.StatusCode == http.StatusTooManyRequests {
			limited, err := sniffsFreeLimit(out)
			if err != nil {
				return nil, err
			}
			if limited {
				out.Body.Close()
				if t.spentNow(egress) {
					return t.exhausted(req), nil
				}
				t.markSpent(egress, "free allowance spent")
				t.recycle(egress)
				continue
			}
		}
		if out.StatusCode >= 500 && attempt < maxRotations {
			drain(out)
			t.logger.Warn("relay answered a server error; trying another instance", "component", "relay",
				"event", "relay_server_error", "endpoint", t.endpoint.Host, "status", out.StatusCode, "attempt", attempt+1)
			t.recycle(egress)
			continue
		}
		t.quiet.Store(0)
		return out, nil
	}
	if lastErr != nil {
		return nil, lastErr
	}
	// Every allowed instance was recycled: answer locally for a short window so
	// a burst does not repeat the same walk.
	t.quiet.Store(t.now().Add(quietWindow).UnixNano())
	return t.exhausted(req), nil
}

// forward sends one attempt and returns the relay's response untouched.
func (t *Transport) forward(ctx context.Context, req *http.Request, snapshot []byte, target, path string) (*http.Response, error) {
	url := *t.endpoint
	url.Path = singleSlash(url.Path)
	body := io.Reader(nil)
	if req.Body != nil {
		body = bytes.NewReader(snapshot)
	}
	out, err := http.NewRequestWithContext(ctx, req.Method, url.String(), body)
	if err != nil {
		return nil, err
	}
	for name, values := range req.Header {
		if isHopHeader(name) || strings.HasPrefix(strings.ToLower(name), "x-relay-") {
			continue
		}
		for _, value := range values {
			out.Header.Add(name, value)
		}
	}
	out.Header.Set(HeaderTarget, target)
	out.Header.Set(HeaderPath, path)
	out.Header.Set(HeaderKey, t.token)
	if avoid := t.avoid(); avoid != "" {
		out.Header.Set(HeaderAvoid, avoid)
	}
	return t.client.Do(out)
}

// recycle asks the relay to discard the function instance that served an exit,
// so the next attempt lands on an instance with a different address. The call
// is advisory: a relay that does not know the control header answers the
// request normally and the next attempt simply reuses the instance.
func (t *Transport) recycle(egress string) {
	ctx, cancel := context.WithTimeout(context.Background(), controlTimeout)
	defer cancel()
	url := *t.endpoint
	url.Path = singleSlash(url.Path)
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url.String(), nil)
	if err != nil {
		return
	}
	req.Header.Set(HeaderKey, t.token)
	req.Header.Set(HeaderControl, "recycle")
	if egress != "" {
		req.Header.Set(HeaderAvoid, egress)
	}
	resp, err := t.client.Do(req)
	if err != nil {
		t.logger.Debug("relay recycle failed", "component", "relay", "event", "relay_recycle_failed",
			"endpoint", t.endpoint.Host, "error", err)
		return
	}
	drain(resp)
	t.logger.Info("relay instance recycled", "component", "relay", "event", "relay_recycled",
		"endpoint", t.endpoint.Host, "egress", egress)
}

// markSpent records that egress has no free allowance left today. An empty
// egress (a relay that does not report one) cannot be remembered.
func (t *Transport) markSpent(egress, reason string) {
	egress = strings.TrimSpace(egress)
	if egress == "" {
		return
	}
	until := nextUTCReset(t.now())
	t.mu.Lock()
	previous, known := t.spent[egress]
	if known && !until.After(previous) {
		t.mu.Unlock()
		return
	}
	t.spent[egress] = until
	t.mu.Unlock()
	t.logger.Warn("relay exit address spent", "component", "relay", "event", "relay_egress_spent",
		"endpoint", t.endpoint.Host, "egress", egress, "until", until.UTC().Format(time.RFC3339), "reason", reason)
}

// avoid renders the spent exit addresses as the relay's avoid list, dropping
// entries whose allowance has reset since they were recorded.
func (t *Transport) avoid() string {
	now := t.now()
	t.mu.Lock()
	defer t.mu.Unlock()
	values := make([]string, 0, len(t.spent))
	for egress, until := range t.spent {
		if now.Before(until) {
			values = append(values, egress)
			continue
		}
		delete(t.spent, egress)
	}
	if len(values) > avoidLimit {
		values = values[:avoidLimit]
	}
	return strings.Join(values, ",")
}

// spentNow reports whether egress has no allowance left today.
func (t *Transport) spentNow(egress string) bool {
	egress = strings.TrimSpace(egress)
	if egress == "" {
		return false
	}
	t.mu.Lock()
	defer t.mu.Unlock()
	until, known := t.spent[egress]
	return known && t.now().Before(until)
}

// SpentCount reports how many exit addresses are currently spent, for status.
func (t *Transport) SpentCount() int {
	now := t.now()
	t.mu.Lock()
	defer t.mu.Unlock()
	count := 0
	for _, until := range t.spent {
		if now.Before(until) {
			count++
		}
	}
	return count
}

// exhausted answers a request whose every allowed instance was recycled.
func (t *Transport) exhausted(req *http.Request) *http.Response {
	until := nextUTCReset(t.now())
	body, _ := json.Marshal(map[string]any{
		"type": "error",
		"error": map[string]any{
			"type":    "FreeUsageLimitError",
			"message": "Free usage limit reached. Every instance of this relay that was tried is used up.",
		},
	})
	header := http.Header{}
	header.Set("Content-Type", "application/json")
	header.Set("Retry-After", fmt.Sprintf("%d", int(until.Sub(t.now()).Seconds())+1))
	return &http.Response{
		Status:        "429 Too Many Requests",
		StatusCode:    http.StatusTooManyRequests,
		Proto:         "HTTP/1.1",
		ProtoMajor:    1,
		ProtoMinor:    1,
		Header:        header,
		Body:          io.NopCloser(bytes.NewReader(body)),
		ContentLength: int64(len(body)),
		Request:       req,
	}
}

// snapshotBody reads the request body once so retries can replay it.
func snapshotBody(req *http.Request) ([]byte, error) {
	if req.Body == nil {
		return nil, nil
	}
	if req.GetBody != nil {
		body, err := req.GetBody()
		if err == nil {
			defer body.Close()
			return io.ReadAll(io.LimitReader(body, maxSnapshot+1))
		}
	}
	data, err := io.ReadAll(io.LimitReader(req.Body, maxSnapshot+1))
	req.Body.Close()
	if err != nil {
		return nil, fmt.Errorf("relay: read request body: %w", err)
	}
	if len(data) > maxSnapshot {
		return nil, errors.New("relay: request body exceeds the replay limit")
	}
	req.Body = io.NopCloser(bytes.NewReader(data))
	return data, nil
}

// sniffsFreeLimit reports whether a 429 carries the free-allowance document,
// leaving the body whole for a caller that wants to forward it.
func sniffsFreeLimit(resp *http.Response) (bool, error) {
	if resp.Body == nil {
		return false, nil
	}
	head, err := io.ReadAll(io.LimitReader(resp.Body, sniffLimit))
	if err != nil {
		return false, err
	}
	resp.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(head), resp.Body), resp.Body}
	return freeLimitText.Match(head), nil
}

// drain reads a bounded amount and closes the body so the connection can be
// reused.
func drain(resp *http.Response) {
	if resp == nil || resp.Body == nil {
		return
	}
	io.CopyN(io.Discard, resp.Body, sniffLimit)
	resp.Body.Close()
}

// isHopHeader reports whether a request header describes the connection
// rather than the message.
func isHopHeader(name string) bool {
	switch strings.ToLower(name) {
	case "connection", "keep-alive", "proxy-connection", "transfer-encoding", "upgrade",
		"te", "trailer", "content-length", "host", "accept-encoding":
		return true
	}
	return false
}

// singleSlash keeps a relay that is deployed under a sub-path working.
func singleSlash(path string) string {
	if path == "" {
		return "/"
	}
	return path
}

// nextUTCReset is the instant the free allowance resets: the next midnight UTC.
func nextUTCReset(now time.Time) time.Time {
	utc := now.UTC()
	return time.Date(utc.Year(), utc.Month(), utc.Day(), 0, 0, 0, 0, time.UTC).Add(24 * time.Hour)
}
