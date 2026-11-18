package gateway

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"hash/fnv"
	"io"
	"log/slog"
	rand "math/rand/v2"
	"net"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"opencode2api/internal/config"
	"opencode2api/internal/httpx"
	"opencode2api/internal/relay"
)

// proxyTransport is one configured proxy route, or "direct", with its own HTTP
// clients: client for regular traffic and bun for the free-model traffic that
// goes out disguised as the Bun runtime (see Gateway.inferenceClient). pool
// marks a rotating proxy pool entry (#pool or pool+), whose health is never
// tracked. healthy is cleared by proxy-route failures and set again by
// upstream HTTP responses (see syncProxyResult) or a passing health check;
// checking claims the single health check allowed in flight.
type proxyTransport struct {
	index int
	name  string
	pool  bool
	// ipv4 marks a tcp4-forced twin of another route. It exists to give a
	// dual-stack host a second quota bucket without changing the URL host or
	// TLS SNI (the free tier counts IPv6 /64 and IPv4 separately).
	ipv4         bool
	client       *http.Client
	bun          *http.Client
	relay        *relay.Transport
	healthy      atomic.Bool
	checking     atomic.Bool
	blockedUntil atomic.Int64
	lastChecked  atomic.Int64
	// lastError keeps the reason the last health check failed so the console
	// can say why a route is unusable instead of only that it is.
	lastError atomic.Value
}

// setLastError stores the health check outcome; a pass clears the reason.
func (t *proxyTransport) setLastError(err error) {
	if err == nil {
		t.lastError.Store("")
		return
	}
	t.lastError.Store(err.Error())
}

func (t *proxyTransport) lastErrorString() string {
	value, _ := t.lastError.Load().(string)
	return value
}

// proxyTransports holds every configured proxy route, indexed by
// proxyTransport.index, shared by the key and anonymous node pools.
type proxyTransports struct {
	items []*proxyTransport
}

// blocked reports whether this exit is under the free tier's IP abuse block.
func (t *proxyTransport) blocked(now time.Time) bool {
	if t == nil {
		return false
	}
	return t.blockedUntil.Load() > now.UnixNano()
}

// markBlocked keeps this exit out of the anonymous cursor until until. The
// block is a property of the egress IP, so it is never cleared by a response
// from the same exit: only another exit can serve traffic in the meantime.
func (t *proxyTransport) markBlocked(until time.Time) {
	if t == nil {
		return
	}
	t.blockedUntil.Store(until.UnixNano())
}

// anonymousNodes gives the shared OpenCode "public" credential an independent
// cooldown per proxy. Unlike key nodes, anonymous nodes are never rebound:
// changing proxy is the failover mechanism because Zen rate-limits them by IP.
type anonymousNodes struct {
	nodes    []*anonymousNode
	next     atomic.Uint64
	cooldown time.Duration
	hasPool  bool
	// breaker stops the whole lane after repeated failures or when every
	// exit is under the free tier's IP block: the lane then pauses for
	// backoff.base, doubling every further trip (one strike per trip) up to
	// backoff.max. A success closes the breaker. probing marks the single
	// half-open attempt allowed after the window expires.
	backoff        anonymousBackoff
	breakerUntil   atomic.Int64
	breakerStrikes atomic.Int32
	consecutive    atomic.Int32
	probing        atomic.Bool
	// walls tracks a model family's short-lived rate-limit wall across
	// requests. Once two independent 429/quota answers land inside the
	// window, the lane spends one short probe instead of walking every exit
	// and then falls through to the authenticated tiers.
	wallMu sync.Mutex
	walls  map[string]*anonymousWall
}

type anonymousBackoff struct {
	threshold int32
	base      time.Duration
	max       time.Duration
}

const (
	anonymousWallThreshold    = 2
	anonymousWallWindow       = time.Minute
	anonymousWallCooldown     = time.Minute
	anonymousWallProbeTimeout = 8 * time.Second
)

// anonymousWall records rate-limit evidence for one model family. The
// upstream quota is scoped to a two-character model family and an egress
// block, so a wall seen for "de" says nothing about "gl".
type anonymousWall struct {
	hits  int
	first time.Time
	until time.Time
}

// wallFamily returns the quota family key used by the free tier and the
// egress board: the first two characters of the model ID.
func wallFamily(model string) string {
	if len(model) > 2 {
		return model[:2]
	}
	return model
}

// noteWall records one rate-limit signal for model. It reports whether the
// family is now inside a wall.
func (p *anonymousNodes) noteWall(model string, now time.Time) bool {
	if p == nil || model == "" {
		return false
	}
	p.wallMu.Lock()
	defer p.wallMu.Unlock()
	if p.walls == nil {
		p.walls = make(map[string]*anonymousWall)
	}
	key := wallFamily(model)
	wall := p.walls[key]
	if wall == nil {
		wall = &anonymousWall{}
		p.walls[key] = wall
	}
	if now.Before(wall.until) {
		return true
	}
	if wall.first.IsZero() || now.Sub(wall.first) > anonymousWallWindow {
		wall.hits = 1
		wall.first = now
		return false
	}
	wall.hits++
	if wall.hits < anonymousWallThreshold {
		return false
	}
	wall.until = now.Add(anonymousWallCooldown)
	wall.hits = 0
	wall.first = time.Time{}
	return true
}

// wallActive reports whether model's family is in its short fast-fail window.
func (p *anonymousNodes) wallActive(model string, now time.Time) bool {
	if p == nil || model == "" {
		return false
	}
	p.wallMu.Lock()
	defer p.wallMu.Unlock()
	wall := p.walls[wallFamily(model)]
	if wall == nil {
		return false
	}
	if !wall.until.IsZero() && now.Before(wall.until) {
		return true
	}
	if !wall.until.IsZero() {
		wall.until = time.Time{}
		wall.hits = 0
		wall.first = time.Time{}
	}
	return false
}

// clearWall closes the wall after a successful attempt.
func (p *anonymousNodes) clearWall(model string) {
	if p == nil || model == "" {
		return
	}
	p.wallMu.Lock()
	defer p.wallMu.Unlock()
	delete(p.walls, wallFamily(model))
}

// wallProbeTimeout caps one probe attempt while a wall is open. A zero
// configured timeout stays zero: the wall must not turn the first-event wait
// on for a deployment that disabled it.
func (p *anonymousNodes) wallProbeTimeout(model string, base time.Duration) time.Duration {
	if p == nil || base <= 0 || model == "" {
		return base
	}
	now := time.Now()
	p.wallMu.Lock()
	defer p.wallMu.Unlock()
	wall := p.walls[wallFamily(model)]
	if wall == nil || wall.until.IsZero() || !now.Before(wall.until) {
		return base
	}
	return min(base, anonymousWallProbeTimeout)
}

type anonymousNode struct {
	proxy         *proxyTransport
	failures      atomic.Uint32
	cooldownUntil atomic.Int64
	// quotaCooldown marks a cooldown that came from the upstream's Retry-After
	// (a spent quota window) rather than a transport failure. A proxy recovery
	// must not clear it: the exit IP is still out of allowance.
	quotaCooldown atomic.Bool
}

// anonymousCursor walks the anonymous nodes for one request from start;
// offset counts the positions visited so far and used marks the nodes the
// first pass or the fallback of Next has already returned.
type anonymousCursor struct {
	pool   *anonymousNodes
	start  int
	offset int
	used   []bool
	single bool
	// family and quota let an exit with a spent bucket be skipped before a
	// request burns another 429 on it. Empty family keeps the old behaviour.
	family string
	quota  *quotaBoard
}

// anonymousPool builds one anonymous node per proxy transport, or an empty
// pool when the anonymous tier is disabled.
func anonymousPool(enabled bool, transports *proxyTransports, cooldown time.Duration, backoff anonymousBackoff) *anonymousNodes {
	pool := &anonymousNodes{cooldown: cooldown, backoff: backoff}
	if !enabled || transports == nil {
		return pool
	}
	pool.nodes = make([]*anonymousNode, 0, len(transports.items))
	for _, proxy := range transports.items {
		if proxy.pool {
			pool.hasPool = true
		}
		pool.nodes = append(pool.nodes, &anonymousNode{proxy: proxy})
	}
	return pool
}

func (p *anonymousNodes) Len() int {
	if p == nil {
		return 0
	}
	return len(p.nodes)
}

// blockedExits reports whether every configured exit is currently under the
// free tier's IP block, so no exit can serve the request.
func (p *anonymousNodes) blockedExits(now time.Time) bool {
	if p == nil || len(p.nodes) == 0 {
		return false
	}
	for _, node := range p.nodes {
		if node.proxy != nil && !node.proxy.blocked(now) {
			return false
		}
	}
	return true
}

// HasPool reports whether any node uses a rotating proxy pool (#pool), which
// lets callers make more attempts than there are nodes.
func (p *anonymousNodes) HasPool() bool {
	return p != nil && p.hasPool
}

// CursorFor returns a cursor whose first node is stable for a non-empty
// affinity key, keeping a conversation on one exit; an empty affinity rotates
// the starting node round-robin.
// CursorForProxy returns a cursor whose first Next hands back the node that
// uses proxy and whose second returns nil, so a relay-pinned diagnostic
// attempts exactly one exit. A proxy outside the pool yields the same
// single-shot cursor over no node, which the caller reports as an error.
func (p *anonymousNodes) CursorForProxy(proxy *proxyTransport) anonymousCursor {
	c := anonymousCursor{pool: p, offset: len(p.nodes), single: true}
	if p == nil || proxy == nil {
		return c
	}
	for i, node := range p.nodes {
		if node.proxy == proxy {
			c.start = i
			return c
		}
	}
	return c
}

func (p *anonymousNodes) CursorFor(affinity string) anonymousCursor {
	if p == nil || len(p.nodes) == 0 {
		return anonymousCursor{pool: p}
	}
	start := 0
	if affinity == "" {
		start = int((p.next.Add(1) - 1) % uint64(len(p.nodes)))
	} else {
		hash := fnv.New64a()
		_, _ = hash.Write([]byte(affinity))
		start = int(hash.Sum64() % uint64(len(p.nodes)))
	}
	return anonymousCursor{pool: p, start: start}
}

// CursorForOffset is CursorFor shifted by offset nodes. A request that has
// already spent offset attempts starts on the next node instead of repeating
// the exit that just failed it.
func (p *anonymousNodes) CursorForOffset(affinity string, offset int) anonymousCursor {
	cursor := p.CursorFor(affinity)
	if p == nil || len(p.nodes) == 0 || offset <= 0 {
		return cursor
	}
	cursor.start = (cursor.start + offset) % len(p.nodes)
	return cursor
}

// AllowAttempt reports whether an anonymous attempt may run. An open breaker
// refuses every attempt. Once its window expires, exactly one caller wins the
// half-open probe, so the lane is not flooded before it is known to work
// again; every winner must release the probe through NoteSuccess, NoteFailure,
// NoteNeutral or ReleaseProbe.
func (p *anonymousNodes) AllowAttempt() bool {
	if p == nil || p.backoff.threshold < 1 {
		return true
	}
	until := p.breakerUntil.Load()
	if until == 0 {
		return true
	}
	if time.Now().UnixNano() < until {
		return false
	}
	return p.probing.CompareAndSwap(false, true)
}

// Open reports whether the breaker currently holds the lane shut. A half-open
// breaker whose window has expired reports false: the lane is being probed.
func (p *anonymousNodes) Open() bool {
	if p == nil {
		return false
	}
	until := p.breakerUntil.Load()
	return until != 0 && time.Now().UnixNano() < until
}

// ReleaseProbe returns an unused half-open probe slot. It is safe to call
// after any outcome has already closed or reopened the breaker.
func (p *anonymousNodes) ReleaseProbe() {
	if p == nil {
		return
	}
	p.probing.Store(false)
}

// NoteSuccess closes the breaker and forgets the strikes.
func (p *anonymousNodes) NoteSuccess() {
	if p == nil {
		return
	}
	p.consecutive.Store(0)
	p.breakerStrikes.Store(0)
	p.breakerUntil.Store(0)
	p.probing.Store(false)
}

// NoteNeutral releases a half-open probe and closes the breaker without
// counting a strike. It is used when the upstream answered the lane but
// rejected the request itself (for example a context-length 400): the lane is
// reachable, so it should not stay paused, and the request is not evidence of
// an upstream outage.
func (p *anonymousNodes) NoteNeutral() {
	if p == nil {
		return
	}
	p.consecutive.Store(0)
	p.breakerUntil.Store(0)
	p.probing.Store(false)
}

// NoteFailure records one failed attempt. It reports the pause window when the
// consecutive-failure threshold trips the breaker. A failure during the
// half-open probe reopens the breaker immediately with the next, longer window
// instead of waiting for another threshold's worth of traffic.
func (p *anonymousNodes) NoteFailure() (time.Duration, bool) {
	if p == nil || p.backoff.threshold < 1 {
		return 0, false
	}
	now := time.Now().UnixNano()
	until := p.breakerUntil.Load()
	if p.probing.Load() || (until != 0 && now >= until) {
		return p.trip()
	}
	if p.consecutive.Add(1) < p.backoff.threshold {
		return 0, false
	}
	return p.trip()
}

// ForceOpen trips the breaker immediately. The anonymous walk calls it when
// every exit is under the free tier's IP block: another request cannot reach a
// working exit, and probing would only extend the penalty.
func (p *anonymousNodes) ForceOpen() (time.Duration, bool) {
	if p == nil || p.backoff.threshold < 1 {
		return 0, false
	}
	return p.trip()
}

// trip opens the breaker for the next backoff window and returns it.
func (p *anonymousNodes) trip() (time.Duration, bool) {
	p.consecutive.Store(0)
	strike := p.breakerStrikes.Add(1)
	pause := p.backoff.base << min(strike-1, 30)
	if pause > p.backoff.max || pause <= 0 {
		pause = p.backoff.max
	}
	p.breakerUntil.Store(time.Now().Add(pause).UnixNano())
	p.probing.Store(false)
	return pause, true
}

// Next returns the next node to try. Its first pass visits every node once
// from the cursor's start and returns each healthy, non-cooling static proxy
// and each rotating proxy-pool transport (#pool) it reaches, so healthy nodes
// are tried first. When the pool contains #pool transports, Next then keeps
// cycling through only those, so retry.max_attempts can draw a fresh upstream
// connection (and new exit IP) on every attempt. Otherwise it falls back to
// the nodes this cursor has not returned yet, healthy ones first and, among
// those, the one whose cooldown expires earliest, and returns nil only once
// every node has been returned.
func (c *anonymousCursor) Next() *anonymousNode {
	if c.pool == nil || len(c.pool.nodes) == 0 {
		return nil
	}
	n := len(c.pool.nodes)
	if c.used == nil {
		c.used = make([]bool, n)
	}
	if c.single {
		index := c.start
		c.single = false
		c.offset = n
		if index < 0 || index >= n {
			return nil
		}
		return c.pool.nodes[index]
	}
	now := time.Now().UnixNano()
	isBlocked := func(node *anonymousNode) bool {
		if node.proxy != nil && node.proxy.blockedUntil.Load() > now {
			return true
		}
		if c.family != "" && c.quota != nil && node.proxy != nil {
			// A static exit whose bucket for this model family is already
			// spent must not receive a leg: the answer is a known 429. Pool
			// transports draw a fresh egress each attempt, so their shared
			// label is not a safe quota key.
			label := node.proxy.name
			if !strings.Contains(label, "#pool") && c.quota.spentFor(label, c.family, time.Now()) {
				return true
			}
		}
		return false
	}
	for c.offset < n {
		idx := (c.start + c.offset) % n
		c.offset++
		node := c.pool.nodes[idx]
		if isBlocked(node) {
			// The free tier flagged this egress IP: the answer would be the
			// same on every lane, so neither a fresh leg nor the cooldown
			// fallback below may return it.
			c.used[idx] = true
			continue
		}
		if node.proxy.pool || (!c.used[idx] && node.proxy.healthy.Load() && node.cooldownUntil.Load() <= now) {
			c.used[idx] = true
			return node
		}
	}
	if c.pool.hasPool {
		for range n {
			idx := (c.start + c.offset) % n
			c.offset++
			node := c.pool.nodes[idx]
			if node.proxy.pool && !isBlocked(node) {
				return node
			}
		}
	}
	choice := -1
	var earliest int64
	choiceHealthy := false
	for step := range n {
		idx := (c.start + step) % n
		if c.used[idx] {
			continue
		}
		node := c.pool.nodes[idx]
		if isBlocked(node) {
			continue
		}
		healthy := node.proxy.healthy.Load()
		until := node.cooldownUntil.Load()
		if choice < 0 || (healthy && !choiceHealthy) || (healthy == choiceHealthy && until < earliest) {
			choice, earliest, choiceHealthy = idx, until, healthy
		}
	}
	if choice >= 0 {
		c.used[choice] = true
		return c.pool.nodes[choice]
	}
	return nil
}

func (p *anonymousNodes) MarkSuccess(node *anonymousNode) {
	if node == nil {
		return
	}
	node.failures.Store(0)
	node.cooldownUntil.Store(0)
	node.quotaCooldown.Store(false)
}

// RestoreProxy clears cooldowns for anonymous nodes bound to a recovered
// proxy, mirroring upstreamNodes.RestoreProxy for key nodes. Without this, an
// outage-era cooldown survives the proxy's recovery and the anonymous lane
// stays skipped even though the route is healthy again. It returns how many
// nodes it cleared.
func (p *anonymousNodes) RestoreProxy(proxy *proxyTransport) int {
	if p == nil || proxy == nil {
		return 0
	}
	cleared := 0
	for _, node := range p.nodes {
		if node.proxy != proxy {
			continue
		}
		if node.quotaCooldown.Load() {
			// The upstream said this exit has no allowance left; recovering
			// the socket does not refill it.
			continue
		}
		if node.cooldownUntil.Load() == 0 && node.failures.Load() == 0 {
			continue
		}
		node.failures.Store(0)
		node.cooldownUntil.Store(0)
		cleared++
	}
	return cleared
}

// maxAnonymousQuotaCooldown bounds an upstream Retry-After on the anonymous
// lane. The free tier's window is the UTC calendar day, so a hint never
// legitimately exceeds one day; the bound only guards against a malformed
// header pinning an exit forever. Unlike the exponential backoff, the hint is
// honoured in full: a spent /64 stays out of rotation until its window resets
// instead of being retried every few minutes.
const maxAnonymousQuotaCooldown = 24 * time.Hour

// MarkFailure cools node down after an error or a 401, 403, 429 or 5xx
// response; any other response leaves it untouched, and #pool nodes are never
// cooled. The cooldown doubles with each consecutive failure, from the
// base cooldown up to 8x. An upstream Retry-After is honoured in full (bounded
// by one day) and flagged as a quota cooldown, because a spent quota window
// does not reopen when the proxy recovers.
// MarkQuotaSpent keeps an exit out of rotation until the free tier's counting
// window resets after a 200 quota document. A 2xx response is not evidence
// that the exit is broken, so MarkFailure would leave it in rotation; the
// spent bucket is what makes it unusable. A Retry-After header wins when the
// upstream supplies one.
func (p *anonymousNodes) MarkQuotaSpent(node *anonymousNode, resp *http.Response) {
	if node == nil || (node.proxy != nil && node.proxy.pool) {
		return
	}
	delay := quotaResetAfter(time.Now())
	if resp != nil {
		if retryAfter := parseRetryAfter(resp.Header.Get("Retry-After")); retryAfter > 0 {
			delay = min(retryAfter, maxAnonymousQuotaCooldown)
		}
	}
	node.cooldownUntil.Store(time.Now().Add(delay).UnixNano())
	node.quotaCooldown.Store(true)
}

func (p *anonymousNodes) MarkFailure(node *anonymousNode, resp *http.Response, err error) {
	if node == nil || (node.proxy != nil && node.proxy.pool) {
		return
	}
	if err == nil && resp != nil && resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode < 500 {
		return
	}
	failures := node.failures.Add(1)
	delay := p.cooldown * time.Duration(1<<min(failures-1, 3))
	if resp != nil {
		if retryAfter := parseRetryAfter(resp.Header.Get("Retry-After")); retryAfter > delay {
			delay = min(retryAfter, maxAnonymousQuotaCooldown)
			node.quotaCooldown.Store(true)
		}
	}
	node.cooldownUntil.Store(time.Now().Add(delay).UnixNano())
}

func (p *proxyTransports) hasHealthy() bool {
	for _, proxy := range p.items {
		if proxy.healthy.Load() {
			return true
		}
	}
	return false
}

func (p *proxyTransports) healthCounts() (total, healthy int) {
	if p == nil {
		return 0, 0
	}
	for _, proxy := range p.items {
		if proxy.healthy.Load() {
			healthy++
		}
	}
	return len(p.items), healthy
}

// transportPool builds the HTTP clients of every configured proxy entry, each
// entry starting healthy: a regular client and a Bun-disguised one
// (httpx.BunTransport) sharing the proxy, connect timeout, idle settings and
// responseHeaderTimeout as their header wait. A #pool entry gets HTTP/1.1
// transports without keep-alives, so every attempt opens a new connection
// through the pool; "direct" dials the upstream without any proxy, ignoring
// the proxy environment variables.
func transportPool(proxies []string, cfg config.PerformanceConfig, egress config.EgressConfig, responseHeaderTimeout time.Duration, logger *slog.Logger) (*proxyTransports, error) {
	p := &proxyTransports{items: make([]*proxyTransport, 0, len(proxies))}
	// A tcp4-only dialer is safe only for a direct route: for a proxied route
	// BunTransport's DialContext would replace the proxy CONNECT entirely.
	tcp4 := func(ctx context.Context, _, addr string) (net.Conn, error) {
		return (&net.Dialer{
			Timeout:   time.Duration(cfg.ConnectTimeoutSeconds) * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext(ctx, "tcp4", addr)
	}
	for _, raw := range proxies {
		clean, isPool := config.ParseProxyEntry(raw)
		if endpoint, token, ok := relayEntry(clean); ok {
			transport, err := relay.New(relay.Options{
				Endpoint: endpoint,
				Token:    token,
				Timeout:  responseHeaderTimeout,
				Logger:   logger,
			})
			if err != nil {
				return nil, fmt.Errorf("relay %s: %w", config.RedactURL(raw), err)
			}
			client := &http.Client{Transport: transport}
			proxy := &proxyTransport{index: len(p.items), name: raw, pool: true, client: client, bun: client, relay: transport}
			proxy.healthy.Store(true)
			p.items = append(p.items, proxy)
			continue
		}
		transport := http.DefaultTransport.(*http.Transport).Clone()
		transport.MaxIdleConns = cfg.MaxIdleConns
		transport.MaxIdleConnsPerHost = cfg.MaxIdleConnsPerHost
		transport.MaxConnsPerHost = cfg.MaxConnsPerHost
		transport.IdleConnTimeout = time.Duration(cfg.IdleConnTimeoutSeconds) * time.Second
		transport.ResponseHeaderTimeout = responseHeaderTimeout
		transport.ForceAttemptHTTP2 = !isPool
		transport.DisableKeepAlives = isPool
		if isPool {
			// Cloning DefaultTransport after its first dial inherits the
			// already-configured HTTP/2 ALPN and TLSNextProto map, which ignores
			// DisableKeepAlives and req.Close and multiplexes every request over
			// one CONNECT tunnel. An explicit empty TLSNextProto map and HTTP/1.1
			// ALPN force a brand-new TCP + CONNECT tunnel per attempt.
			transport.TLSNextProto = make(map[string]func(authority string, c *tls.Conn) http.RoundTripper)
			if transport.TLSClientConfig == nil {
				transport.TLSClientConfig = &tls.Config{NextProtos: []string{"http/1.1"}}
			} else {
				tlsCfg := transport.TLSClientConfig.Clone()
				tlsCfg.NextProtos = []string{"http/1.1"}
				transport.TLSClientConfig = tlsCfg
			}
		}
		transport.DialContext = (&net.Dialer{
			Timeout:   time.Duration(cfg.ConnectTimeoutSeconds) * time.Second,
			KeepAlive: 30 * time.Second,
		}).DialContext
		var proxyURL *url.URL
		if clean == "direct" {
			transport.Proxy = nil
		} else {
			u, err := url.Parse(clean)
			if err != nil {
				return nil, fmt.Errorf("parse proxy %s: %w", config.RedactURL(raw), err)
			}
			transport.Proxy = http.ProxyURL(u)
			proxyURL = u
		}
		bun, err := httpx.NewBunTransport(httpx.BunOptions{
			Proxy:                 proxyURL,
			ConnectTimeout:        time.Duration(cfg.ConnectTimeoutSeconds) * time.Second,
			ResponseHeaderTimeout: responseHeaderTimeout,
			IdleConnTimeout:       transport.IdleConnTimeout,
			MaxIdleConnsPerHost:   cfg.MaxIdleConnsPerHost,
			DisableKeepAlives:     isPool,
		})
		if err != nil {
			return nil, fmt.Errorf("proxy %s: %w", config.RedactURL(raw), err)
		}
		proxy := &proxyTransport{index: len(p.items), name: raw, pool: isPool, client: &http.Client{Transport: transport}, bun: &http.Client{Transport: bun}}
		proxy.healthy.Store(true)
		p.items = append(p.items, proxy)

		if egress.IPv4Twins && clean == "direct" {
			// Direct twin: force tcp4 only. The URL still carries the upstream
			// hostname, so Host and Bun TLS SNI stay correct; forcing IPv4 via
			// the dialer is what creates a separate quota bucket.
			twinTransport := transport.Clone()
			twinTransport.Proxy = nil
			twinTransport.DialContext = tcp4
			twinBun, err := httpx.NewBunTransport(httpx.BunOptions{
				ConnectTimeout:        time.Duration(cfg.ConnectTimeoutSeconds) * time.Second,
				ResponseHeaderTimeout: responseHeaderTimeout,
				IdleConnTimeout:       transport.IdleConnTimeout,
				MaxIdleConnsPerHost:   cfg.MaxIdleConnsPerHost,
				DisableKeepAlives:     isPool,
				DialContext:           tcp4,
			})
			if err != nil {
				return nil, fmt.Errorf("proxy %s ipv4 twin: %w", config.RedactURL(raw), err)
			}
			twin := &proxyTransport{
				index: len(p.items), name: config.RedactURL(raw) + "-v4", ipv4: true,
				client: &http.Client{Transport: twinTransport}, bun: &http.Client{Transport: twinBun},
			}
			twin.healthy.Store(true)
			p.items = append(p.items, twin)
		} else if egress.IPv4Twins && clean != "direct" && logger != nil {
			logger.Warn("ipv4_twins applies only to the direct route; skipping proxied route",
				"component", "egress", "event", "ipv4_twin_skipped", "proxy", config.RedactURL(raw))
		}
	}
	if egress.IPv4First {
		// Keep relative order inside both groups. Relays/Tor stay at the end.
		v4, rest := make([]*proxyTransport, 0, len(p.items)), make([]*proxyTransport, 0, len(p.items))
		for _, item := range p.items {
			if item.ipv4 {
				v4 = append(v4, item)
			} else {
				rest = append(rest, item)
			}
		}
		p.items = append(v4, rest...)
		for index, item := range p.items {
			item.index = index
		}
	}
	return p, nil
}

// relayEntry reports whether a clean proxy entry names a relay route
// ("relay://<token>@<host>[/path]") and returns its endpoint and token. A relay
// route is not a TCP proxy: the transport forwards each request with the
// relay's own framing, so the free tier's TLS check is answered by the relay
// runtime and the exit address belongs to the relay's function instance.
func relayEntry(clean string) (*url.URL, string, bool) {
	if !strings.HasPrefix(strings.ToLower(clean), "relay://") && !strings.HasPrefix(strings.ToLower(clean), "relay+http://") {
		return nil, "", false
	}
	u, err := url.Parse(clean)
	if err != nil || u.Host == "" {
		return nil, "", false
	}
	token := ""
	if u.User != nil {
		token = u.User.Username()
		if password, ok := u.User.Password(); ok && token == "" {
			token = password
		}
	}
	endpoint := *u
	endpoint.User = nil
	if endpoint.Path == "" {
		endpoint.Path = "/"
	}
	return &endpoint, token, true
}

// proxyHealthResult is the outcome of one health check. failed is set only
// for a proxy-route failure (see isProxyFailure), and wasHealthy is the
// healthy flag as it stood before the check updated it.
type proxyHealthResult struct {
	proxy      *proxyTransport
	err        error
	failed     bool
	wasHealthy bool
}

// CheckHealth concurrently rechecks only proxies already marked unhealthy and
// waits for every check to finish. Healthy proxies, #pool transports and
// proxies whose check is already in flight are skipped. Any HTTP response
// from the test URL proves that the route is reachable and restores the
// proxy; any error keeps it unhealthy.
func (p *proxyTransports) CheckHealth(ctx context.Context, target string, timeout time.Duration) []proxyHealthResult {
	return p.check(ctx, target, timeout, false)
}

// CheckAll concurrently probes every non-pool proxy, including proxies still
// marked healthy. A healthy flag is only an assumption until the route is
// exercised: an external tunnel can die while its local SOCKS listener stays
// open. Checking all routes periodically is therefore required to detect a
// silent outage instead of waiting for real traffic to fail.
func (p *proxyTransports) CheckAll(ctx context.Context, target string, timeout time.Duration) []proxyHealthResult {
	return p.check(ctx, target, timeout, true)
}

func (p *proxyTransports) check(ctx context.Context, target string, timeout time.Duration, includeHealthy bool) []proxyHealthResult {
	results := make(chan proxyHealthResult, len(p.items))
	checks := 0
	for _, proxy := range p.items {
		if proxy.pool || !proxy.checking.CompareAndSwap(false, true) {
			continue
		}
		// A real request may have restored the proxy between the health read
		// and claiming this check. A full sweep deliberately ignores that flag.
		if !includeHealthy && proxy.healthy.Load() {
			proxy.checking.Store(false)
			continue
		}
		checks++
		go func() {
			results <- p.checkClaimedProxy(ctx, proxy, target, timeout)
		}()
	}
	out := make([]proxyHealthResult, 0, checks)
	for range checks {
		out = append(out, <-results)
	}
	return out
}

// checkClaimedProxy sends one GET to target through proxy after the caller
// has acquired checking, and releases that claim when done. Any HTTP response
// marks the proxy healthy, a proxy-route failure marks it unhealthy, and any
// other error leaves the flag as it is.
func (p *proxyTransports) checkClaimedProxy(ctx context.Context, proxy *proxyTransport, target string, timeout time.Duration) proxyHealthResult {
	defer func() {
		proxy.lastChecked.Store(time.Now().UnixNano())
		proxy.checking.Store(false)
	}()
	checkCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(checkCtx, http.MethodGet, target, nil)
	if err == nil {
		req.Header.Set("User-Agent", httpx.V2UserAgent())
		// Probe with the same client free-model traffic uses. The Bun client
		// reproduces the official OpenCode client's TLS fingerprint, so a probe
		// through it measures the path it judges instead of a plain Go TLS
		// handshake that some origins treat differently.
		probeClient := proxy.client
		if proxy.bun != nil {
			probeClient = proxy.bun
		}
		resp, requestErr := probeClient.Do(req)
		err = requestErr
		if resp != nil {
			_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 64<<10))
			_ = resp.Body.Close()
		}
	}
	result := proxyHealthResult{proxy: proxy, err: err, wasHealthy: proxy.healthy.Load()}
	if err == nil {
		result.wasHealthy = proxy.healthy.Swap(true)
	} else if !errors.Is(err, context.Canceled) {
		// A probe is a plain HTTP GET issued *through* the proxy. If it did not
		// return a response, this proxy cannot currently serve traffic — the
		// CONNECT was refused, the tunnel broke, the connection was reset, or it
		// timed out. Every such outcome must count as a failure.
		//
		// isProxyFailure() only recognized timeouts and ECONNREFUSED, so a proxy
		// whose CONNECT is rejected for any other reason (a provider refusing
		// with its own error, e.g. an exhausted-bandwidth response) was reported
		// as "inconclusive". It then stayed healthy forever: never taken out of
		// rotation, never re-bound for key traffic. Only cancellation is exempt,
		// because that says the *caller* gave up, not that the proxy is down.
		result.failed = true
		result.wasHealthy = proxy.healthy.Swap(false)
	}
	return result
}

// isProxyFailure deliberately recognizes only failures that say the proxy
// route is unavailable: a timeout or a connection refusal. HTTP responses,
// timeouts in the first-event wait (the upstream already answered with
// headers) and unrelated transport/protocol errors must not evict a proxy.
func isProxyFailure(err error) bool {
	if errors.Is(err, errFirstEventTimeout) || upstreamAccepted(err) {
		return false // The upstream accepted HTTP; slow generation is not a broken proxy.
	}
	if err == nil {
		return false
	}
	if errors.Is(err, context.DeadlineExceeded) || errors.Is(err, syscall.ECONNREFUSED) {
		return true
	}
	var timeout interface{ Timeout() bool }
	return errors.As(err, &timeout) && timeout.Timeout()
}

// upstreamNode keeps a key stable while allowing its proxy binding to change
// atomically when a proxy becomes unavailable.
type upstreamNode struct {
	key            string
	index          int
	preferredProxy int
	bound          bool
	proxyIndex     atomic.Int64
	failures       atomic.Uint32
	cooldownUntil  atomic.Int64
	// credUntil is the credential quarantine horizon: an upstream 401/403
	// rejected the key itself, so it stays out of rotation until a probe or a
	// real request sees it accepted again. It is separate from cooldownUntil,
	// which a 429 or a 5xx also sets, so the two failure classes never
	// clear each other.
	credUntil atomic.Int64
	// quotaUntil is the free-tier quota wall horizon from a 429's Retry-After
	// (hours). It is a semantic superset of cooldownUntil: transport failures
	// and proxy recovery may clear cooldownUntil, but a spent quota window does
	// not reopen because a proxy came back or a listing succeeded.
	quotaUntil atomic.Int64
}

// upstreamNodes is one tier's key pool. bindingsMu serializes rebinding and
// guards bindingCount, the number of keys bound to each proxy index; a node's
// proxyIndex is atomic, so reading a binding needs no lock.
type upstreamNodes struct {
	nodes        []*upstreamNode
	transports   *proxyTransports
	next         atomic.Uint64
	cooldown     time.Duration
	quarantine   time.Duration
	bindingsMu   sync.Mutex
	bindingCount []int
}

// nodePool distributes keys over proxies. When explicit proxy bindings are
// configured, bound keys are permanently pinned to their specified proxy and
// fail closed if that proxy fails. Unbound keys are distributed in round-robin
// order and can fail over to other proxies.
func nodePool(keys []string, transports *proxyTransports, cooldown time.Duration, bindings map[string]string) (*upstreamNodes, error) {
	if transports == nil || len(transports.items) == 0 {
		return nil, fmt.Errorf("at least one proxy transport is required")
	}
	pool := &upstreamNodes{
		nodes:        make([]*upstreamNode, 0, len(keys)),
		transports:   transports,
		cooldown:     cooldown,
		bindingCount: make([]int, len(transports.items)),
	}
	if len(keys) == 0 {
		return pool, nil
	}
	proxyMap := make(map[string]int, len(transports.items)*2)
	for i, t := range transports.items {
		proxyMap[t.name] = i
		clean, _ := config.ParseProxyEntry(t.name)
		proxyMap[clean] = i
	}

	for i, key := range keys {
		fp := config.Fingerprint(key)
		targetProxy, hasBinding := bindings[fp]
		if !hasBinding {
			targetProxy, hasBinding = bindings[key]
		}

		var proxyIndex int
		var bound bool
		if hasBinding {
			idx, ok := proxyMap[targetProxy]
			if !ok {
				cleanTarget, _ := config.ParseProxyEntry(targetProxy)
				idx, ok = proxyMap[cleanTarget]
			}
			if !ok {
				return nil, fmt.Errorf("bound proxy %q for key %s not found in transports", targetProxy, config.KeyDisplayID(key))
			}
			proxyIndex = idx
			bound = true
		} else {
			proxyIndex = i % len(transports.items)
			bound = false
		}

		node := &upstreamNode{
			key:            key,
			index:          i,
			preferredProxy: proxyIndex,
			bound:          bound,
		}
		node.proxyIndex.Store(int64(proxyIndex))
		pool.nodes = append(pool.nodes, node)
		pool.bindingCount[proxyIndex]++
	}
	return pool, nil
}

// RestoreProxy moves keys that were originally assigned to a recovered proxy
// back to it, clearing their failure counts and cooldowns, and returns how
// many keys moved. Proxies without an original key simply become healthy
// failover candidates again.
func (p *upstreamNodes) RestoreProxy(recoveredProxy int) int {
	if p == nil || p.transports == nil || recoveredProxy < 0 || recoveredProxy >= len(p.transports.items) {
		return 0
	}
	p.bindingsMu.Lock()
	defer p.bindingsMu.Unlock()

	moved := 0
	for _, node := range p.nodes {
		current := int(node.proxyIndex.Load())
		if node.preferredProxy != recoveredProxy {
			continue
		}
		if node.bound {
			// Clear failures and cooldown so bound node immediately resumes traffic on recovered proxy.
			node.failures.Store(0)
			node.cooldownUntil.Store(0)
			continue
		}
		if current == recoveredProxy {
			continue
		}
		if current >= 0 && current < len(p.bindingCount) {
			p.bindingCount[current]--
		}
		p.bindingCount[recoveredProxy]++
		node.proxyIndex.Store(int64(recoveredProxy))
		node.failures.Store(0)
		node.cooldownUntil.Store(0)
		moved++
	}
	return moved
}

// MarkKeyQuotaSpent records that an upstream rejected this credential with a
// 429. It sets only quotaUntil: unlike a 401/403 credential quarantine, a
// quota wall opens at the upstream window, and unlike a transport cooldown a
// recovered proxy or a successful listing must not clear it.
func (p *upstreamNodes) MarkKeyQuotaSpent(node *upstreamNode, retryAfter time.Duration) {
	if p == nil || node == nil {
		return
	}
	now := time.Now()
	delay := quotaResetAfter(now)
	if retryAfter > 0 {
		delay = min(retryAfter, maxAnonymousQuotaCooldown)
	}
	until := now.Add(delay).UnixNano()
	if cred := node.credUntil.Load(); cred > until {
		until = cred
	}
	node.quotaUntil.Store(until)
}

func (p *upstreamNodes) Len() int { return len(p.nodes) }

// NodeByID resolves a node by its stable fingerprint. Callers only ever hold
// the fingerprint, never the key value, so the lookup cannot leak a secret.
func (p *upstreamNodes) NodeByID(id string) *upstreamNode {
	if p == nil || id == "" {
		return nil
	}
	for _, node := range p.nodes {
		if config.Fingerprint(node.key) == id {
			return node
		}
	}
	return nil
}

func (p *upstreamNodes) Proxy(node *upstreamNode) *proxyTransport {
	if p == nil || node == nil || p.transports == nil {
		return nil
	}
	index := int(node.proxyIndex.Load())
	if index < 0 || index >= len(p.transports.items) {
		return nil
	}
	return p.transports.items[index]
}

// RebindProxy moves every key currently using failedProxy to the least-loaded
// healthy proxy. Empty proxies are selected in configuration order; otherwise
// one of the least-loaded proxies is chosen at random. If every alternative is
// currently unhealthy, it still moves the key to the least-loaded alternative.
// Moved keys start with a clean failure count and cooldown. It returns how
// many keys moved; with a single proxy there is nowhere to move them.
func (p *upstreamNodes) RebindProxy(failedProxy int) int {
	if p == nil || p.transports == nil || len(p.transports.items) < 2 || failedProxy < 0 || failedProxy >= len(p.transports.items) {
		return 0
	}
	p.bindingsMu.Lock()
	defer p.bindingsMu.Unlock()

	moved := 0
	for _, node := range p.nodes {
		if int(node.proxyIndex.Load()) != failedProxy {
			continue
		}
		if node.bound {
			// Fail-closed: bound nodes never migrate to another proxy or direct route.
			continue
		}
		target := p.replacementLocked(failedProxy, true)
		if target < 0 {
			target = p.replacementLocked(failedProxy, false)
		}
		if target < 0 {
			continue
		}
		p.bindingCount[failedProxy]--
		p.bindingCount[target]++
		node.proxyIndex.Store(int64(target))
		node.failures.Store(0)
		node.cooldownUntil.Store(0)
		moved++
	}
	return moved
}

// replacementLocked picks the proxy a key leaving failedProxy moves to: the
// first proxy in configuration order with no keys bound, otherwise a random
// one of the least-loaded, considering only healthy proxies when healthyOnly
// is set. It returns -1 when no proxy qualifies. The caller must hold
// bindingsMu.
func (p *upstreamNodes) replacementLocked(failedProxy int, healthyOnly bool) int {
	minimum := int(^uint(0) >> 1)
	candidates := make([]int, 0, len(p.transports.items)-1)
	for i := range p.transports.items {
		if i == failedProxy || healthyOnly && !p.transports.items[i].healthy.Load() {
			continue
		}
		count := p.bindingCount[i]
		if count == 0 {
			return i
		}
		if count < minimum {
			minimum = count
			candidates = candidates[:0]
			candidates = append(candidates, i)
		} else if count == minimum {
			candidates = append(candidates, i)
		}
	}
	if len(candidates) == 0 {
		return -1
	}
	return candidates[rand.IntN(len(candidates))]
}

type nodeCursor struct {
	pool *upstreamNodes
	next int
}

// Cursor reserves a different starting node for each concurrent request.
// Selection is delayed until Next, so a node marked failed by the preceding
// attempt is immediately skipped. Both Cursor and Next allocate no memory.
func (p *upstreamNodes) Cursor() nodeCursor {
	if len(p.nodes) == 0 {
		return nodeCursor{pool: p}
	}
	return nodeCursor{pool: p, next: int((p.next.Add(1) - 1) % uint64(len(p.nodes)))}
}

// CursorFor returns a cursor whose first choice is stable for the supplied
// affinity key. This keeps every turn in a conversation on the same upstream
// key while retaining Next's cooldown-aware failover behavior. Empty affinity
// keys keep the round-robin behavior used by background tasks.
func (p *upstreamNodes) CursorFor(affinity string) nodeCursor {
	if affinity == "" || len(p.nodes) == 0 {
		return p.Cursor()
	}
	hash := fnv.New64a()
	_, _ = hash.Write([]byte(affinity))
	return nodeCursor{pool: p, next: int(hash.Sum64() % uint64(len(p.nodes)))}
}

// Next chooses an uncooled key on a healthy proxy first, then the earliest
// cooling key on any healthy proxy. Only when no healthy proxy has a key does
// it fall back to an uncooled key on an unhealthy proxy, then the earliest
// cooldown overall. It moves the cursor past the selected node. Unlike
// anonymousCursor.Next it never returns nil for a non-empty pool and may revisit
// a node, so callers bound attempts themselves.
func (c *nodeCursor) Next() *upstreamNode {
	if c.pool == nil || len(c.pool.nodes) == 0 {
		return nil
	}
	now := time.Now().UnixNano()
	ready, healthyReady := -1, -1
	var earliest, healthyEarliest int64
	choice, healthyChoice := -1, -1
	for offset := range len(c.pool.nodes) {
		i := (c.next + offset) % len(c.pool.nodes)
		node := c.pool.nodes[i]
		if node.credUntil.Load() > now {
			// A quarantined credential is not a candidate at any priority: the
			// upstream already said the key itself is rejected, so burning an
			// attempt on it only repeats the 401.
			continue
		}
		proxy := c.pool.Proxy(node)
		healthy := proxy != nil && proxy.healthy.Load()
		until := node.cooldownUntil.Load()
		// A quota wall outlasts the short transport cooldown. Keep walled keys
		// out of the ready/earliest buckets; when the whole pool is walled the
		// fallback below still returns the earliest-reopening key.
		if walled := node.quotaUntil.Load(); walled > until {
			until = walled
		}
		if until <= now {
			if healthy {
				healthyReady = i
				break
			}
			if ready < 0 {
				ready = i
			}
			continue
		}
		if healthy && (healthyChoice < 0 || until < healthyEarliest) {
			healthyChoice, healthyEarliest = i, until
		}
		if choice < 0 || until < earliest {
			choice, earliest = i, until
		}
	}
	if healthyReady >= 0 {
		choice = healthyReady
	} else if healthyChoice >= 0 {
		choice = healthyChoice
	} else if ready >= 0 {
		choice = ready
	} else if choice < 0 {
		return nil
	}
	c.next = (choice + 1) % len(c.pool.nodes)
	return c.pool.nodes[choice]
}

func (p *upstreamNodes) MarkSuccess(node *upstreamNode) {
	node.failures.Store(0)
	node.cooldownUntil.Store(0)
	node.credUntil.Store(0)
	node.quotaUntil.Store(0)
}

// MarkListingSuccess records that a model listing succeeded through the key.
// A listing is not an inference: it must not reopen a live quota wall, which
// would put a spent key back in front of the next real request.
func (p *upstreamNodes) MarkListingSuccess(node *upstreamNode) {
	node.failures.Store(0)
	node.cooldownUntil.Store(0)
}

// MarkFailure cools node down after an error or a 401, 403, 429 or 5xx
// response; any other response leaves it untouched. The cooldown doubles with
// each consecutive failure, from the base cooldown up to 8x, and an upstream
// Retry-After longer than that is honored in full.
//
// A 401 or 403 is handled differently: the upstream rejected the key itself,
// not the request and not the exit, so a two-minute cooldown would put the
// same dead key back in front of the next caller (and, behind a fronting
// gateway that auto-bans on a forwarded 401, can take the whole channel dark
// while healthy keys sit one rotation away). With quarantine configured the
// key stays out for the whole horizon; failures reset so it returns clean, and
// the cooldown is pinned to that horizon because MarkSuccess, not the
// cooldown, is what clears it.
func (p *upstreamNodes) MarkFailure(node *upstreamNode, resp *http.Response, err error) {
	if err == nil && resp != nil && resp.StatusCode != http.StatusUnauthorized && resp.StatusCode != http.StatusForbidden && resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode < 500 {
		return
	}
	now := time.Now()
	if resp != nil && (resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) && p.quarantine > 0 {
		until := now.Add(p.quarantine).UnixNano()
		node.credUntil.Store(until)
		node.failures.Store(0)
		node.cooldownUntil.Store(until)
		return
	}
	failures := node.failures.Add(1)
	multiplier := time.Duration(1 << min(failures-1, 3))
	delay := p.cooldown * multiplier
	if resp != nil {
		if retryAfter := parseRetryAfter(resp.Header.Get("Retry-After")); retryAfter > delay {
			delay = retryAfter
		}
	}
	until := now.Add(delay)
	if cred := node.credUntil.Load(); cred > until.UnixNano() {
		// A live credential quarantine outlasts any quota cooldown; letting a
		// short 429 shorten it would put a rejected key back in rotation.
		until = time.Unix(0, cred)
	}
	node.cooldownUntil.Store(until.UnixNano())
	if resp != nil && resp.StatusCode == http.StatusTooManyRequests {
		// The free tier's Retry-After runs for hours. Remember it separately so
		// transport recovery or a successful listing cannot reopen the wall.
		node.quotaUntil.Store(until.UnixNano())
	}
}

// QuarantinedUntil reports the credential quarantine horizon, zero when the
// key is not quarantined.
func (n *upstreamNode) QuarantinedUntil() time.Time {
	if n == nil {
		return time.Time{}
	}
	if nanos := n.credUntil.Load(); nanos > 0 {
		return time.Unix(0, nanos)
	}
	return time.Time{}
}

// CoolingSummary counts the keys that are out of rotation right now and
// reports the earliest moment one of them comes back: the first from the
// quota/cooldown class and the first from the credential class.
func (p *upstreamNodes) CoolingSummary(now time.Time) (cooling, quarantined int, nextReady, nextCredential time.Time) {
	if p == nil {
		return 0, 0, time.Time{}, time.Time{}
	}
	for _, node := range p.nodes {
		cred := node.credUntil.Load()
		if cred > now.UnixNano() {
			quarantined++
			when := time.Unix(0, cred)
			if nextCredential.IsZero() || when.Before(nextCredential) {
				nextCredential = when
			}
		}
		until := node.cooldownUntil.Load()
		if until > now.UnixNano() {
			cooling++
			when := time.Unix(0, until)
			if nextReady.IsZero() || when.Before(nextReady) {
				nextReady = when
			}
		}
	}
	return cooling, quarantined, nextReady, nextCredential
}

// AllCooling reports whether every key in the pool is out of rotation, and
// when the earliest one returns. An empty pool is not "cooling": it is
// unconfigured, which the caller reports separately.
func (p *upstreamNodes) AllCooling(now time.Time) (bool, time.Time) {
	if p == nil || len(p.nodes) == 0 {
		return false, time.Time{}
	}
	cooling, _, nextReady, _ := p.CoolingSummary(now)
	return cooling == len(p.nodes), nextReady
}

// AllQuarantined reports whether every key in the pool was rejected by the
// upstream credential check, and when the earliest quarantine expires.
func (p *upstreamNodes) AllQuarantined(now time.Time) (bool, time.Time) {
	if p == nil || len(p.nodes) == 0 {
		return false, time.Time{}
	}
	_, quarantined, _, nextCredential := p.CoolingSummary(now)
	return quarantined == len(p.nodes), nextCredential
}

// parseRetryAfter reads a Retry-After header in delta-seconds or HTTP-date
// form. It returns 0 for an empty, non-positive or unparsable value and for a
// date already in the past.
func parseRetryAfter(value string) time.Duration {
	if seconds, err := strconv.Atoi(strings.TrimSpace(value)); err == nil && seconds > 0 {
		return time.Duration(seconds) * time.Second
	}
	if when, err := http.ParseTime(value); err == nil {
		return max(time.Until(when), 0)
	}
	return 0
}
