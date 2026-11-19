package gateway

import (
	"context"
	"fmt"
	"net"
	"net/netip"
	"net/url"
	"sort"
	"sync"
	"time"

	"opencode2api/internal/config"
)

// newEdgePinConfig builds the optional edge pin. The host set comes from the
// anonymous upstream URL so the pin always targets the origin free-model
// traffic actually uses.
func newEdgePinConfig(cfg config.WarpConfig, upstreamAnonymous string) *edgePin {
	if !cfg.RotateEdge {
		return nil
	}
	u, err := url.Parse(upstreamAnonymous)
	if err != nil {
		return nil
	}
	host := u.Hostname()
	if host == "" {
		return nil
	}
	return newEdgePin([]string{host}, time.Duration(cfg.EdgePinTTL)*time.Second)
}

// edgePin opens a request to a chosen Cloudflare edge address instead of
// letting the go resolver pick one. The upstream free tier is keyed on both the
// egress /64 and the Cloudflare edge that terminates TLS, so spreading turns
// across the live AAAA set adds a second rotation axis on top of the WARP pool.
//
// Only hosts in allowedHosts are rewritten. Every other address keeps the
// normal resolution path, so a probe to Cloudflare's trace endpoint or a
// different upstream is never redirected to an OpenCode edge.
type edgePin struct {
	mu          sync.Mutex
	allowed     map[string]bool
	addresses   []netip.Addr
	resolvedAt  time.Time
	ttl         time.Duration
	preferred   netip.Addr
	retired     map[netip.Addr]time.Time
	resolver    *net.Resolver
	streamFails map[netip.Addr]int
	lastFail    map[netip.Addr]time.Time
}

func newEdgePin(hosts []string, ttl time.Duration) *edgePin {
	allowed := make(map[string]bool, len(hosts))
	for _, h := range hosts {
		allowed[h] = true
	}
	if ttl <= 0 {
		ttl = 10 * time.Minute
	}
	return &edgePin{
		allowed:     allowed,
		ttl:         ttl,
		retired:     make(map[netip.Addr]time.Time),
		streamFails: make(map[netip.Addr]int),
		lastFail:    make(map[netip.Addr]time.Time),
		resolver:    net.DefaultResolver,
	}
}

// resolve returns the live AAAA set for the pinned hosts. A failed lookup keeps
// the last good set so a transient DNS hiccup does not stop rotation; if there
// is no set yet the call falls back to the host name (normal resolution).
func (e *edgePin) resolve(ctx context.Context, host string) ([]netip.Addr, error) {
	if e == nil {
		return nil, fmt.Errorf("edge pin disabled")
	}
	e.mu.Lock()
	if time.Since(e.resolvedAt) < e.ttl && len(e.addresses) > 0 {
		out := append([]netip.Addr(nil), e.addresses...)
		e.mu.Unlock()
		return out, nil
	}
	e.mu.Unlock()

	lookCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	addrs, err := e.resolver.LookupNetIP(lookCtx, "ip6", host)
	if err != nil {
		e.mu.Lock()
		out := append([]netip.Addr(nil), e.addresses...)
		e.mu.Unlock()
		if len(out) > 0 {
			return out, nil
		}
		return nil, err
	}
	seen := make(map[netip.Addr]bool)
	clean := make([]netip.Addr, 0, len(addrs))
	for _, a := range addrs {
		if a.Is6() && !seen[a] {
			seen[a] = true
			clean = append(clean, a)
		}
	}
	sort.Slice(clean, func(i, j int) bool { return clean[i].Less(clean[j]) })

	e.mu.Lock()
	e.addresses = clean
	e.resolvedAt = time.Now()
	e.mu.Unlock()
	return append([]netip.Addr(nil), clean...), nil
}

// rewrite replaces the host in host:port with the selected edge address. It is
// a no-op for hosts that are not pinned or when no address could be resolved.
func (e *edgePin) rewrite(ctx context.Context, address string) (string, error) {
	if e == nil {
		return address, nil
	}
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return address, nil
	}
	if !e.allowed[host] {
		return address, nil
	}
	addrs, err := e.resolve(ctx, host)
	if err != nil || len(addrs) == 0 {
		return address, nil
	}
	pick := e.pick(addrs, time.Now())
	if !pick.IsValid() {
		return address, nil
	}
	// Remember the edge this connection used so a later stream reset can be
	// attributed to it. Session affinity keeps the same edge until retirement.
	e.mu.Lock()
	e.preferred = pick
	e.mu.Unlock()
	return net.JoinHostPort(pick.String(), port), nil
}

// pick chooses an eligible edge. Retired edges (two live-stream cuts inside the
// retirement window) are skipped until their ban expires; if every edge is
// retired the oldest ban is cleared so traffic keeps flowing.
func (e *edgePin) pick(addrs []netip.Addr, now time.Time) netip.Addr {
	e.mu.Lock()
	defer e.mu.Unlock()
	for a, until := range e.retired {
		if !until.After(now) {
			delete(e.retired, a)
		}
	}
	var eligible []netip.Addr
	for _, a := range addrs {
		if !e.retired[a].After(now) {
			eligible = append(eligible, a)
		}
	}
	if len(eligible) == 0 {
		eligible = addrs
		e.retired = make(map[netip.Addr]time.Time)
	}
	// Prefer the current pin while it is healthy: churning edges mid-session
	// loses prompt-cache affinity. A new session has no pin and picks randomly
	// from the set via the caller-supplied seed.
	if e.preferred.IsValid() {
		for _, a := range eligible {
			if a == e.preferred {
				return a
			}
		}
	}
	return eligible[0]
}

// setPreferred pins a session to one edge.
func (e *edgePin) setPreferred(addr netip.Addr) {
	if e == nil || !addr.IsValid() {
		return
	}
	e.mu.Lock()
	e.preferred = addr
	e.mu.Unlock()
}

// Preferred returns the edge currently pinned for new connections.
func (e *edgePin) Preferred() netip.Addr {
	if e == nil {
		return netip.Addr{}
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.preferred
}

// ReportStreamReset records a mid-stream cut on the pinned edge. Two cuts
// within five minutes retire that edge for new connections, matching the
// behaviour described by the upstream rotators; existing streams are untouched.
func (e *edgePin) ReportStreamReset(addr netip.Addr, now time.Time) bool {
	if e == nil || !addr.IsValid() {
		return false
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	const window = 5 * time.Minute
	if t, ok := e.lastFail[addr]; !ok || now.Sub(t) > window {
		e.streamFails[addr] = 0
	}
	e.streamFails[addr]++
	e.lastFail[addr] = now
	if e.streamFails[addr] >= 2 {
		e.retired[addr] = now.Add(30 * time.Minute)
		delete(e.streamFails, addr)
		if e.preferred == addr {
			e.preferred = netip.Addr{}
		}
		return true
	}
	return false
}
