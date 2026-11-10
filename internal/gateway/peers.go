package gateway

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/http"
	"sort"
	"strings"
	"sync"
	"time"

	"opencode2api/internal/config"
	wire "opencode2api/internal/protocol"
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
	peers []config.PeerConfig
	// clients is keyed by peer name; each client bounds only the wait for
	// response headers. An established SSE stream must keep flowing for as
	// long as the upstream and client want, so the request context is not
	// wrapped in an overall timeout here.
	clients map[string]*http.Client
	// fallback bounds a peer whose name is missing from clients, so a stale
	// or hand-built pool cannot hang the request forever.
	fallback *http.Client

	mu      sync.Mutex
	cooling map[string]time.Time
	cursor  map[int]int
}

func peerTransport(headerWait time.Duration) *http.Transport {
	return &http.Transport{
		Proxy:                 http.ProxyFromEnvironment,
		DialContext:           (&net.Dialer{Timeout: 10 * time.Second, KeepAlive: 30 * time.Second}).DialContext,
		MaxIdleConns:          32,
		MaxIdleConnsPerHost:   8,
		IdleConnTimeout:       90 * time.Second,
		ResponseHeaderTimeout: headerWait,
	}
}

func newPeerPool(peers []config.PeerConfig) *peerPool {
	if len(peers) == 0 {
		return nil
	}
	ordered := append([]config.PeerConfig(nil), peers...)
	sort.SliceStable(ordered, func(i, j int) bool { return ordered[i].Tier < ordered[j].Tier })
	clients := make(map[string]*http.Client, len(ordered))
	for _, peer := range ordered {
		headerWait := time.Duration(peer.TimeoutSeconds) * time.Second
		if headerWait <= 0 {
			headerWait = 180 * time.Second
		}
		clients[peer.Name] = &http.Client{Transport: peerTransport(headerWait)}
	}
	return &peerPool{
		peers:    ordered,
		clients:  clients,
		fallback: &http.Client{Transport: peerTransport(180 * time.Second)},
		cooling:  make(map[string]time.Time),
		cursor:   make(map[int]int),
	}
}

func (p *peerPool) clientFor(peer config.PeerConfig) *http.Client {
	if client := p.clients[peer.Name]; client != nil {
		return client
	}
	if p.fallback != nil {
		return p.fallback
	}
	return &http.Client{Transport: peerTransport(180 * time.Second)}
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
	// Round-robin inside the first tier so equal machines share the load while
	// lower-tier peers still come before higher-tier ones.
	start, tierEnd := 0, len(hot)
	if len(hot) > 1 {
		firstTier := hot[0].Tier
		tierEnd = 1
		for tierEnd < len(hot) && hot[tierEnd].Tier == firstTier {
			tierEnd++
		}
		start = p.cursor[firstTier] % tierEnd
		p.cursor[firstTier] = (start + 1) % tierEnd
	}
	if start > 0 {
		rotated := make([]config.PeerConfig, 0, len(hot))
		rotated = append(rotated, hot[start:tierEnd]...)
		rotated = append(rotated, hot[:start]...)
		rotated = append(rotated, hot[tierEnd:]...)
		hot = rotated
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
func (g *Gateway) tryPeers(ctx context.Context, w http.ResponseWriter, r *http.Request, body []byte, model string, tier config.Tier, requestID string) bool {
	served, _ := g.tryPeersStatus(ctx, w, r, body, model, tier, requestID)
	return served
}

// tryPeersStatus forwards to peers and reports whether a response was written
// plus its status, so a caller can refund a quota slot when the peer rejected
// the request instead of serving it.
func (g *Gateway) tryPeersStatus(ctx context.Context, w http.ResponseWriter, r *http.Request, body []byte, model string, tier config.Tier, requestID string) (bool, int) {
	pool := g.peers
	if pool == nil {
		return false, 0
	}
	type softReply struct {
		status int
		header http.Header
		body   []byte
	}
	var soft *softReply
	for _, peer := range pool.ordered(model) {
		target := peer.BaseURL + r.URL.Path
		if r.URL.RawQuery != "" {
			target += "?" + r.URL.RawQuery
		}
		// The peer client bounds only the response-header wait. Wrapping the
		// whole request in the peer timeout used to cut long SSE streams.
		req, err := http.NewRequestWithContext(ctx, r.Method, target, bytes.NewReader(body))
		if err != nil {
			continue
		}
		copyPeerHeaders(req.Header, r.Header)
		req.Header.Set("Authorization", "Bearer "+peer.Key)
		req.ContentLength = int64(len(body))
		started := time.Now()
		resp, err := pool.clientFor(peer).Do(req)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, context.Canceled) {
				// The caller went away. Cooling a healthy peer for a client
				// disconnect would let a burst of abandoned requests empty the
				// fallback fleet.
				return false, 0
			}
			delay := pool.cool(peer, 0, http.Header{})
			g.logger.Warn("peer attempt failed", "component", "peers", "event", "peer_transport_failed", "request_id", requestID, "peer", peer.Name, "cooldown_s", int(delay.Seconds()), "error", err)
			continue
		}
		status := resp.StatusCode
		if status/100 == 2 {
			pool.clear(peer)
			g.writePeerSuccess(r.Context(), w, resp, r.URL.Path, model, tier, int64(len(body)), peer.Name, requestID)
			g.logger.Info("request served by peer", "component", "peers", "event", "peer_served", "request_id", requestID, "peer", peer.Name, "model", model, "status", status, "duration_ms", time.Since(started).Milliseconds())
			return true, status
		}
		if peerSoftCodes[status] && peerSoftMarker(resp) {
			blob, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
			resp.Body.Close()
			soft = &softReply{status: status, header: resp.Header.Clone(), body: blob}
			continue
		}
		if peerFallbackCodes[status] {
			delay := pool.cool(peer, status, resp.Header)
			io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
			resp.Body.Close()
			g.logger.Warn("peer answered a fallback status", "component", "peers", "event", "peer_fallback", "request_id", requestID, "peer", peer.Name, "model", model, "status", status, "cooldown_s", int(delay.Seconds()))
			continue
		}
		// A request-level rejection: forward it as-is so the client sees the
		// real reason instead of a gateway failure.
		pool.clear(peer)
		if status == http.StatusBadRequest || status == http.StatusRequestEntityTooLarge || status == http.StatusUnprocessableEntity {
			prefix := peekResponseBody(resp)
			if !isDiagnosticRequest(r.Context()) {
				if advertised, applied := g.noteContextRejection(w, model, tier, status, prefix, int64(len(body))); advertised > 0 {
					g.logger.Warn("peer context limit exceeded", "component", "peers", "event", "peer_context_limit_exceeded", "request_id", requestID, "peer", peer.Name, "model", model, "tier", tier, "advertised", advertised, "advertisement_updated", applied)
				}
			}
		}
		g.writePeerResponse(w, resp, peer.Name, requestID)
		return true, status
	}
	if soft != nil {
		for key, values := range soft.header {
			if isHopByHopHeader(key) || strings.EqualFold(key, "Content-Length") {
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
		return true, soft.status
	}
	return false, 0
}

// peerProtocol maps the client endpoint to the wire protocol of its response.
func peerProtocol(path string) wire.Protocol {
	switch strings.TrimSuffix(path, "/") {
	case "/v1/responses":
		return wire.Responses
	case "/v1/messages":
		return wire.Anthropic
	case "/v1/systemone":
		return wire.SystemOne
	default:
		return wire.Chat
	}
}

// copyPeerResponseHeaders keeps the headers that shape a response and drops
// hop-by-hop ones. A locally computed X-Context-Limit wins over the peer's.
func copyPeerResponseHeaders(w http.ResponseWriter, resp *http.Response, peerName, requestID string) {
	for key, values := range resp.Header {
		if isHopByHopHeader(key) || strings.EqualFold(key, "Content-Length") {
			continue
		}
		if strings.EqualFold(key, "X-Context-Limit") && w.Header().Get("X-Context-Limit") != "" {
			continue
		}
		for _, value := range values {
			w.Header().Add(key, value)
		}
	}
	w.Header().Set("X-Peer", peerName)
	w.Header().Set("x-request-id", requestID)
}

// writePeerSuccess forwards a successful peer response while feeding its
// reported usage into the context board. Non-stream JSON responses are buffered
// for parsing; same-protocol event streams are relayed through ForwardStream,
// which observes usage without delaying the stream.
func (g *Gateway) writePeerSuccess(ctx context.Context, w http.ResponseWriter, resp *http.Response, path, model string, tier config.Tier, requestBytes int64, peerName, requestID string) {
	defer resp.Body.Close()
	protocol := peerProtocol(path)
	observe := tier != "" && protocol != wire.SystemOne && g.contextLimits != nil && !isDiagnosticRequest(ctx)
	contentType := strings.ToLower(resp.Header.Get("Content-Type"))
	if observe && strings.HasPrefix(contentType, "text/event-stream") {
		copyPeerResponseHeaders(w, resp, peerName, requestID)
		w.WriteHeader(resp.StatusCode)
		if _, ok := w.(http.Flusher); !ok {
			_, _ = io.Copy(w, resp.Body)
			return
		}
		usage, reported, err := wire.ForwardStream(ctx, w, resp.Body, protocol, model)
		if err == nil && reported && usage.Input > 0 {
			g.contextLimits.ObserveSuccess(model, tier, usage.Input, requestBytes)
		}
		return
	}
	if observe && strings.HasPrefix(contentType, "application/json") && resp.ContentLength >= 0 && resp.ContentLength <= 64<<20 {
		body, err := io.ReadAll(io.LimitReader(resp.Body, (64<<20)+1))
		if err != nil {
			wire.WriteError(w, protocol, http.StatusBadGateway, "failed to read peer response", "upstream_error", requestID)
			return
		}
		copyPeerResponseHeaders(w, resp, peerName, requestID)
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(body)
		if usage, reported := wire.ResponseUsage(protocol, body); reported && usage.Input > 0 {
			g.contextLimits.ObserveSuccess(model, tier, usage.Input, requestBytes)
		}
		return
	}
	g.writePeerResponse(w, resp, peerName, requestID)
}

// writePeerResponse copies a peer's response through, flushing as it goes so an
// event stream reaches the client in real time.
func (g *Gateway) writePeerResponse(w http.ResponseWriter, resp *http.Response, peerName, requestID string) {
	defer resp.Body.Close()
	copyPeerResponseHeaders(w, resp, peerName, requestID)
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
	original := resp.Body
	blob, err := io.ReadAll(io.LimitReader(original, 8<<10))
	// Replay the probe bytes and keep the original closer, so draining the
	// restored body still returns the HTTP connection to the pool.
	resp.Body = struct {
		io.Reader
		io.Closer
	}{io.MultiReader(bytes.NewReader(blob), original), original}
	if err != nil {
		return false
	}
	text := strings.ToLower(string(blob))
	for _, marker := range peerSoftMarkers {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}
