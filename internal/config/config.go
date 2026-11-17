// Package config loads, validates, persists, and redacts service configuration.
package config

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"net/netip"
	"net/url"
	"os"
	"slices"
	"strings"
	"time"
	"unicode"

	"opencode2api/internal/jsonutil"
	wire "opencode2api/internal/protocol"
	"opencode2api/internal/warp"
)

// Config is the service configuration stored in config.json.
//
// ServerKeys are the local keys clients present to the gateway; ZenKeys and
// GoKeys are the upstream keys of the two keyed tiers, and Anonymous enables
// the keyless free tier. Proxies lists the configured proxy entries; the
// entries read from ProxyFile join them only in the resolved list that
// RuntimeProxies returns, which is never saved. Prefer picks the keyed tier
// that is tried first when both serve a model.
type Config struct {
	Rotation          RotationConfig               `json:"rotation"`
	Listen            string                       `json:"listen"`
	ServerKeys        []string                     `json:"server_keys"`
	ServerKeyMetadata map[string]ServerKeyMetadata `json:"server_key_metadata,omitempty"`
	ZenKeys           []string                     `json:"zen_keys"`
	GoKeys            []string                     `json:"go_keys"`
	Anonymous         bool                         `json:"anonymous"`
	Warp              WarpConfig                   `json:"warp"`
	Proxies           []string                     `json:"proxies"`
	ProxyFile         string                       `json:"proxyfile"`
	ProxyBindings     map[string]string            `json:"proxy_bindings,omitempty"`
	Upstream          UpstreamConfig               `json:"upstream"`
	Retry             RetryConfig                  `json:"retry"`
	Models            ModelsConfig                 `json:"models"`
	Performance       PerformanceConfig            `json:"performance"`
	Logging           LoggingConfig                `json:"logging"`
	Storage           StorageConfig                `json:"storage,omitempty"`
	WebUI             WebUIConfig                  `json:"webui"`
	Prefer            Tier                         `json:"prefer"`
	Reasoning         ReasoningConfig              `json:"reasoning"`
	// Peers are other o2a-compatible gateways this process falls back to when
	// every local lane fails, so one listen address serves the whole fleet
	// without an external router.
	Peers []PeerConfig `json:"peers,omitempty"`

	effectiveProxies []string
}

// ReasoningConfig forces a thinking level for requests that do not state one.
//
// Effort is the default for every model; EffortByModel overrides it for the
// model IDs it names. A level a client sends explicitly (Anthropic's
// output_config.effort / top-level effort, Chat's reasoning_effort, or
// Responses' reasoning.effort) always wins, so a forced level only replaces a
// value that would otherwise have been derived from thinking.budget_tokens or
// left unset. Besides the levels themselves, "none" strips reasoning from the
// request, and "auto" applies, for free models on the Zen tier only, the
// highest level the availability probe found the model to accept.
type ReasoningConfig struct {
	// omitempty keeps a saved configuration free of the fields the operator
	// never set, matching how the rest of the optional surface is persisted.
	Effort        string            `json:"effort,omitempty"`
	EffortByModel map[string]string `json:"effort_by_model,omitempty"`
}

// WarpConfig controls the optional Cloudflare WARP egress. While enabled,
// free-model traffic on the anonymous lane and the free Zen-key fallback is
// sent through a userspace WireGuard tunnel instead of any configured proxy.
// PoolSize is the number of registered WARP exits kept warm (the last one is
// held as hot standby); Stack selects the tunnel's address family. Initialization
// is fail-closed: a configuration that enables WARP is rejected when no exit
// can be established.
type WarpConfig struct {
	Enabled  bool   `json:"enabled"`
	PoolSize int    `json:"pool_size,omitempty"`
	Stack    string `json:"stack,omitempty"`
	// SourceIPv6 is an outer IPv6 prefix (usually a routed /64) each WARP exit
	// binds its WireGuard socket to. Rotating the source is what gives a
	// replacement exit a new public egress; without it, exits share one
	// address and an egress-scoped free quota stays exhausted.
	SourceIPv6 string `json:"source_ipv6,omitempty"`
	// AttemptTimeoutSeconds bounds one WARP attempt before its response is
	// committed. A tunnel that stops answering is dropped quickly so the
	// request can fall back to a direct egress instead of waiting for the
	// request-level deadline.
	AttemptTimeoutSeconds int `json:"attempt_timeout_seconds,omitempty"`
	// PauseAfterQuotaSeconds skips WARP after the anonymous phase reported a
	// spent free-tier quota, so the key tiers are not sent through the same
	// slow, quota-walled exits.
	PauseAfterQuotaSeconds int `json:"pause_after_quota_seconds,omitempty"`
	// RotateAfterMinutes retires an idle exit once it has served this long, so
	// a working address is not kept for a whole day. The replacement registers
	// in the background and only idle exits are taken, so requests are not
	// interrupted. Zero disables it.
	RotateAfterMinutes int `json:"rotate_after_minutes,omitempty"`
	// RotateCooldownSeconds is the minimum spacing between two registrations
	// caused by retiring an exit on age. It keeps a burst of voluntary
	// rotations from minting Cloudflare devices faster than its rate limit.
	RotateCooldownSeconds int `json:"rotate_cooldown_seconds,omitempty"`
	// Preload is how many exits to keep warm while idle. Zero uses half of
	// PoolSize (at least one). Raise it on an IPv4-only host: Cloudflare needs
	// ~50 s for the first registration and paces later ones, so a burst would
	// otherwise arrive before enough distinct /64 exits exist.
	Preload int `json:"preload,omitempty"`
	// RequireIPv6Egress discards a registration whose public egress is IPv4.
	// An IPv4 WARP exit is a Cloudflare address shared by all users, so its
	// free-tier quota is usually already spent. Set this on `dual` stacks:
	// the tunnel handshakes over IPv4 but traffic to AAAA destinations still
	// leaves from the device's own /64. On a native `ipv6` stack it is a no-op.
	RequireIPv6Egress bool `json:"require_ipv6_egress,omitempty"`
}

// Normalize fills the defaults and canonicalizes the stack name.
func (w *WarpConfig) Normalize() error {
	if w.PoolSize == 0 {
		w.PoolSize = 2
	}
	if strings.TrimSpace(w.Stack) == "" {
		w.Stack = string(warp.DefaultStack)
	}
	stack, err := warp.ParseStack(w.Stack)
	if err != nil {
		return fmt.Errorf("warp.stack: %w", err)
	}
	w.Stack = string(stack)
	if w.PoolSize < 1 || w.PoolSize > 32 {
		return errors.New("warp.pool_size must be between 1 and 32")
	}
	if w.AttemptTimeoutSeconds == 0 {
		w.AttemptTimeoutSeconds = 15
	}
	if w.AttemptTimeoutSeconds < 3 || w.AttemptTimeoutSeconds > 120 {
		return errors.New("warp.attempt_timeout_seconds must be between 3 and 120")
	}
	if w.PauseAfterQuotaSeconds == 0 {
		w.PauseAfterQuotaSeconds = 300
	}
	if w.PauseAfterQuotaSeconds < 0 || w.PauseAfterQuotaSeconds > 86400 {
		return errors.New("warp.pause_after_quota_seconds must be between 0 and 86400")
	}
	if w.RotateAfterMinutes < 0 || w.RotateAfterMinutes > 1440 {
		return errors.New("warp.rotate_after_minutes must be between 0 and 1440")
	}
	if w.RotateCooldownSeconds == 0 {
		w.RotateCooldownSeconds = int(warp.DefaultRotateCooldown / time.Second)
	}
	if w.RotateCooldownSeconds < 30 || w.RotateCooldownSeconds > 3600 {
		return errors.New("warp.rotate_cooldown_seconds must be between 30 and 3600")
	}
	if w.Preload < 0 || w.Preload > 32 {
		return errors.New("warp.preload must be between 0 and 32 (0 = half of pool_size)")
	}
	w.SourceIPv6 = strings.TrimSpace(w.SourceIPv6)
	if w.SourceIPv6 != "" {
		// A comma-separated list lets several routed /64s (or one /48 from a
		// tunnel broker) feed the pool: every exit draws from one of them, and
		// each block is metered separately by the free tier.
		parts := strings.Split(w.SourceIPv6, ",")
		normalized := make([]string, 0, len(parts))
		for _, part := range parts {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			prefix, err := netip.ParsePrefix(part)
			if err != nil || !prefix.Addr().Is6() || prefix.Addr().Is4In6() {
				return fmt.Errorf("warp.source_ipv6: %q is not an IPv6 prefix", part)
			}
			if prefix.Bits() > 64 {
				return fmt.Errorf("warp.source_ipv6: %s must be /64 or shorter", prefix)
			}
			normalized = append(normalized, prefix.Masked().String())
		}
		if len(normalized) == 0 {
			return errors.New("warp.source_ipv6: no prefix given")
		}
		if stack != warp.StackIPv6 {
			return errors.New("warp.source_ipv6 requires warp.stack to be \"ipv6\"; an IPv4 stack cannot present an IPv6 egress")
		}
		w.SourceIPv6 = strings.Join(normalized, ",")
	}
	return nil
}

// UpstreamConfig holds the base URLs of the OpenCode origins. The gateway
// picks the request shape from the base URL (models.IsV1Upstream): one ending
// in /zen is the v1 lane, any other a v2 lane.
type UpstreamConfig struct {
	// Zen is the base URL of the keyed Zen tier. Its default,
	// https://opencode.ai/zen, is the v1 lane, which expects the OpenCode 1.x
	// client request shape.
	Zen string `json:"zen"`
	// Go is the base URL of the keyed Go plan tier. Its default,
	// https://opencode.ai/zen/go, is a v2 lane, which expects the OpenCode 2.x
	// CLI shape. The WebUI also reads the plan's quota from its /v1/usage.
	Go string `json:"go"`
	// Anonymous is the v2 /inference origin of the keyless free tier. The
	// anonymous lane uses only this origin: the v1 /zen fallback is retired for
	// free models and its call site is commented out. Empty means
	// https://opencode.ai/inference.
	Anonymous string `json:"anonymous"`
	// PromptEnvironment overrides the values the gateway injects into the
	// official CLI prompt's <env> block and the shell tool description. The
	// gateway runs on a Linux VPS, but clients may run elsewhere; the free tier
	// reads this block, so it must describe the client's world without letting
	// arbitrary operator text break the request shape.
	PromptEnvironment *PromptEnvironmentConfig `json:"prompt_environment,omitempty"`
}

// PromptEnvironmentConfig replaces the host-derived fields of the injected
// <env> block. Field names, order and the UTC date stay exactly as the official
// CLI writes them; only the values change.
type PromptEnvironmentConfig struct {
	WorkingDirectory string `json:"working_directory,omitempty"`
	WorkspaceRoot    string `json:"workspace_root,omitempty"`
	Platform         string `json:"platform,omitempty"`
	IsGitRepo        *bool  `json:"is_git_repo,omitempty"`
}

// Normalize rejects values that could break the prompt or claim another
// client's environment, and folds "windows" onto the CLI's win32 spelling.
func (environment *PromptEnvironmentConfig) Normalize() error {
	if environment == nil {
		return nil
	}
	for name, value := range map[string]string{
		"working_directory": environment.WorkingDirectory,
		"workspace_root":    environment.WorkspaceRoot,
	} {
		for _, character := range value {
			if unicode.IsControl(character) || character == '\u2028' || character == '\u2029' || character == '<' || character == '>' {
				return fmt.Errorf("upstream.prompt_environment.%s must not contain control characters, line separators, or angle brackets", name)
			}
		}
	}
	platform := strings.ToLower(strings.TrimSpace(environment.Platform))
	if platform == "windows" {
		platform = "win32"
	}
	switch platform {
	case "", "linux", "darwin", "win32":
	default:
		return errors.New("upstream.prompt_environment.platform must be linux, darwin, win32, windows, or empty")
	}
	environment.WorkingDirectory = strings.TrimSpace(environment.WorkingDirectory)
	environment.WorkspaceRoot = strings.TrimSpace(environment.WorkspaceRoot)
	environment.Platform = platform
	return nil
}

// RetryConfig bounds the upstream retries. MaxAttempts caps the attempts of a
// retry loop: the keyed attempts of a client request outside the rotation
// path, and the catalog refresh. For the anonymous lane it is instead a floor
// for how many rotating proxies may be drawn. TimeoutSeconds is the deadline
// of the whole client request, retries included.
type RetryConfig struct {
	MaxAttempts    int `json:"max_attempts"`
	TimeoutSeconds int `json:"timeout_seconds"`
}

// ModelsConfig controls model discovery and routing. RefreshSeconds is the
// catalog refresh interval, and Protocols maps a model ID to the wire protocol
// used upstream, overriding the one the catalog reports.
type ModelsConfig struct {
	RefreshSeconds int               `json:"refresh_seconds"`
	Protocols      map[string]string `json:"protocols"`
	// FreeOnly limits discovery and routing to free models, even when Zen or
	// Go keys could serve paid ones. Free models may still fall back to keys.
	FreeOnly bool `json:"free_only"`
	// ActiveProbes enables background active inference requests for free model
	// health checks. When false (default), availability is learned passively
	// from actual user requests, eliminating quota consumption from synthetic probes.
	ActiveProbes bool `json:"active_probes,omitempty"`
	// AnonymousBackoff configures a circuit breaker for the keyless lane:
	// after Threshold consecutive failed attempts, or when every exit is under
	// the free tier's IP block, the lane pauses for BaseHours, doubling on
	// every further trip up to MaxHours. An absent block leaves the lane
	// unpaused, which is the previous behavior.
	AnonymousBackoff *AnonymousBackoffConfig `json:"anonymous_backoff,omitempty"`
	// ContextOverrides publishes different limits in /v1/models than the
	// capability catalog states. The free tier enforces looser context than it
	// advertises (measured: a model advertised at 200k served 308k input
	// tokens), so a deployment can tell clients what actually works without
	// patching each one. Zero fields keep the catalog value.
	ContextOverrides map[string]ModelLimits `json:"context_overrides,omitempty"`
	// QuotaProbeSeconds re-tests a cooling upstream credential or relay with
	// one minimal request, so a quota window that reopens early (or a
	// Retry-After that overshoots) is noticed without waiting for real
	// traffic. 0 disables it; otherwise it is the interval between probes.
	QuotaProbeSeconds int `json:"quota_probe_seconds,omitempty"`
	// DeadIDs hides model IDs that upstream has retired. A stale disk cache or
	// a lagging capability catalog can still list them, so they are filtered
	// on every route and discovery list. An explicit empty array re-enables
	// all IDs; omitting the field applies DefaultDeadModels.
	DeadIDs []string `json:"dead_ids,omitempty"`
}

// DefaultDeadModels are Zen IDs confirmed retired upstream: a 410
// "model has been deprecated" and a 400 "model is unavailable" respectively.
// They are absent from the live free listing, so serving them only spends
// retries on answers no client can use.
var DefaultDeadModels = []string{
	"longcat-2.5-preview-free",
	"nemotron-3-ultra-free",
}

// ModelLimits overrides the per-model limits advertised to discovery clients.
// A zero field keeps the catalog value.
type ModelLimits struct {
	Context int `json:"context,omitempty"`
	Input   int `json:"input,omitempty"`
	Output  int `json:"output,omitempty"`
}

// AnonymousBackoffConfig tunes the keyless lane's circuit breaker. Zero fields
// take the defaults shown in Normalize.
type AnonymousBackoffConfig struct {
	Threshold int `json:"threshold,omitempty"`
	BaseHours int `json:"base_hours,omitempty"`
	MaxHours  int `json:"max_hours,omitempty"`
}

// Normalize fills the defaults and rejects nonsensical windows.
func (b *AnonymousBackoffConfig) Normalize() error {
	if b == nil {
		return nil
	}
	if b.Threshold == 0 {
		b.Threshold = 6
	}
	if b.BaseHours == 0 {
		b.BaseHours = 1
	}
	if b.MaxHours == 0 {
		b.MaxHours = 24
	}
	if b.Threshold < 1 {
		return errors.New("models.anonymous_backoff.threshold must be at least 1")
	}
	if b.BaseHours < 1 {
		return errors.New("models.anonymous_backoff.base_hours must be at least 1")
	}
	if b.MaxHours < b.BaseHours {
		return errors.New("models.anonymous_backoff.max_hours must not be smaller than base_hours")
	}
	return nil
}

// LoggingConfig sets the minimum log level (debug, info, warn or error) and
// RingSize, the number of recent log events kept in memory for the WebUI.
type LoggingConfig struct {
	Level    string `json:"level"`
	RingSize int    `json:"ring_size"`
	// DumpRequestBodies logs the exact bytes sent upstream (redacted, capped)
	// so retry and translation problems can be diagnosed without patching the
	// binary. Off by default: the bodies contain conversation content.
	DumpRequestBodies bool `json:"dump_request_bodies"`
}

// StorageConfig selects the long-lived usage database. UsageSQLite is the
// path to a SQLite file; an empty value disables persistence. Relative paths
// are resolved from the process working directory, so an absolute path is
// recommended for a service.
type StorageConfig struct {
	UsageSQLite string `json:"usage_sqlite,omitempty"`
}

// WebUIConfig configures the management WebUI. Password is a plaintext
// password supplied only to set one: it is hashed into PasswordHash and
// cleared before the configuration is saved. SessionTTLMinutes is the
// lifetime of a login session. Changes to Enabled and Listen take effect only
// after a restart.
type WebUIConfig struct {
	Enabled           bool   `json:"enabled"`
	Listen            string `json:"listen"`
	Username          string `json:"username"`
	Password          string `json:"password,omitempty"`
	PasswordHash      string `json:"password_hash,omitempty"`
	SessionTTLMinutes int    `json:"session_ttl_minutes"`
}

// PerformanceConfig tunes the shared upstream transports and failure
// handling: the connection pool limits (MaxConnsPerHost 0 means unlimited),
// the dial timeout, the base cooldown after an upstream key or anonymous
// proxy fails, and the per-attempt and first-event timeouts.
// AttemptTimeoutSeconds 0 leaves only the request-level timeout in place,
// FirstEventTimeoutSeconds 0 disables the first-event wait, MaxAnonymousExits
// 0 uses the built-in cap, and FreeTierBlockSeconds 0 uses the built-in
// cool-down for an exit the free tier flagged.
type PerformanceConfig struct {
	MaxIdleConns             int `json:"max_idle_conns"`
	MaxIdleConnsPerHost      int `json:"max_idle_conns_per_host"`
	MaxConnsPerHost          int `json:"max_conns_per_host"`
	IdleConnTimeoutSeconds   int `json:"idle_conn_timeout_seconds"`
	ConnectTimeoutSeconds    int `json:"connect_timeout_seconds"`
	FailureCooldownSeconds   int `json:"failure_cooldown_seconds"`
	AttemptTimeoutSeconds    int `json:"attempt_timeout_seconds"`
	FirstEventTimeoutSeconds int `json:"first_event_timeout_seconds"`
	// MaxAnonymousExits bounds how many distinct static proxy exits one
	// anonymous request may walk. Each exit may still try the v2 /inference
	// lane and the v1 /zen lane, so the upstream request count can be twice
	// this value; a rotating #pool entry is drawn once per retry.max_attempts
	// instead.
	MaxAnonymousExits int `json:"max_anonymous_exits"`
	// FreeTierBlockSeconds is how long an exit the free tier flagged as
	// abusive stays out of the anonymous cursor. Upstream does not publish
	// the block duration, so the default is a conservative 15 minutes.
	FreeTierBlockSeconds int `json:"free_tier_block_seconds"`
	// ModelTimeouts overrides AttemptTimeoutSeconds for the model IDs it
	// names, in seconds. A model that is not listed keeps the global attempt
	// timeout, so a slow reasoning model can be given a longer budget without
	// loosening the bound for every other model.
	ModelTimeouts map[string]int `json:"model_timeouts,omitempty"`
	// HeartbeatSeconds emits a client-invisible SSE keepalive frame after
	// this many seconds of upstream silence on a committed stream, so
	// intermediaries and client idle watchdogs stop cutting a long reasoning
	// turn. Absent keeps the 15 second default; 0 disables keepalive frames.
	HeartbeatSeconds *int `json:"heartbeat_seconds,omitempty"`
	// IdleTimeoutSeconds ends a committed stream whose upstream produced no
	// bytes for this long: the gateway writes an in-band upstream_idle_timeout
	// error and closes the upstream request instead of waiting out the whole
	// request budget. Absent keeps the 5 minute default; 0 disables it.
	IdleTimeoutSeconds *int `json:"idle_timeout_seconds,omitempty"`
	// CredentialQuarantineSeconds is how long a key the upstream rejected
	// with 401/403 stays out of rotation. The free tier meters per key and
	// egress, so a rejected credential is not a transient fault: re-entering
	// rotation every cooldown keeps a dead key in front of callers and can
	// teach a fronting gateway to ban the channel. Absent keeps the 24 hour
	// default; 0 restores the plain cooldown behavior.
	CredentialQuarantineSeconds *int `json:"credential_quarantine_seconds,omitempty"`
	// MaxInflight bounds concurrent inference turns gateway-wide. Overflow
	// waits up to QueueWaitSeconds, then answers 429 gateway_busy with a
	// Retry-After. 0 (the default) leaves the gateway unbounded.
	MaxInflight int `json:"max_inflight,omitempty"`
	// QueueWaitSeconds is how long an overflow request waits for a slot before
	// it is refused. 0 refuses immediately.
	QueueWaitSeconds int `json:"queue_wait_seconds,omitempty"`
	// PprofEnabled mounts net/http/pprof under /debug/pprof/ on the WebUI
	// listener, behind the same session auth as every other API route. It is
	// a startup-only switch (the mux is built once).
	PprofEnabled bool `json:"pprof_enabled,omitempty"`
	// MemoryLimitBytes sets the Go runtime's soft memory limit
	// (debug.SetMemoryLimit), which makes GC work harder as total memory
	// approaches the ceiling. 0 keeps the runtime default.
	MemoryLimitBytes int64 `json:"memory_limit_bytes,omitempty"`
}

// Defaults for the stream and credential settings. They are separate from the
// JSON fields so an absent field and an explicit 0 stay distinguishable.
const (
	DefaultHeartbeatSeconds            = 15
	DefaultStreamIdleTimeoutSeconds    = 300
	DefaultCredentialQuarantineSeconds = 86400
)

// Heartbeat returns how long a committed stream may stay silent before the
// gateway writes a keepalive frame. A nil setting keeps the default; an
// explicit 0 disables the frames.
func (cfg PerformanceConfig) Heartbeat() time.Duration {
	seconds := DefaultHeartbeatSeconds
	if cfg.HeartbeatSeconds != nil {
		seconds = *cfg.HeartbeatSeconds
	}
	return time.Duration(seconds) * time.Second
}

// StreamIdleTimeout returns the post-commit idle bound. A nil setting keeps
// the default; an explicit 0 disables the watchdog.
func (cfg PerformanceConfig) StreamIdleTimeout() time.Duration {
	seconds := DefaultStreamIdleTimeoutSeconds
	if cfg.IdleTimeoutSeconds != nil {
		seconds = *cfg.IdleTimeoutSeconds
	}
	return time.Duration(seconds) * time.Second
}

// CredentialQuarantine returns how long a key rejected with 401/403 stays out
// of rotation. A nil setting keeps the default; 0 disables the quarantine.
func (cfg PerformanceConfig) CredentialQuarantine() time.Duration {
	seconds := DefaultCredentialQuarantineSeconds
	if cfg.CredentialQuarantineSeconds != nil {
		seconds = *cfg.CredentialQuarantineSeconds
	}
	return time.Duration(seconds) * time.Second
}

// AttemptTimeoutFor returns the per-attempt bound for model: its
// performance.model_timeouts entry when set, otherwise AttemptTimeout. The
// result is capped at requestTimeout exactly like AttemptTimeout.
func (cfg PerformanceConfig) AttemptTimeoutFor(model string, requestTimeout time.Duration) time.Duration {
	if seconds := cfg.ModelTimeouts[model]; seconds > 0 {
		attempt := time.Duration(seconds) * time.Second
		if requestTimeout > 0 && attempt > requestTimeout {
			return requestTimeout
		}
		return attempt
	}
	return cfg.AttemptTimeout(requestTimeout)
}

// AttemptTimeout returns the bound for a single upstream attempt. When
// attempt_timeout_seconds is positive the result is that setting, capped at
// requestTimeout only when requestTimeout is positive. Otherwise the result is
// requestTimeout unchanged, so a config without the setting keeps using the
// request-level retry timeout.
//
// The gateway installs the result on the shared transports as the
// response-header wait. When the setting is positive, the inference attempts
// except /systemone also arm it as a context deadline (the rotation path keeps
// only the header wait, since it has its own first-output timer), covering the
// whole attempt up to the response being released (dial, request write,
// headers and, when enabled, the first SSE event); an established stream
// keeps flowing under the request-level timeout. Without that deadline, one
// hung exit can consume the entire request budget by itself, and the attempts
// that follow are fired against an already-expired context.
func (cfg PerformanceConfig) AttemptTimeout(requestTimeout time.Duration) time.Duration {
	if cfg.AttemptTimeoutSeconds > 0 {
		attempt := time.Duration(cfg.AttemptTimeoutSeconds) * time.Second
		if requestTimeout > 0 && attempt > requestTimeout {
			return requestTimeout
		}
		return attempt
	}
	return requestTimeout
}

// Load reads the config file at path, strips its // and /* */ comments, and
// decodes it over the built-in defaults. Decoding is strict: unknown fields
// and anything after the first JSON value are errors. The result is then
// checked by Normalize.
func Load(path string) (Config, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read %s: %w", path, err)
	}
	data, err = stripJSONComments(data)
	if err != nil {
		return Config{}, fmt.Errorf("parse %s: %w", path, err)
	}
	cfg := Config{
		Listen:      "127.0.0.1:8080",
		Warp:        WarpConfig{PoolSize: 2, Stack: string(warp.DefaultStack)},
		Upstream:    UpstreamConfig{Zen: "https://opencode.ai/zen", Go: "https://opencode.ai/zen/go", Anonymous: "https://opencode.ai/inference"},
		Retry:       RetryConfig{MaxAttempts: 3, TimeoutSeconds: 300},
		Models:      ModelsConfig{RefreshSeconds: 300, QuotaProbeSeconds: 600, Protocols: map[string]string{}},
		Performance: PerformanceConfig{MaxIdleConns: 2048, MaxIdleConnsPerHost: 256, MaxConnsPerHost: 0, IdleConnTimeoutSeconds: 120, ConnectTimeoutSeconds: 5, FailureCooldownSeconds: 15},
		Logging:     LoggingConfig{Level: "info", RingSize: 2000},
		WebUI:       WebUIConfig{Listen: "0.0.0.0:8081", SessionTTLMinutes: 720},
		Prefer:      TierGo,
	}
	dec := json.NewDecoder(bytes.NewReader(data))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&cfg); err != nil {
		return Config{}, fmt.Errorf("parse %s: %w", path, err)
	}
	if err := jsonutil.EnsureEOF(dec); err != nil {
		return Config{}, fmt.Errorf("parse %s: %w", path, err)
	}
	return Normalize(path, cfg)
}

// Normalize resolves external inputs and validates a Config supplied by
// either the JSON file or the authenticated management API. A relative
// proxyfile is resolved against the directory of path.
func Normalize(path string, cfg Config) (Config, error) {
	cfg.Rotation.Defaults()
	if err := cfg.Rotation.Validate(nil); err != nil {
		return Config{}, err
	}
	if err := cfg.Warp.Normalize(); err != nil {
		return Config{}, err
	}
	trimList(&cfg.ServerKeys)
	remapServerKeyMetadata(&cfg)

	trimList(&cfg.ZenKeys)
	trimList(&cfg.GoKeys)
	// A placeholder copied from the documentation is not a credential. Treating
	// it as one routes every free-model failure into a 401 from the keyed tier.
	cfg = DropPlaceholderKeys(cfg)
	cfg.ProxyFile = strings.TrimSpace(cfg.ProxyFile)
	if err := resolveProxyFiles(path, &cfg); err != nil {
		return Config{}, err
	}
	if cfg.Prefer != TierZen && cfg.Prefer != TierGo {
		return Config{}, errors.New("prefer must be \"zen\" or \"go\"")
	}
	if cfg.Listen == "" {
		return Config{}, errors.New("listen must not be empty")
	}
	if cfg.Upstream.PromptEnvironment != nil {
		environment := *cfg.Upstream.PromptEnvironment
		if err := environment.Normalize(); err != nil {
			return Config{}, err
		}
		if environment.IsGitRepo != nil {
			isGitRepo := *environment.IsGitRepo
			environment.IsGitRepo = &isGitRepo
		}
		cfg.Upstream.PromptEnvironment = &environment
	}
	cfg.Upstream.Zen = strings.TrimSpace(cfg.Upstream.Zen)
	cfg.Upstream.Go = strings.TrimSpace(cfg.Upstream.Go)
	cfg.Upstream.Anonymous = strings.TrimSpace(cfg.Upstream.Anonymous)
	if cfg.Upstream.Anonymous == "" {
		cfg.Upstream.Anonymous = "https://opencode.ai/inference"
	}
	for name, raw := range map[string]string{"upstream.zen": cfg.Upstream.Zen, "upstream.go": cfg.Upstream.Go, "upstream.anonymous": cfg.Upstream.Anonymous} {
		u, err := url.Parse(strings.TrimSpace(raw))
		if err != nil || u.Host == "" || (strings.ToLower(u.Scheme) != "http" && strings.ToLower(u.Scheme) != "https") {
			return Config{}, fmt.Errorf("%s must be an http or https URL", name)
		}
	}
	if len(cfg.ServerKeys) == 0 {
		return Config{}, errors.New("server_keys must contain at least one local key")
	}
	if cfg.FirstEnabledServerKey() == "" {
		return Config{}, errors.New("at least one server key must remain enabled")
	}
	if !cfg.Anonymous && len(cfg.ZenKeys) == 0 && len(cfg.GoKeys) == 0 {
		return Config{}, errors.New("zen_keys or go_keys must contain at least one upstream key unless anonymous is enabled")
	}
	if cfg.Retry.MaxAttempts < 1 {
		return Config{}, errors.New("retry.max_attempts must be at least 1")
	}
	if cfg.Retry.TimeoutSeconds < 1 {
		return Config{}, errors.New("retry.timeout_seconds must be at least 1")
	}
	if cfg.Models.RefreshSeconds < 1 {
		return Config{}, errors.New("models.refresh_seconds must be at least 1")
	}
	if cfg.Models.DeadIDs == nil {
		cfg.Models.DeadIDs = slices.Clone(DefaultDeadModels)
	} else {
		dead := make([]string, 0, len(cfg.Models.DeadIDs))
		for _, id := range cfg.Models.DeadIDs {
			if id = strings.TrimSpace(id); id != "" {
				dead = append(dead, id)
			}
		}
		cfg.Models.DeadIDs = dead
	}
	if cfg.Models.QuotaProbeSeconds != 0 && (cfg.Models.QuotaProbeSeconds < 60 || cfg.Models.QuotaProbeSeconds > 86400) {
		return Config{}, errors.New("models.quota_probe_seconds must be 0 (disabled) or between 60 and 86400")
	}
	if cfg.Performance.MaxIdleConns < 1 || cfg.Performance.MaxIdleConnsPerHost < 1 || cfg.Performance.MaxConnsPerHost < 0 || cfg.Performance.IdleConnTimeoutSeconds < 1 || cfg.Performance.ConnectTimeoutSeconds < 1 || cfg.Performance.FailureCooldownSeconds < 1 {
		return Config{}, errors.New("performance values must be positive (max_conns_per_host may be zero for unlimited)")
	}
	if cfg.Performance.AttemptTimeoutSeconds < 0 {
		return Config{}, errors.New("performance.attempt_timeout_seconds must not be negative (0 keeps the retry timeout)")
	}
	if cfg.Performance.FirstEventTimeoutSeconds < 0 || cfg.Performance.FirstEventTimeoutSeconds > 86400 {
		return Config{}, errors.New("performance.first_event_timeout_seconds must be between 0 and 86400 (0 disables the first-event timeout)")
	}
	if cfg.Performance.MaxAnonymousExits < 0 {
		return Config{}, errors.New("performance.max_anonymous_exits must not be negative (0 uses the built-in cap)")
	}
	if cfg.Performance.FreeTierBlockSeconds < 0 {
		return Config{}, errors.New("performance.free_tier_block_seconds must not be negative (0 uses the built-in cool-down)")
	}
	if cfg.Performance.HeartbeatSeconds != nil && (*cfg.Performance.HeartbeatSeconds < 0 || *cfg.Performance.HeartbeatSeconds > 3600) {
		return Config{}, errors.New("performance.heartbeat_seconds must be between 0 and 3600 (0 disables keepalive frames)")
	}
	if cfg.Performance.IdleTimeoutSeconds != nil && (*cfg.Performance.IdleTimeoutSeconds < 0 || *cfg.Performance.IdleTimeoutSeconds > 86400) {
		return Config{}, errors.New("performance.idle_timeout_seconds must be between 0 and 86400 (0 disables the watchdog)")
	}
	if cfg.Performance.CredentialQuarantineSeconds != nil && (*cfg.Performance.CredentialQuarantineSeconds < 0 || *cfg.Performance.CredentialQuarantineSeconds > 2592000) {
		return Config{}, errors.New("performance.credential_quarantine_seconds must be between 0 and 2592000 (0 disables the quarantine)")
	}
	if cfg.Performance.MaxInflight < 0 || cfg.Performance.MaxInflight > 100000 {
		return Config{}, errors.New("performance.max_inflight must be between 0 and 100000 (0 leaves the gateway unbounded)")
	}
	if cfg.Performance.QueueWaitSeconds < 0 || cfg.Performance.QueueWaitSeconds > 300 {
		return Config{}, errors.New("performance.queue_wait_seconds must be between 0 and 300")
	}
	if cfg.Performance.MemoryLimitBytes < 0 {
		return Config{}, errors.New("performance.memory_limit_bytes must not be negative (0 keeps the runtime default)")
	}
	for model, seconds := range cfg.Performance.ModelTimeouts {
		if strings.TrimSpace(model) == "" {
			return Config{}, errors.New("performance.model_timeouts: model IDs must not be empty")
		}
		if seconds < 1 || seconds > 86400 {
			return Config{}, fmt.Errorf("performance.model_timeouts[%s] must be between 1 and 86400 seconds", model)
		}
	}
	for id, meta := range cfg.ServerKeyMetadata {
		if meta.MaxConcurrency < 0 || meta.MaxConcurrency > 256 {
			return Config{}, fmt.Errorf("server_key_metadata[%s].max_concurrency must be between 0 and 256", id)
		}
		if meta.DailyRequests < 0 || meta.DailyRequests > 1000000 {
			return Config{}, fmt.Errorf("server_key_metadata[%s].daily_requests must be between 0 and 1000000", id)
		}
		for _, allowed := range meta.AllowModels {
			if strings.TrimSpace(allowed) == "" {
				return Config{}, fmt.Errorf("server_key_metadata[%s].allow_models: model IDs must not be empty", id)
			}
		}
	}
	if cfg.Logging.Level != "debug" && cfg.Logging.Level != "info" && cfg.Logging.Level != "warn" && cfg.Logging.Level != "error" {
		return Config{}, errors.New("logging.level must be debug, info, warn, or error")
	}
	if cfg.Logging.RingSize < 100 || cfg.Logging.RingSize > 50000 {
		return Config{}, errors.New("logging.ring_size must be between 100 and 50000")
	}
	if cfg.WebUI.Password != "" && len(cfg.WebUI.Password) < 10 {
		return Config{}, errors.New("webui.password must contain at least 10 characters")
	}
	if cfg.WebUI.Enabled {
		cfg.WebUI.Listen = strings.TrimSpace(cfg.WebUI.Listen)
		cfg.WebUI.Username = strings.TrimSpace(cfg.WebUI.Username)
		if cfg.WebUI.Listen == "" {
			return Config{}, errors.New("webui.listen must not be empty when webui is enabled")
		}
		if cfg.WebUI.Username == "" {
			return Config{}, errors.New("webui.username must not be empty when webui is enabled")
		}
		if cfg.WebUI.Password == "" && cfg.WebUI.PasswordHash == "" {
			return Config{}, errors.New("webui.password is required for first-time setup")
		}
		if cfg.WebUI.SessionTTLMinutes < 5 || cfg.WebUI.SessionTTLMinutes > 10080 {
			return Config{}, errors.New("webui.session_ttl_minutes must be between 5 and 10080")
		}
	}
	for _, raw := range cfg.RuntimeProxies() {
		if raw == "direct" {
			continue
		}
		clean, _ := ParseProxyEntry(raw)
		u, err := url.Parse(clean)
		if err != nil || u.Host == "" {
			return Config{}, fmt.Errorf("invalid proxy URL %q", RedactURL(raw))
		}
		switch strings.ToLower(u.Scheme) {
		case "http", "https", "socks5", "socks5h", "relay", "relay+http":
		default:
			return Config{}, fmt.Errorf("unsupported proxy scheme %q", u.Scheme)
		}
	}
	if err := cfg.Models.AnonymousBackoff.Normalize(); err != nil {
		return Config{}, err
	}
	for model, protocol := range cfg.Models.Protocols {
		if model == "" || !wire.Valid(wire.Protocol(protocol)) {
			return Config{}, fmt.Errorf("models.protocols contains invalid mapping %q: %q", model, protocol)
		}
	}
	if err := normalizeContextOverrides(&cfg); err != nil {
		return Config{}, err
	}
	if err := normalizeReasoning(&cfg); err != nil {
		return Config{}, err
	}
	if err := normalizeProxyBindings(&cfg); err != nil {
		return Config{}, err
	}
	if err := normalizePeers(&cfg); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

// normalizeContextOverrides trims the model IDs, rejects limits outside the
// range the gateway can serve and drops entries that override nothing.
func normalizeContextOverrides(cfg *Config) error {
	if len(cfg.Models.ContextOverrides) == 0 {
		cfg.Models.ContextOverrides = nil
		return nil
	}
	if len(cfg.Models.ContextOverrides) > 128 {
		return fmt.Errorf("models.context_overrides lists %d entries, at most 128 are supported", len(cfg.Models.ContextOverrides))
	}
	limits := make(map[string]ModelLimits, len(cfg.Models.ContextOverrides))
	for raw, limit := range cfg.Models.ContextOverrides {
		model := strings.TrimSpace(raw)
		if model == "" {
			return errors.New("models.context_overrides: model IDs must not be empty")
		}
		if limit.Context < 0 || limit.Context > 10000000 || limit.Input < 0 || limit.Input > 10000000 || limit.Output < 0 || limit.Output > 1000000 {
			return fmt.Errorf("models.context_overrides[%s] limits are out of range", model)
		}
		if limit.Context == 0 && limit.Input == 0 && limit.Output == 0 {
			return fmt.Errorf("models.context_overrides[%s] overrides nothing", model)
		}
		limits[model] = limit
	}
	cfg.Models.ContextOverrides = limits
	return nil
}

// normalizePeers trims and defaults the peer list, then rejects duplicate names
// because logs and the X-Peer header identify a peer by name alone.
func normalizePeers(cfg *Config) error {
	if len(cfg.Peers) == 0 {
		cfg.Peers = nil
		return nil
	}
	if len(cfg.Peers) > 32 {
		return fmt.Errorf("peers lists %d entries, at most 32 are supported", len(cfg.Peers))
	}
	seen := make(map[string]bool, len(cfg.Peers))
	for index := range cfg.Peers {
		peer := &cfg.Peers[index]
		peer.Normalize()
		if peer.Name == "" {
			peer.Name = peer.BaseURL
		}
		if seen[peer.Name] {
			return fmt.Errorf("peers[%d].name %q is used twice", index, peer.Name)
		}
		seen[peer.Name] = true
		if err := peer.Validate(index); err != nil {
			return err
		}
	}
	return nil
}

func remapServerKeyMetadata(cfg *Config) {
	if len(cfg.ServerKeyMetadata) == 0 {
		return
	}
	for _, key := range cfg.ServerKeys {
		oldID := Fingerprint(key)[:10]
		metadata, exists := cfg.ServerKeyMetadata[oldID]
		if !exists {
			continue
		}
		newID := Fingerprint(key)
		if _, alreadyPresent := cfg.ServerKeyMetadata[newID]; !alreadyPresent {
			cfg.ServerKeyMetadata[newID] = metadata
		}
		delete(cfg.ServerKeyMetadata, oldID)
	}
}

// normalizeReasoning validates and canonicalizes the forced thinking level.
//
// Levels are stored lowercased so the hot path can compare them without
// allocating. An empty value means "not forced", which is the default and keeps
// the feature absent for existing configurations.
func normalizeReasoning(cfg *Config) error {
	cfg.Reasoning.Effort = strings.ToLower(strings.TrimSpace(cfg.Reasoning.Effort))
	if err := validateEffort("reasoning.effort", cfg.Reasoning.Effort); err != nil {
		return err
	}
	if cfg.Reasoning.EffortByModel == nil {
		return nil
	}
	// An empty map is the same as an absent one, and writing nil back keeps a
	// saved configuration free of a field the user never set.
	if len(cfg.Reasoning.EffortByModel) == 0 {
		cfg.Reasoning.EffortByModel = nil
		return nil
	}
	normalized := make(map[string]string, len(cfg.Reasoning.EffortByModel))
	for model, effort := range cfg.Reasoning.EffortByModel {
		key := strings.TrimSpace(model)
		if key == "" {
			return errors.New("reasoning.effort_by_model must not contain an empty model ID")
		}
		level := strings.ToLower(strings.TrimSpace(effort))
		if err := validateEffort(fmt.Sprintf("reasoning.effort_by_model[%q]", key), level); err != nil {
			return err
		}
		normalized[key] = level
	}
	cfg.Reasoning.EffortByModel = normalized
	return nil
}

// validateEffort accepts the levels the OpenAI and Anthropic surfaces define
// (minimal through max), the gateway controls "auto" and "none", and an empty
// string, which means "not forced".
func validateEffort(name, value string) error {
	switch value {
	case "", "auto", "minimal", "low", "medium", "high", "xhigh", "max", "none":
		return nil
	default:
		return fmt.Errorf("%s must be one of auto, minimal, low, medium, high, xhigh, max, none, or empty to disable the override", name)
	}
}

// ForcedEffort returns the level the operator forced for a model, or an empty
// string when none applies. A per-model entry, even an empty one, takes
// precedence over the default. A stored level is already lowercase.
func (cfg Config) ForcedEffort(model string) string {
	if effort, ok := cfg.Reasoning.EffortByModel[model]; ok {
		return effort
	}
	return cfg.Reasoning.Effort
}

// RuntimeProxies returns the resolved proxy list, including proxyfile entries,
// or ["direct"] when there are none. Config.Proxies intentionally remains the
// list stored in config.json so a save never duplicates values loaded from
// proxyfile.
func (cfg Config) RuntimeProxies() []string {
	if len(cfg.effectiveProxies) > 0 {
		return cfg.effectiveProxies
	}
	if len(cfg.Proxies) > 0 {
		return cfg.Proxies
	}
	return []string{"direct"}
}

// PasswordForSave clears the resolved proxy list (the proxies plus proxyfile
// entries that RuntimeProxies returns), leaving only Config.Proxies; despite
// its name it does not touch the WebUI password. SaveAtomic calls it on its
// own copy before encoding. The field is unexported and so never encoded
// anyway, which makes the call a guard rather than a requirement.
func (cfg *Config) PasswordForSave() {
	cfg.effectiveProxies = nil
}

// Clone returns a copy of cfg whose slices and maps, including the rotation
// orders and the resolved proxy list, no longer share storage with cfg.
func Clone(cfg Config) Config {
	cfg.Rotation.SOTA.Order = append([]string(nil), cfg.Rotation.SOTA.Order...)
	cfg.Rotation.Sweet.Order = append([]string(nil), cfg.Rotation.Sweet.Order...)
	cfg.ServerKeys = append([]string(nil), cfg.ServerKeys...)
	metadata := make(map[string]ServerKeyMetadata, len(cfg.ServerKeyMetadata))
	for id, item := range cfg.ServerKeyMetadata {
		item.AllowModels = append([]string(nil), item.AllowModels...)
		metadata[id] = item
	}
	cfg.ServerKeyMetadata = metadata
	cfg.ZenKeys = append([]string(nil), cfg.ZenKeys...)
	cfg.GoKeys = append([]string(nil), cfg.GoKeys...)
	cfg.Proxies = append([]string(nil), cfg.Proxies...)
	cfg.effectiveProxies = append([]string(nil), cfg.effectiveProxies...)
	cfg.Models.DeadIDs = slices.Clone(cfg.Models.DeadIDs)
	if cfg.Upstream.PromptEnvironment != nil {
		environment := *cfg.Upstream.PromptEnvironment
		if environment.IsGitRepo != nil {
			isGitRepo := *environment.IsGitRepo
			environment.IsGitRepo = &isGitRepo
		}
		cfg.Upstream.PromptEnvironment = &environment
	}
	if cfg.Models.Protocols != nil {
		protocols := make(map[string]string, len(cfg.Models.Protocols))
		for key, value := range cfg.Models.Protocols {
			protocols[key] = value
		}
		cfg.Models.Protocols = protocols
	}
	if cfg.Models.ContextOverrides != nil {
		limits := make(map[string]ModelLimits, len(cfg.Models.ContextOverrides))
		for model, limit := range cfg.Models.ContextOverrides {
			limits[model] = limit
		}
		cfg.Models.ContextOverrides = limits
	}
	if cfg.Reasoning.EffortByModel != nil {
		efforts := make(map[string]string, len(cfg.Reasoning.EffortByModel))
		for model, effort := range cfg.Reasoning.EffortByModel {
			efforts[model] = effort
		}
		cfg.Reasoning.EffortByModel = efforts
	}
	if cfg.ProxyBindings != nil {
		bindings := make(map[string]string, len(cfg.ProxyBindings))
		for k, v := range cfg.ProxyBindings {
			bindings[k] = v
		}
		cfg.ProxyBindings = bindings
	}
	peers := make([]PeerConfig, len(cfg.Peers))
	for index, peer := range cfg.Peers {
		peer.Models = append([]string(nil), peer.Models...)
		peers[index] = peer
	}
	cfg.Peers = peers
	return cfg
}

// normalizeProxyBindings validates and canonicalizes explicit per-key proxy
// bindings. Keys are mapped to their 16-hex fingerprint so secrets are never
// stored twice, and target proxies must match a valid runtime proxy (or "direct").
// Bound credentials fail closed: when their assigned proxy becomes unavailable,
// they do not migrate to another route.
//
// NOTE: Binding to a proxy route with a #pool fragment or an external rotator
// does not guarantee a static public egress IP; it pins traffic to that proxy transport.
func normalizeProxyBindings(cfg *Config) error {
	if len(cfg.ProxyBindings) == 0 {
		return nil
	}
	keyMap := make(map[string]string, len(cfg.ZenKeys)+len(cfg.GoKeys))
	for _, k := range cfg.ZenKeys {
		fp := Fingerprint(k)
		keyMap[k] = fp
		keyMap[fp] = fp
	}
	for _, k := range cfg.GoKeys {
		fp := Fingerprint(k)
		keyMap[k] = fp
		keyMap[fp] = fp
	}

	knownProxies := make(map[string]string)
	for _, raw := range cfg.RuntimeProxies() {
		clean, _ := ParseProxyEntry(raw)
		knownProxies[raw] = raw
		knownProxies[clean] = raw
		if clean == "direct" {
			knownProxies["direct"] = "direct"
		}
	}

	normalized := make(map[string]string, len(cfg.ProxyBindings))
	for keyRef, proxyRef := range cfg.ProxyBindings {
		trimmedKey := strings.TrimSpace(keyRef)
		trimmedProxy := strings.TrimSpace(proxyRef)
		fp, ok := keyMap[trimmedKey]
		if !ok {
			return fmt.Errorf("proxy_bindings contains reference to unknown key %q", MaskValue(trimmedKey))
		}
		canonicalProxy, ok := knownProxies[trimmedProxy]
		if !ok {
			return fmt.Errorf("proxy_bindings references unknown proxy %q", RedactURL(trimmedProxy))
		}
		normalized[fp] = canonicalProxy
	}
	cfg.ProxyBindings = normalized
	return nil
}
