package warp

import (
	"bufio"
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/netip"
	"strings"
	"sync"
	"time"

	"golang.zx2c4.com/wireguard/device"
	"golang.zx2c4.com/wireguard/tun/netstack"
)

const (
	probeTimeout    = 5 * time.Second
	probeCandidates = 10

	// laneProbeTimeout bounds the free-lane admission request. It is looser
	// than probeTimeout because the gateway answers a prompt, not a trace
	// document, and a slow answer is still a usable exit.
	laneProbeTimeout = 20 * time.Second

	// DefaultLaneProbeModel is the free model the admission request asks for.
	// Any live free model answers it; a model renamed upstream only costs a
	// failed admission until this constant is bumped.
	DefaultLaneProbeModel = "deepseek-v4.1-flash-free"
)

// errLaneBurnt reports a fresh exit whose free-lane allowance is already spent.
var errLaneBurnt = errors.New("free lane refused this WARP exit")

type Stack string

const (
	StackIPv4 Stack = "ipv4"
	StackIPv6 Stack = "ipv6"
	StackDual Stack = "dual"

	// DefaultStack is IPv6: a WARP IPv4 egress is a public address shared by
	// every user behind Cloudflare's NAT, so the upstream's per-address free
	// quota is usually already spent, while the IPv6 egress belongs to the
	// tunnel. The handshake still falls back to IPv4 endpoints when IPv6 does
	// not answer, so an IPv4-only host keeps working without extra settings.
	DefaultStack = StackIPv6
)

var (
	dnsV4 = netip.MustParseAddr("1.1.1.1")
	dnsV6 = netip.MustParseAddr("2606:4700:4700::1111")
)

func ParseStack(value string) (Stack, error) {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "":
		return DefaultStack, nil
	case "ipv6", "v6":
		return StackIPv6, nil
	case "ipv4", "v4":
		return StackIPv4, nil
	case "dual", "both":
		return StackDual, nil
	default:
		return "", fmt.Errorf("unsupported WARP stack %q; use ipv6, ipv4 or dual", value)
	}
}

type exit struct {
	id        string
	egress    netip.Addr
	source    netip.Addr
	stack     Stack
	device    *device.Device
	network   *netstack.Net
	mu        sync.Mutex
	refs      int
	retired   bool
	closed    bool
	penalized bool
	closeOnce sync.Once

	// regID/regToken identify the Cloudflare registration behind this exit and
	// drop deletes it once the exit is gone, so the next registration is a new
	// device with a new egress block.
	regID    string
	regToken string
	drop     func(id, token string)
	dropOnce sync.Once
	burned   bool
}

// markBurned records that the exit's allowance is spent, so closing it also
// deletes its Cloudflare device. A clean shutdown keeps the registration for
// the next process (state.go reuses it), a retired-because-burned one does not.
func (e *exit) markBurned() {
	e.mu.Lock()
	e.burned = true
	e.mu.Unlock()
}

// bindRegistration records the registration an exit was built from and how to
// delete it when the exit is retired.
func (e *exit) bindRegistration(reg registration, drop func(id, token string)) {
	e.regID, e.regToken, e.drop = reg.ID, reg.Token, drop
}

// dropRegistration deletes the Cloudflare device behind this exit, once.
func (e *exit) dropRegistration() {
	if e == nil || e.drop == nil {
		return
	}
	e.dropOnce.Do(func() { e.drop(e.regID, e.regToken) })
}

// admit proves the exit can answer on the free lane before the pool rotates
// real traffic onto it. A tunnel that reaches Cloudflare's trace endpoint can
// still be refused by the gateway with FreeUsageLimitError, and admitting such
// an exit spends one production request and one rotation to learn what a single
// minimal prompt says up front.
func (e *exit) admit(ctx context.Context, probe LaneProbe) error {
	if probe.URL == "" || probe.Model == "" {
		return nil
	}
	probeCtx, cancel := context.WithTimeout(ctx, laneProbeTimeout)
	defer cancel()
	network := "tcp6"
	if e.stack == StackIPv4 {
		network = "tcp4"
	}
	transport := &http.Transport{
		Proxy:               nil,
		ForceAttemptHTTP2:   false,
		DisableKeepAlives:   true,
		TLSHandshakeTimeout: 10 * time.Second,
		DialContext: func(ctx context.Context, _, address string) (net.Conn, error) {
			return e.network.DialContext(ctx, network, address)
		},
	}
	defer transport.CloseIdleConnections()
	body := fmt.Sprintf(`{"model":%q,"messages":[{"role":"user","content":"ok"}],"max_tokens":1}`, probe.Model)
	req, err := http.NewRequestWithContext(probeCtx, http.MethodPost, probe.URL, strings.NewReader(body))
	if err != nil {
		return err
	}
	session := probeSession()
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "opencode/latest/2.1.1/cli")
	req.Header.Set("x-opencode-client", "cli")
	req.Header.Set("x-opencode-session", session)
	req.Header.Set("x-opencode-session-id", session)
	req.Header.Set("x-session-affinity", session)
	req.Header.Set("X-Session-Id", session)
	if probe.Auth != "" {
		req.Header.Set("Authorization", probe.Auth)
	}
	resp, err := (&http.Client{Transport: transport}).Do(req)
	if err != nil {
		return fmt.Errorf("free-lane admission: %w", err)
	}
	defer resp.Body.Close()
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<16))
	switch {
	case resp.StatusCode >= 200 && resp.StatusCode < 300:
		return nil
	case resp.StatusCode == http.StatusTooManyRequests:
		return fmt.Errorf("%w: HTTP 429", errLaneBurnt)
	default:
		return fmt.Errorf("free-lane admission: HTTP %d", resp.StatusCode)
	}
}

// probeSession mints a session id in the CLI's shape so the admission request
// carries the identity headers the gateway fingerprints.
func probeSession() string {
	var raw [6]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "ses_0000000000000000000000000000"
	}
	const alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	var tail [14]byte
	for index := range tail {
		tail[index] = alphabet[int(raw[index%len(raw)])%len(alphabet)]
	}
	return "ses_" + hex.EncodeToString(raw[:]) + string(tail[:])
}

func newExit(ctx context.Context, reg registration) (*exit, error) {
	return newExitWithStack(ctx, reg, DefaultStack, netip.Addr{})
}

// newExitWithStack builds one userspace tunnel for reg. A valid source pins
// the outer WireGuard socket to that address and restricts the peer endpoints
// to IPv6, so the public egress is derived from the rotating source rather than
// from whatever address the host happens to share.
func newExitWithStack(ctx context.Context, reg registration, stack Stack, source netip.Addr) (*exit, error) {
	normalized, err := ParseStack(string(stack))
	if err != nil {
		return nil, err
	}
	stack = normalized
	localV6, v6Err := parseAddress(reg.AddressV6)
	localV4, v4Err := parseAddress(reg.AddressV4)
	var localAddresses []netip.Addr
	var dns []netip.Addr
	switch stack {
	case StackIPv6:
		if v6Err != nil || !localV6.Is6() {
			return nil, fmt.Errorf("invalid WARP IPv6 address %q", reg.AddressV6)
		}
		localAddresses = []netip.Addr{localV6}
		dns = []netip.Addr{dnsV6}
	case StackIPv4:
		if v4Err != nil || !localV4.Is4() {
			return nil, fmt.Errorf("invalid WARP IPv4 address %q", reg.AddressV4)
		}
		localAddresses = []netip.Addr{localV4}
		dns = []netip.Addr{dnsV4}
	case StackDual:
		if v6Err == nil && localV6.Is6() {
			localAddresses = append(localAddresses, localV6)
		}
		if v4Err == nil && localV4.Is4() {
			localAddresses = append(localAddresses, localV4)
		}
		if len(localAddresses) == 0 {
			return nil, fmt.Errorf("WARP registration has no usable address: v4=%q v6=%q", reg.AddressV4, reg.AddressV6)
		}
		dns = []netip.Addr{dnsV4, dnsV6}
	}
	tunDevice, network, err := netstack.CreateNetTUN(localAddresses, dns, 1280)
	if err != nil {
		return nil, fmt.Errorf("create WARP network stack: %w", err)
	}
	privateKey, err := uapiKey(reg.PrivateKey)
	if err != nil {
		tunDevice.Close()
		return nil, fmt.Errorf("decode WARP private key: %w", err)
	}
	peerKey, err := uapiKey(reg.PeerPublicKey)
	if err != nil {
		tunDevice.Close()
		return nil, fmt.Errorf("decode WARP peer key: %w", err)
	}
	endpoints, err := resolveEndpoints(ctx, reg.Endpoints, reg.Endpoint)
	if err != nil {
		tunDevice.Close()
		return nil, err
	}
	if source.IsValid() {
		// Try IPv6 endpoints first so the rotating source is used, but keep
		// the IPv4 ones behind them: a host (or network) that cannot reach
		// Cloudflare over IPv6 still gets a working tunnel, just with the
		// shared address the prefix was supposed to avoid.
		endpoints = preferIPv6Endpoints(endpoints)
	} else {
		// Without a pinned source there is no rotation to protect. Prefer the
		// IPv4 endpoint so a host with only IPv4 routing (WSL, many VPS NATs)
		// does not spend the probe budget on unreachable IPv6 handshakes.
		endpoints = preferIPv4Endpoints(endpoints)
	}
	wg := device.NewDevice(tunDevice, &simpleBind{source: source}, device.NewLogger(device.LogLevelError, "warp: "))
	uapi := fmt.Sprintf(
		"private_key=%s\npublic_key=%s\nallowed_ip=0.0.0.0/0\nallowed_ip=::/0\nendpoint=%s\npersistent_keepalive_interval=25\n",
		privateKey, peerKey, endpoints[0],
	)
	if err := wg.IpcSet(uapi); err != nil {
		wg.Close()
		<-wg.Wait()
		return nil, fmt.Errorf("configure WARP device: %w", err)
	}
	if err := wg.Up(); err != nil {
		wg.Close()
		<-wg.Wait()
		return nil, fmt.Errorf("start WARP device: %w", err)
	}
	e := &exit{id: reg.ID, source: source, stack: stack, device: wg, network: network}
	egress, err := e.probe(ctx, wg, peerKey, endpoints)
	if err != nil {
		e.close()
		return nil, err
	}
	e.egress = egress
	return e, nil
}

// preferIPv6Endpoints returns endpoints with the IPv6 ones first and the IPv4
// ones after them, both in their original order. Nothing is dropped: an IPv4
// fallback keeps WARP usable on a host that cannot reach Cloudflare over IPv6.
func preferIPv6Endpoints(endpoints []string) []string {
	return orderEndpoints(endpoints, true)
}

// preferIPv4Endpoints is the inverse of preferIPv6Endpoints. It is used when
// the tunnel has no pinned outer source: an IPv4 endpoint avoids spending the
// startup probe on IPv6 handshakes that cannot leave a WSL/NAT-only host.
func preferIPv4Endpoints(endpoints []string) []string {
	return orderEndpoints(endpoints, false)
}

func orderEndpoints(endpoints []string, ipv6First bool) []string {
	first := make([]string, 0, len(endpoints))
	second := make([]string, 0, len(endpoints))
	for _, endpoint := range endpoints {
		host, _, err := net.SplitHostPort(endpoint)
		if err != nil {
			continue
		}
		addr, err := netip.ParseAddr(host)
		if err != nil || addr.Is4In6() {
			continue
		}
		isV6 := addr.Is6()
		if isV6 == ipv6First {
			first = append(first, endpoint)
		} else {
			second = append(second, endpoint)
		}
	}
	return append(first, second...)
}

func resolveEndpoints(ctx context.Context, candidates []string, fallback string) ([]string, error) {
	if len(candidates) == 0 && fallback != "" {
		candidates = []string{fallback}
	}
	// Cloudflare's registration response can contain only IPv6 endpoints.
	// Keep the static IPv4 endpoints as a fallback so a host without IPv6
	// routing (WSL, NAT-only VPS) can still establish the tunnel.
	candidates = append(candidates, fallbackEndpoints...)
	seen := make(map[string]bool)
	var endpoints []string
	for _, candidate := range candidates {
		normalized, err := normalizeEndpoint(candidate)
		if err != nil {
			continue
		}
		host, port, _ := net.SplitHostPort(normalized)
		if net.ParseIP(host) != nil {
			if !seen[normalized] {
				seen[normalized] = true
				endpoints = append(endpoints, normalized)
			}
		} else {
			addrs, err := net.DefaultResolver.LookupIPAddr(ctx, host)
			if err != nil {
				continue
			}
			for _, ip := range addrs {
				value := net.JoinHostPort(ip.IP.String(), port)
				if !seen[value] {
					seen[value] = true
					endpoints = append(endpoints, value)
				}
			}
		}
		if len(endpoints) >= probeCandidates {
			break
		}
	}
	if len(endpoints) == 0 {
		return nil, fmt.Errorf("resolve WARP endpoint candidates %v", candidates)
	}
	return endpoints[:min(len(endpoints), probeCandidates)], nil
}

func normalizeEndpoint(endpoint string) (string, error) {
	endpoint = strings.TrimSpace(endpoint)
	if !strings.Contains(endpoint, ":") {
		endpoint = net.JoinHostPort(endpoint, "2408")
	} else if addr, err := netip.ParseAddr(endpoint); err == nil {
		endpoint = net.JoinHostPort(addr.String(), "2408")
	}
	host, port, err := net.SplitHostPort(endpoint)
	if err != nil {
		return "", fmt.Errorf("invalid WARP endpoint %q", endpoint)
	}
	if port == "" || port == "0" {
		port = "2408"
	}
	return net.JoinHostPort(host, port), nil
}

func parseAddress(value string) (netip.Addr, error) {
	value = strings.TrimSpace(value)
	if addr, err := netip.ParseAddr(value); err == nil {
		return addr.Unmap(), nil
	}
	prefix, err := netip.ParsePrefix(value)
	if err != nil {
		return netip.Addr{}, err
	}
	return prefix.Addr().Unmap(), nil
}

func uapiKey(encoded string) (string, error) {
	key, err := base64.StdEncoding.DecodeString(strings.TrimSpace(encoded))
	if err != nil || len(key) != 32 {
		return "", errors.New("invalid WireGuard key")
	}
	return hex.EncodeToString(key), nil
}

type probeTarget struct {
	network string
	url     string
}

func probeTargets(stack Stack) []probeTarget {
	switch stack {
	case StackIPv4:
		return []probeTarget{{network: "tcp4", url: "https://1.1.1.1/cdn-cgi/trace"}}
	case StackDual:
		return []probeTarget{
			{network: "tcp4", url: "https://1.1.1.1/cdn-cgi/trace"},
			{network: "tcp6", url: "https://[2606:4700:4700::1111]/cdn-cgi/trace"},
		}
	default:
		return []probeTarget{{network: "tcp6", url: "https://[2606:4700:4700::1111]/cdn-cgi/trace"}}
	}
}

func (e *exit) probe(ctx context.Context, wg *device.Device, peerKey string, endpoints []string) (netip.Addr, error) {
	var lastErr error
	for index, endpoint := range endpoints {
		if index > 0 {
			if err := wg.IpcSet(fmt.Sprintf("public_key=%s\nendpoint=%s\n", peerKey, endpoint)); err != nil {
				lastErr = fmt.Errorf("switch WARP endpoint to %s: %w", endpoint, err)
				continue
			}
		}
		for _, target := range probeTargets(e.stack) {
			egress, err := e.probeTarget(ctx, target)
			if err == nil {
				return egress, nil
			}
			lastErr = err
		}
	}
	return netip.Addr{}, fmt.Errorf("verify WARP %s egress: %w", e.stack, lastErr)
}

func (e *exit) probeTarget(ctx context.Context, target probeTarget) (netip.Addr, error) {
	probeCtx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()
	transport := &http.Transport{
		Proxy:               nil,
		ForceAttemptHTTP2:   false,
		DisableKeepAlives:   true,
		TLSHandshakeTimeout: 10 * time.Second,
		DialContext: func(ctx context.Context, _, address string) (net.Conn, error) {
			return e.network.DialContext(ctx, target.network, address)
		},
	}
	defer transport.CloseIdleConnections()
	req, err := http.NewRequestWithContext(probeCtx, http.MethodGet, target.url, nil)
	if err != nil {
		return netip.Addr{}, err
	}
	resp, err := (&http.Client{Transport: transport}).Do(req)
	if err != nil {
		return netip.Addr{}, err
	}
	defer resp.Body.Close()
	egress, err := parseTrace(resp.Body)
	if err != nil {
		return netip.Addr{}, err
	}
	if target.network == "tcp4" && !egress.Is4() {
		return netip.Addr{}, fmt.Errorf("WARP trace over IPv4 reported %s", egress)
	}
	if target.network == "tcp6" && !egress.Is6() {
		return netip.Addr{}, fmt.Errorf("WARP trace over IPv6 reported %s", egress)
	}
	return egress, nil
}

func parseTrace(body io.Reader) (netip.Addr, error) {
	scanner := bufio.NewScanner(io.LimitReader(body, 64<<10))
	var rawIP string
	warpEnabled := false
	for scanner.Scan() {
		key, value, ok := strings.Cut(scanner.Text(), "=")
		if !ok {
			continue
		}
		switch strings.TrimSpace(key) {
		case "ip":
			rawIP = strings.TrimSpace(value)
		case "warp":
			warpEnabled = strings.TrimSpace(value) == "on"
		}
	}
	if err := scanner.Err(); err != nil {
		return netip.Addr{}, err
	}
	if !warpEnabled {
		return netip.Addr{}, errors.New("Cloudflare trace reports warp=off")
	}
	addr, err := netip.ParseAddr(rawIP)
	if err != nil {
		return netip.Addr{}, fmt.Errorf("invalid trace IP %q", rawIP)
	}
	addr = addr.Unmap()
	if (!addr.Is4() && !addr.Is6()) || !addr.IsGlobalUnicast() || addr.IsPrivate() {
		return netip.Addr{}, fmt.Errorf("trace returned non-public address %s", addr)
	}
	return addr, nil
}

func (e *exit) addRef() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.retired || e.closed {
		return false
	}
	e.refs++
	return true
}

func (e *exit) release() {
	e.mu.Lock()
	if e.refs > 0 {
		e.refs--
	}
	closeNow := e.refs == 0 && e.retired && !e.closed
	e.mu.Unlock()
	if closeNow {
		e.close()
	}
}

func (e *exit) retire() {
	e.mu.Lock()
	e.retired = true
	closeNow := e.refs == 0 && !e.closed
	e.mu.Unlock()
	if closeNow {
		e.close()
	}
}

func (e *exit) close() {
	e.closeOnce.Do(func() {
		e.mu.Lock()
		e.closed = true
		burned := e.burned
		e.mu.Unlock()
		if e.device != nil {
			e.device.Close()
			<-e.device.Wait()
		}
		if burned {
			e.dropRegistration()
		}
	})
}

func (e *exit) observe(status int, body []byte) (bool, string) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.penalized {
		return false, ""
	}
	if status == http.StatusForbidden {
		reason := "HTTP 403"
		if networkAnomaly(body) {
			reason += " network anomaly"
		}
		e.burned = true
		return true, reason
	}
	// A free-tier 429 is scoped to the egress address, not to the request, the
	// key or the session: retrying the same exit repeats it. Replace the exit
	// on the first one, as the instant-rotate deployment does, so the retry
	// draws a fresh outer source and a new public egress.
	if status == http.StatusTooManyRequests {
		e.burned = true
		return true, "HTTP 429"
	}
	return false, ""
}

func (e *exit) isPenalized() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.penalized
}

func (e *exit) markPenalized() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.penalized {
		return false
	}
	e.penalized = true
	return true
}

func (e *exit) clearPenalty() {
	e.mu.Lock()
	e.penalized = false
	e.burned = false
	e.mu.Unlock()
}

func networkAnomaly(body []byte) bool {
	return strings.Contains(strings.ToLower(string(body)), "network")
}

func (e *exit) dialContext(ctx context.Context, network, address string) (net.Conn, error) {
	if e.network == nil {
		return nil, errors.New("WARP network is closed")
	}
	network = strings.ToLower(strings.TrimSpace(network))
	switch network {
	case "tcp":
		network = e.dialNetwork()
	case "tcp4", "tcp6":
	default:
		return nil, fmt.Errorf("WARP supports TCP only, got %q", network)
	}
	switch e.stack {
	case StackIPv4:
		if network != "tcp4" {
			return nil, fmt.Errorf("WARP IPv4 stack cannot dial %q", network)
		}
	case StackIPv6:
		if network != "tcp6" {
			return nil, fmt.Errorf("WARP IPv6 stack cannot dial %q", network)
		}
	case StackDual:
		// Both address families are present in the netstack.
	default:
		return nil, fmt.Errorf("unsupported WARP stack %q", e.stack)
	}
	return e.network.DialContext(ctx, network, address)
}

func (e *exit) dialNetwork() string {
	switch e.stack {
	case StackIPv4:
		return "tcp4"
	case StackDual:
		if e.egress.Is4() {
			return "tcp4"
		}
		return "tcp6"
	default:
		return "tcp6"
	}
}

func (e *exit) egressKey() string {
	if e.egress.Is4() {
		return netip.PrefixFrom(e.egress, 32).Masked().String()
	}
	if !e.egress.Is6() {
		return ""
	}
	// The free tier treats a Cloudflare /64 as one quota scope: rotating inside
	// the same /64 yields a new /128 but the same exhausted quota. Keying on
	// the /64 keeps every active and cooling exit on a distinct scope, so a
	// replacement is a real rotation rather than a cosmetic address change.
	return netip.PrefixFrom(e.egress, 64).Masked().String()
}

// idle reports whether no lease currently holds this exit.
func (e *exit) idle() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.refs == 0 && !e.retired && !e.closed
}
