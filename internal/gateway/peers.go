package gateway

import (
	"bytes"
	"context"
	"io"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"opencode2api/internal/config"
)

// peerFallbackCodes are statuses that mean "this peer cannot serve now": the
// request moves to the next peer instead of reaching the client. 5xx covers a
// peer whose own lanes are exhausted; the peer answers 429/502/503 for that.
var peerFallbackCodes = map[int]bool{
	http.StatusUnauthorized:        true,
	http.StatusForbidden:           true,
	http.StatusTooManyRequests:     true,
	http.StatusInternalServerError: true,
	http.StatusBadGateway:          true,
	http.StatusServiceUnavailable:  true,
	http.StatusGatewayTimeout:      true,
}

// peerSoftCodes are answerable by another peer when the body looks like a
// model-catalog mismatch rather than a malformed request.
var peerSoftCodes = map[int]bool{
	http.StatusBadRequest: true,
	http.StatusNotFound:   true,
}

var peerSoftMarkers = []string{
	"not available",
	"not found",
	"not registered",
	"unknown model",
	"does not exist",
	"unsupported model",
	"no such model",
	"model_not_found",
	"not supported",
}

// peerPool forwards a request to another o2a gateway when every local lane has
// failed. It is the in-process replacement for the external router: one listen
// address serves the whole fleet, ordered by tier with round-robin inside a
// tier, cooldowns on failure and Retry-After awareness.
type peerPool struct {
	peers  []config.PeerConfig
	client *http.Client

	mu      sync.Mutex
	cooling map[string]time.Time
	cursor  map[int]int
}

func newPeerPool(peers []config.PeerConfig) *peerPool {
	if len(peers) == 0 {
		return nil
	}
	ordered := append([]config.PeerConfig(nil), peers...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Tier < ordered[j].Tier })
	return &peerPool{
		peers:   ordered,
		cooling: make(map[string]time.Time),
		cursor:  make(map[int]int),
		client: &http.Client{
			Transport: &http.Transport{
				Proxy:               http.ProxyFromEnvironment,
				MaxIdleConns:        32,
				MaxIdleConnsPerHost: 8,
				IdleConnTimeout:     90 * time.Second,
				// A forwarded response may be a long event stream, so the
				// client has no overall timeout; each attempt is bounded by
				// the per-peer timeout through its own context.
				ResponseHeaderTimeout: 0,
			},
		},
	}
}

func (p *peerPool) Len() int {
	if p == nil {
		return 0
	}
	return len(p.peers)
}

// ordered returns the peers to try for model: hot ones first, tier by tier with
// the cursor rotating inside each tier, then cooling ones as a last resort.
func (p *peerPool) ordered(model string) []config.PeerConfig {
	now := time.Now()
	p.mu.Lock()
	defer p.mu.Unlock()
	hot := make([]config.PeerConfig, 0, len(p.peers))
	cold := make([]config.PeerConfig, 0, len(p.peers))
	for _, peer := range p.peers {
		if !peer.PeerAllowed(model) {
			continue
		}
		if p.cooling[peer.Name].After(now) {
			cold = append(cold, peer)
			continue
		}
		hot = append(hot, peer)
	}
	// Round-robin inside a tier so equal machines share the load.
	start := 0
	if len(hot) > 1 {
		firstTier := hot[0].Tier
		start = p.cursor[firstTier] % len(hot)
		for !(hot[start].Tier == firstTier) {
			start++
		}
		p.cursor[firstTier] = (start + 1) % len(hot)
	}
	if start > 0 && start < len(hot) {
		hot = append(append([]config.PeerConfig(nil), hot[start:]...), hot[:start]...)
	}
	return append(hot, cold...)
}

// cool marks a peer unusable for a while. A numeric Retry-After wins for 429,
// clamped so a malformed header cannot pin a peer forever.
func (p *peerPool) cool(peer config.PeerConfig, status int, header http.Header) time.Duration {
	delay := time.Duration(peer.CooldownSeconds) * time.Second
	if retryAfter := parseRetryAfter(header.Get("Retry-After")); retryAfter > delay {
		delay = min(retryAfter, 15*time.Minute)
	}
	if status == http.StatusUnauthorized || status == http.StatusForbidden {
		delay = min(delay, 30*time.Second)
	}
	p.mu.Lock()
	p.cooling[peer.Name] = time.Now().Add(delay)
	p.mu.Unlock()
	return delay
}

func (p *peerPool) clear(peer config.PeerConfig) {
	p.mu.Lock()
	delete(p.cooling, peer.Name)
	p.mu.Unlock()
}

// peerSnapshot reports the peers and their cooling state for /healthz.
func (p *peerPool) peerSnapshot() []map[string]any {
	if p == nil {
		return nil
	}
	now := time.Now()
	p.mu.Lock()
	defer p.mu.Unlock()
	out := make([]map[string]any, 0, len(p.peers))
	for _, peer := range p.peers {
		entry := map[string]any{"name": peer.Name, "tier": peer.Tier, "base_url": config.RedactURL(peer.BaseURL)}
		if until := p.cooling[peer.Name]; until.After(now) {
			entry["cooling_seconds"] = int(until.Sub(now).Seconds())
		}
		out = append(out, entry)
	}
	return out
}

// markLocalQuotaSpent remembers that the local free-tier bucket of a model
// family is spent until the counting window resets. Later requests for that
// family go to a peer first instead of spending 8-30s rotating exits that
// cannot serve them.
func (g *Gateway) markLocalQuotaSpent(model string, now time.Time, until time.Time) {
	if g == nil || model == "" {
		return
	}
	if !until.After(now) {
		return
	}
	family := model
	if len(family) > 2 {
		family = family[:2]
	}
	g.peerMu.Lock()
	if g.peerFirst == nil {
		g.peerFirst = make(map[string]time.Time)
	}
	if until.After(g.peerFirst[family]) {
		g.peerFirst[family] = until
	}
	g.peerMu.Unlock()
}

// localBucketSpent reports whether the local free-tier bucket of a model family
// is known to be spent for the rest of its window.
func (g *Gateway) localBucketSpent(model string) bool {
	if g == nil {
		return false
	}
	family := model
	if len(family) > 2 {
		family = family[:2]
	}
	g.peerMu.Lock()
	defer g.peerMu.Unlock()
	return time.Now().Before(g.peerFirst[family])
}

// tryPeers forwards the original request body to the fallback peers in order
// and writes the first usable answer. It reports whether a response was
// written; false means every peer failed and the caller should answer itself.
func (g *Gateway) tryPeers(ctx context.Context, w http.ResponseWriter, r *http.Request, body []byte, model, requestID string) bool {
	pool := g.peers
	if pool == nil {
		return false
	}
	type softReply struct {
		status int
		header http.Header
		body   []byte
	}
	var soft *softReply
	for _, peer := range pool.ordered(model) {
		attemptCtx, cancel := context.WithTimeout(ctx, time.Duration(peer.TimeoutSeconds)*time.Second)
		target := peer.BaseURL + r.URL.Path
		if r.URL.RawQuery != "" {
			target += "?" + r.URL.RawQuery
		}
		req, err := http.NewRequestWithContext(attemptCtx, r.Method, target, bytes.NewReader(body))
		if err != nil {
			cancel()
			continue
		}
		copyPeerHeaders(req.Header, r.Header)
		req.Header.Set("Authorization", "Bearer "+peer.Key)
		req.ContentLength = int64(len(body))
		started := time.Now()
		resp, err := pool.client.Do(req)
		if err != nil {
			cancel()
			delay := pool.cool(peer, 0, http.Header{})
			g.logger.Warn("peer attempt failed", "component", "peers", "event", "peer_transport_failed", "request_id", requestID, "peer", peer.Name, "cooldown_s", int(delay.Seconds()), "error", err)
			continue
		}
		status := resp.StatusCode
		if status/100 == 2 {
			pool.clear(peer)
			g.writePeerResponse(w, resp, peer.Name, requestID, cancel)
			g.logger.Info("request served by peer", "component", "peers", "event", "peer_served", "request_id", requestID, "peer", peer.Name, "model", model, "status", status, "duration_ms", time.Since(started).Milliseconds())
			return true
		}
		if peerSoftCodes[status] && peerSoftMarker(resp) {
			blob, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
			resp.Body.Close()
			cancel()
			soft = &softReply{status: status, header: resp.Header.Clone(), body: blob}
			continue
		}
		if peerFallbackCodes[status] {
			delay := pool.cool(peer, status, resp.Header)
			io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
			resp.Body.Close()
			cancel()
			g.logger.Warn("peer answered a fallback status", "component", "peers", "event", "peer_fallback", "request_id", requestID, "peer", peer.Name, "model", model, "status", status, "cooldown_s", int(delay.Seconds()))
			continue
		}
		// A request-level rejection: forward it as-is so the client sees the
		// real reason instead of a gateway failure.
		pool.clear(peer)
		g.writePeerResponse(w, resp, peer.Name, requestID, cancel)
		return true
	}
	if soft != nil {
		for key, values := range soft.header {
			if isHopByHopHeader(key) {
				continue
			}
			for _, value := range values {
				w.Header().Add(key, value)
			}
		}
		w.Header().Set("X-Peer", "none")
		w.Header().Set("x-request-id", requestID)
		w.WriteHeader(soft.status)
		_, _ = w.Write(soft.body)
		return true
	}
	return false
}

// writePeerResponse copies a peer's response through, flushing as it goes so an
// event stream reaches the client in real time, and releases the attempt
// context when the body ends.
func (g *Gateway) writePeerResponse(w http.ResponseWriter, resp *http.Response, peerName, requestID string, cancel context.CancelFunc) {
	defer cancel()
	defer resp.Body.Close()
	for key, values := range resp.Header {
		if isHopByHopHeader(key) || strings.EqualFold(key, "Content-Length") {
			continue
		}
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.Header().Set("X-Peer", peerName)
	w.Header().Set("x-request-id", requestID)
	w.WriteHeader(resp.StatusCode)
	flusher, _ := w.(http.Flusher)
	buf := make([]byte, 16<<10)
	for {
		n, err := resp.Body.Read(buf)
		if n > 0 {
			if _, werr := w.Write(buf[:n]); werr != nil {
				return
			}
			if flusher != nil {
				flusher.Flush()
			}
		}
		if err != nil {
			return
		}
	}
}

// copyPeerHeaders keeps the headers that shape a request (protocol, session
// affinity, encodings) and drops the caller's credential and hop-by-hop ones.
func copyPeerHeaders(dst, src http.Header) {
	for _, key := range []string{
		"Content-Type", "Accept", "User-Agent",
		"X-Session-Id", "X-Opencode-Session", "Session_Id",
		"Conversation-Id", "Conversation_Id",
		"Accept-Encoding", "Cache-Control",
	} {
		if values, ok := src[key]; ok {
			for _, value := range values {
				dst.Add(key, value)
			}
		}
	}
}

func isHopByHopHeader(key string) bool {
	switch strings.ToLower(key) {
	case "connection", "keep-alive", "proxy-authenticate", "proxy-authorization",
		"te", "trailer", "transfer-encoding", "upgrade":
		return true
	}
	return false
}

// peerSoftMarker reports whether a 400/404 body names a model problem rather
// than a malformed request, without consuming the body for the caller.
func peerSoftMarker(resp *http.Response) bool {
	blob, err := io.ReadAll(io.LimitReader(resp.Body, 8<<10))
	if err != nil {
		return false
	}
	resp.Body = io.NopCloser(io.MultiReader(bytes.NewReader(blob), resp.Body))
	text := strings.ToLower(string(blob))
	for _, marker := range peerSoftMarkers {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}
