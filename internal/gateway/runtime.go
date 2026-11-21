package gateway

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"net/netip"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"opencode2api/internal/agentcfg"
	"opencode2api/internal/config"
	modelcatalog "opencode2api/internal/models"
	"opencode2api/internal/protocol"
	"opencode2api/internal/rotation"
	"opencode2api/internal/telemetry"
	"opencode2api/internal/warp"
)

// gatewayRuntime is one applied configuration: the Gateway built from it, that
// gateway's HTTP handler, the availability registry it uses, and cancel,
// which stops the gateway's background proxy checks and model refresh.
type gatewayRuntime struct {
	availability *modelcatalog.AvailabilityRegistry
	config       config.Config
	gateway      *Gateway
	handler      http.Handler
	cancel       context.CancelFunc
	// warpPool is the userspace WARP pool this runtime uses, if any. warpKey
	// identifies its size and stack; an unchanged key is reused across
	// configuration reloads. warpCreated marks a pool this build owns and
	// must close if the build is discarded.
	warpPool    *warp.Pool
	warpKey     string
	warpCreated bool
}

// ApplyResult reports whether a configuration was applied and which listener
// fields only take effect after a restart.
type ApplyResult struct {
	Applied         bool     `json:"applied"`
	RestartRequired bool     `json:"restart_required"`
	RestartFields   []string `json:"restart_fields,omitempty"`
}

// RuntimeController owns the running gateway and swaps in a freshly built one
// whenever a configuration is applied; each request is served by the runtime
// that was current when it arrived. It also owns the state that outlives a
// runtime: the persisted rotation manager, the pricing metadata, the
// free-model availability checks and the listeners bound at startup.
type RuntimeController struct {
	rotation           *rotation.Manager
	configPath         string
	root               context.Context
	logger             *slog.Logger
	monitor            *telemetry.Metrics
	hub                *telemetry.LogBroker
	redactor           *config.Redactor
	level              *slog.LevelVar
	current            atomic.Pointer[gatewayRuntime]
	updateMu           sync.Mutex
	effective          EffectiveListeners
	metadata           *modelcatalog.PricingRegistry
	availabilityCancel context.CancelFunc
	// availabilitySweep wakes the probe loop ahead of its minute tick; the
	// capacity-one buffer coalesces requests while a sweep is in flight.
	availabilitySweep chan struct{}
	// warpPool is the pool shared by successive runtimes when the WARP size
	// and stack are unchanged. warpKey is the identity used for that reuse.
	warpPool      *warp.Pool
	warpKey       string
	usage         *telemetry.UsageStore
	contextLimits *modelcatalog.ContextLimits
}

// EffectiveListeners are the listener settings the running process actually
// bound at startup. They only change after a restart.
type EffectiveListeners struct {
	API          string
	WebUI        string
	WebUIEnabled bool
}

// RuntimeManager builds the RuntimeController for cfg and starts its first
// runtime. Along the way it migrates a plaintext webui.password to a hash,
// rewriting the config file and deleting the backup that held the plaintext;
// loads the persisted rotation state and the model catalog cache; and starts
// the pricing metadata refresh and the free-model availability checks. It
// fails when the rotation config does not validate against the catalog.
func RuntimeManager(root context.Context, configPath string, cfg config.Config, logger *slog.Logger, monitor *telemetry.Metrics, hub *telemetry.LogBroker, redactor *config.Redactor, level *slog.LevelVar) (manager *RuntimeController, err error) {
	manager = &RuntimeController{
		configPath: configPath, root: root, logger: logger, monitor: monitor, hub: hub, redactor: redactor, level: level,
		effective: EffectiveListeners{API: cfg.Listen, WebUI: cfg.WebUI.Listen, WebUIEnabled: cfg.WebUI.Enabled},
	}
	usage, err := telemetry.OpenUsageStore(cfg.Storage.UsageSQLite, logger)
	if err != nil {
		return nil, err
	}
	if usage != nil {
		manager.usage = usage
		monitor.SetRequestSink(usage.Append)
		defer func() {
			if err != nil {
				monitor.SetRequestSink(nil)
				usage.Close()
			}
		}()
	}
	contextLimits, contextErr := modelcatalog.OpenContextLimits(modelcatalog.ContextLimitStatePath(configPath))
	if contextErr != nil && logger != nil {
		logger.Warn("context limit observations could not be loaded; starting empty", "component", "models", "event", "context_limits_load_failed", "path", modelcatalog.ContextLimitStatePath(configPath), "error", contextErr)
	}
	manager.contextLimits = contextLimits
	manager.metadata = modelcatalog.PricingStore(configPath, logger)
	// OpenCode metadata refreshes ride the active runtime's healthy proxy
	// transports and fall back to the registry's direct client when none are
	// available.
	manager.metadata.SetClientProvider(func() []*http.Client {
		runtime := manager.current.Load()
		if runtime == nil || runtime.gateway == nil || runtime.gateway.transports == nil {
			return nil
		}
		clients := make([]*http.Client, 0, len(runtime.gateway.transports.items))
		for _, proxy := range runtime.gateway.transports.items {
			if proxy != nil && proxy.healthy.Load() {
				clients = append(clients, proxy.client)
			}
		}
		return clients
	})
	if cfg.WebUI.Password != "" {
		hash, err := config.HashPassword(cfg.WebUI.Password)
		if err != nil {
			return nil, fmt.Errorf("hash webui password: %w", err)
		}
		cfg.WebUI.PasswordHash = hash
		cfg.WebUI.Password = ""
		if err := config.SaveAtomic(configPath, cfg); err != nil {
			return nil, fmt.Errorf("persist webui password migration: %w", err)
		}
		// The automatically-created backup contains the one-time plaintext
		// bootstrap password and must not be retained.
		_ = os.Remove(configPath + ".bak")
	}
	cfg.Rotation.Defaults()
	rotationManager, err := rotation.New(configPath+".rotation.json", cfg.Rotation)
	if err != nil {
		return nil, err
	}
	manager.rotation = rotationManager
	// Per-key daily budgets live outside the runtime so a hot reload keeps
	// them, and on disk so a process restart does not reset them.
	quotaPath, egressQuotaPath, historyPath := "", "", ""
	if configPath != "" {
		quotaPath = configPath + ".quota.json"
		egressQuotaPath = configPath + ".egress-quota.json"
		historyPath = configPath + ".usage-history.json"
	}
	ConfigureQuotaState(quotaPath)
	ConfigureEgressQuota(egressQuotaPath)
	ConfigureUsageHistory(historyPath)
	runtime, err := manager.build(cfg)
	if err != nil {
		return nil, err
	}
	if err := runtime.gateway.catalog.LoadCache(modelcatalog.CatalogCachePath(configPath)); err != nil && !os.IsNotExist(err) {
		if manager.logger != nil {
			manager.logger.Warn("model catalog cache ignored", "component", "models", "event", "catalog_cache_load_failed", "path", modelcatalog.CatalogCachePath(configPath), "error", err)
		}
	}
	if err := cfg.Rotation.Validate(runtime.gateway.catalog.List()); err != nil {
		if runtime.warpCreated && runtime.warpPool != nil {
			runtime.warpPool.Close()
		}
		return nil, err
	}
	runtime.gateway.syncRotation()
	manager.current.Store(runtime)
	manager.warpPool, manager.warpKey = runtime.warpPool, runtime.warpKey
	manager.redactor.Replace(cfg)
	telemetry.SetLogLevel(manager.level, cfg.Logging.Level)
	manager.start(runtime)
	manager.metadata.Start(root)
	manager.startAvailabilityChecks()
	return manager, nil
}

// migrateAvailabilityPath moves the legacy 40-bit state file to its 64-bit
// fingerprint path when no file at the new path already exists. A new-format
// file takes precedence.

func migrateAvailabilityPath(legacyPath, currentPath string) error {
	if legacyPath == "" || currentPath == "" || legacyPath == currentPath {
		return nil
	}
	if _, err := os.Stat(currentPath); err == nil {
		return nil
	} else if !os.IsNotExist(err) {
		return err
	}
	if _, err := os.Stat(legacyPath); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	return os.Rename(legacyPath, currentPath)
}

// build constructs a runtime for cfg without starting it. The runtime shares
// the controller's rotation manager and pricing metadata, and keeps the
// current runtime's availability registry while the Zen upstream is
// unchanged; otherwise it uses the registry persisted for the new upstream.
func (m *RuntimeController) build(cfg config.Config) (*gatewayRuntime, error) {
	warpPool, warpKey, warpCreated, err := m.selectWARP(cfg)
	if err != nil {
		return nil, err
	}
	discardWARP := func() {
		if warpCreated && warpPool != nil {
			warpPool.Close()
		}
	}
	gateway, err := New(cfg, m.logger, m.monitor, warpPool)
	if err != nil {
		discardWARP()
		return nil, err
	}
	gateway.rotation = m.rotation
	availabilityPath := ""
	if m.configPath != "" {
		availabilityPath = m.configPath + "." + config.Fingerprint(cfg.Upstream.Zen) + ".availability.json"
		if err := migrateAvailabilityPath(m.configPath+"."+config.Fingerprint(cfg.Upstream.Zen)[:10]+".availability.json", availabilityPath); err != nil {
			discardWARP()
			return nil, fmt.Errorf("migrate model availability: %w", err)
		}
	}
	availability, err := modelcatalog.AvailabilityStore(availabilityPath)
	if err != nil {
		discardWARP()
		return nil, fmt.Errorf("load model availability: %w", err)
	}
	if current := m.current.Load(); current != nil && current.config.Upstream.Zen == cfg.Upstream.Zen {
		availability = current.availability
	}
	gateway.catalog.SetPricingStore(m.metadata)
	gateway.catalog.SetAvailabilityStore(availability)
	gateway.availability = availability
	if m.contextLimits != nil {
		gateway.contextLimits = m.contextLimits
	}
	gateway.catalog.SetCachePath(modelcatalog.CatalogCachePath(m.configPath))
	return &gatewayRuntime{config: cfg, gateway: gateway, availability: availability, handler: gateway.Handler(), cancel: func() {}, warpPool: warpPool, warpKey: warpKey, warpCreated: warpCreated}, nil
}

// selectWARP returns the WARP pool a runtime built for cfg must use. A pool
// with the same size, stack and outer source prefix is shared across runtimes;
// creating a new one registers its first exit synchronously, so enabling WARP
// fails closed when Cloudflare cannot be reached. A disabled configuration
// returns no pool.
func (m *RuntimeController) selectWARP(cfg config.Config) (*warp.Pool, string, bool, error) {
	if !cfg.Warp.Enabled {
		return nil, "", false, nil
	}
	key := fmt.Sprintf("%d|%s|%s|%d|%d", cfg.Warp.PoolSize, cfg.Warp.Stack, cfg.Warp.SourceIPv6, cfg.Warp.RotateAfterMinutes, cfg.Warp.RotateCooldownSeconds)
	if m.warpPool != nil && m.warpKey == key {
		return m.warpPool, key, false, nil
	}
	var sources []netip.Prefix
	if cfg.Warp.SourceIPv6 != "" {
		for _, part := range strings.Split(cfg.Warp.SourceIPv6, ",") {
			part = strings.TrimSpace(part)
			if part == "" {
				continue
			}
			prefix, err := netip.ParsePrefix(part)
			if err != nil {
				return nil, "", false, fmt.Errorf("initialize WARP egress: warp.source_ipv6: %w", err)
			}
			sources = append(sources, prefix.Masked())
		}
	}
	var source netip.Prefix
	if len(sources) > 0 {
		source = sources[0]
	}
	timeout := time.Duration(cfg.Retry.TimeoutSeconds) * time.Second
	if timeout <= 0 || timeout > 2*time.Minute {
		timeout = 2 * time.Minute
	}
	// The first registration is synchronous: it decides whether the runtime can
	// serve free traffic at all. Cloudflare can answer it with a rate limit even
	// on a fresh process, so give initialization room for one full window
	// instead of failing startup on a pause that is about to end.
	if timeout < warp.RegistrationWindow+30*time.Second {
		timeout = warp.RegistrationWindow + 30*time.Second
	}
	ctx, cancel := context.WithTimeout(m.root, timeout)
	defer cancel()
	statePath := ""
	if m.configPath != "" {
		statePath = m.configPath + ".warp-state.json"
	}
	// Every exit must answer one minimal free-lane prompt before the pool
	// rotates traffic onto it: a tunnel that only reaches a trace endpoint can
	// still be refused by the gateway, and admitting it costs a real request.
	probe := warp.LaneProbe{}
	if cfg.Anonymous {
		probe = warp.LaneProbe{
			URL:   strings.TrimRight(cfg.Upstream.Anonymous, "/") + "/v1/chat/completions",
			Model: warp.DefaultLaneProbeModel,
			// The gate judges the system prompt (it wants an env block with the
			// working directory), so the admission request must carry the same
			// prompt a real free-lane request does or it is refused for shape.
			SystemPrompt: opencodeSystemPrompt(warp.DefaultLaneProbeModel, false, cfg.Upstream.PromptEnvironment),
		}
	}
	pool, err := warp.NewWithOptions(ctx, m.logger, warp.Options{
		Size:              cfg.Warp.PoolSize,
		Stack:             warp.Stack(cfg.Warp.Stack),
		Preload:           cfg.Warp.Preload,
		RequireIPv6Egress: cfg.Warp.RequireIPv6Egress,
		SourceIPv6:        source,
		Sources:           sources,
		StatePath:         statePath,
		LaneProbe:         probe,
		MaxAge:            time.Duration(cfg.Warp.RotateAfterMinutes) * time.Minute,
		RotateCooldown:    time.Duration(cfg.Warp.RotateCooldownSeconds) * time.Second,
		RotateKeys:        cfg.Warp.RotateKeys,
	})
	if err != nil {
		return nil, "", false, fmt.Errorf("initialize WARP egress: %w", err)
	}
	return pool, key, true, nil
}

// start launches runtime's proxy health checks and model refresh, and stores
// the func that stops them in runtime.cancel.
func (m *RuntimeController) start(runtime *gatewayRuntime) {
	// The loops run under their own context derived from the controller's
	// root, not under any request's: cancel stops catalog refresh and proxy
	// checks while in-flight HTTP requests continue on the old pools.
	runtimeCtx, cancel := context.WithCancel(m.root)
	runtime.cancel = cancel
	runtime.gateway.StartProxyHealthChecks(runtimeCtx)
	runtime.gateway.StartModelRefresh(runtimeCtx)
	runtime.gateway.StartQuotaProbe(runtimeCtx)
}

// Handler returns the API handler, wrapped in the telemetry middleware. Each
// request is served by the runtime current when it arrives, so an applied
// configuration reaches new requests without a restart; before the first
// runtime exists the handler answers 503.
func (m *RuntimeController) Handler() http.Handler {
	dynamic := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		runtime := m.current.Load()
		if runtime == nil {
			http.Error(w, "service unavailable", http.StatusServiceUnavailable)
			return
		}
		runtime.handler.ServeHTTP(w, r)
	})
	return telemetry.Middleware(m.monitor, m.logger, dynamic)
}

// Redact removes configured credentials from diagnostic output.
func (m *RuntimeController) Redact(value string) string {
	return m.redactor.String(value)
}

// Config returns a copy of the configuration the current runtime was built
// from, or the zero Config when no runtime exists.
func (m *RuntimeController) Config() config.Config {
	runtime := m.current.Load()
	if runtime == nil {
		return config.Config{}
	}
	return config.Clone(runtime.config)
}

// RestartStatus returns the listeners the process bound at startup and the
// config fields (listen, webui.listen, webui.enabled) whose current value
// differs from them, i.e. the changes waiting for a restart.
func (m *RuntimeController) RestartStatus() (EffectiveListeners, []string) {
	cfg := m.Config()
	fields := make([]string, 0, 3)
	if cfg.Listen != m.effective.API {
		fields = append(fields, "listen")
	}
	if cfg.WebUI.Listen != m.effective.WebUI {
		fields = append(fields, "webui.listen")
	}
	if cfg.WebUI.Enabled != m.effective.WebUIEnabled {
		fields = append(fields, "webui.enabled")
	}
	return m.effective, fields
}

// Apply normalizes and validates candidate, builds a runtime from it and
// swaps it in, stopping the previous runtime's background loops. It saves the
// config file when persist is set or candidate carried a plaintext WebUI
// password, which is hashed first.
func (m *RuntimeController) Apply(candidate config.Config, persist bool) (ApplyResult, error) {
	m.updateMu.Lock()
	defer m.updateMu.Unlock()
	return m.applyLocked(candidate, persist)
}

// Update serializes read-modify-write operations against the latest config.
// The changed config is applied and always saved.
func (m *RuntimeController) Update(change func(*config.Config) error) (ApplyResult, error) {
	m.updateMu.Lock()
	defer m.updateMu.Unlock()
	candidate := m.Config()
	if err := change(&candidate); err != nil {
		return ApplyResult{}, err
	}
	return m.applyLocked(candidate, true)
}

// applyLocked does the work of Apply and Update; the caller must hold
// updateMu. Nothing changes until candidate has been normalized, built,
// validated and, when required, saved. The new runtime inherits the current
// catalog state and is started before it is swapped in.
func (m *RuntimeController) applyLocked(candidate config.Config, persist bool) (ApplyResult, error) {

	current := m.current.Load()
	hadPlaintextPassword := candidate.WebUI.Password != ""
	if hadPlaintextPassword {
		hash, err := config.HashPassword(candidate.WebUI.Password)
		if err != nil {
			return ApplyResult{}, err
		}
		candidate.WebUI.PasswordHash = hash
		candidate.WebUI.Password = ""
	}
	normalized, err := config.Normalize(m.configPath, candidate)
	if err != nil {
		return ApplyResult{}, err
	}
	next, err := m.build(normalized)
	if err != nil {
		return ApplyResult{}, fmt.Errorf("initialize runtime: %w", err)
	}
	if current != nil {
		next.gateway.catalog.CopyState(current.gateway.catalog)
	}
	if err := normalized.Rotation.Validate(next.gateway.catalog.List()); err != nil {
		if next.warpCreated && next.warpPool != nil {
			next.warpPool.Close()
		}
		return ApplyResult{}, err
	}
	if persist || hadPlaintextPassword {
		if err := config.SaveAtomic(m.configPath, normalized); err != nil {
			next.cancel()
			if next.warpCreated && next.warpPool != nil {
				next.warpPool.Close()
			}
			return ApplyResult{}, err
		}
	}

	result := ApplyResult{Applied: true}
	if normalized.Listen != m.effective.API {
		result.RestartFields = append(result.RestartFields, "listen")
	}
	if normalized.WebUI.Listen != m.effective.WebUI {
		result.RestartFields = append(result.RestartFields, "webui.listen")
	}
	if normalized.WebUI.Enabled != m.effective.WebUIEnabled {
		result.RestartFields = append(result.RestartFields, "webui.enabled")
	}
	result.RestartRequired = len(result.RestartFields) > 0
	m.redactor.Replace(normalized)
	telemetry.SetLogLevel(m.level, normalized.Logging.Level)
	if current == nil || normalized.Logging.RingSize != current.config.Logging.RingSize {
		m.hub.Resize(normalized.Logging.RingSize)
	}
	m.rotation.Configure(normalized.Rotation)
	next.gateway.syncRotation()
	m.start(next)
	previous := m.current.Swap(next)
	m.warpPool, m.warpKey = next.warpPool, next.warpKey
	if previous != nil {
		previous.cancel()
		if previous.warpPool != nil && previous.warpPool != next.warpPool {
			// Requests that loaded the previous runtime may still acquire a
			// lease. Close the old pool only after the longest request budget
			// has passed; leases already held keep their exits alive.
			grace := time.Duration(normalized.Retry.TimeoutSeconds) * time.Second
			if grace <= 0 || grace > 5*time.Minute {
				grace = 5 * time.Minute
			}
			retired := previous.warpPool
			time.AfterFunc(grace, retired.Close)
		}
	}
	m.logger.Info("configuration applied", "component", "config", "event", "config_applied", "restart_required", result.RestartRequired, "restart_fields", result.RestartFields)
	return result, nil
}

// ConfigPath returns the path of the configuration file this controller owns.
func (m *RuntimeController) ConfigPath() string { return m.configPath }

// Reload re-reads the config file and applies it without rewriting it, unless
// it holds a plaintext WebUI password: that is hashed and saved, and the
// backup holding the plaintext is removed.
func (m *RuntimeController) Reload() (ApplyResult, error) {
	cfg, err := config.Load(m.configPath)
	if err != nil {
		return ApplyResult{}, err
	}
	hadPlaintextPassword := cfg.WebUI.Password != ""
	result, err := m.Apply(cfg, false)
	if err == nil && hadPlaintextPassword {
		_ = os.Remove(m.configPath + ".bak")
	}
	return result, err
}

// Shutdown stops the free-model availability checks and the current runtime's
// background loops.
func (m *RuntimeController) Shutdown() {
	SaveQuotaState()
	SaveEgressQuota()
	SaveUsageHistory()
	if m.rotation != nil {
		m.rotation.Save()
	}
	if m.contextLimits != nil {
		m.contextLimits.Save()
	}
	if m.availabilityCancel != nil {
		m.availabilityCancel()
	}
	if current := m.current.Load(); current != nil {
		current.cancel()
	}
	if m.warpPool != nil {
		m.warpPool.Close()
		m.warpPool = nil
		m.warpKey = ""
	}
	if m.usage != nil {
		m.monitor.SetRequestSink(nil)
		m.usage.Close()
		m.usage = nil
	}
}

// ResourceSnapshot is the operator view of the current runtime's models, keys,
// proxies and model metadata (see Resources).
type ResourceSnapshot struct {
	Models    modelcatalog.CatalogSnapshot  `json:"models"`
	Keys      []KeyStatus                   `json:"keys"`
	Proxies   []ProxyStatus                 `json:"proxies"`
	Anonymous bool                          `json:"anonymous"`
	Warp      *WarpStatus                   `json:"warp,omitempty"`
	Peers     []map[string]any              `json:"peers,omitempty"`
	Metadata  modelcatalog.MetadataSnapshot `json:"metadata"`
}

// WarpStatus is the operator view of the WARP egress pool.
type WarpStatus struct {
	Enabled         bool             `json:"enabled"`
	Stack           string           `json:"stack"`
	Size            int              `json:"size"`
	Active          int              `json:"active"`
	Standby         int              `json:"standby"`
	Cooling         int              `json:"cooling"`
	CoolingPrefixes int              `json:"cooling_prefixes"`
	LaneGated       bool             `json:"lane_gated"`
	LaneUntil       *time.Time       `json:"lane_until,omitempty"`
	LaneRefusals    int              `json:"lane_refusals"`
	Egress          []string         `json:"egress,omitempty"`
	Exits           []warp.ExitState `json:"exits,omitempty"`
}

// KeyStatus describes one upstream key by its display ID (its last five
// characters): its tier, its current proxy binding, its consecutive failures
// and any active cooldown.
type KeyStatus struct {
	ID               string     `json:"id"`
	Tier             string     `json:"tier"`
	Index            int        `json:"index"`
	ProxyIndex       int        `json:"proxy_index"`
	Bound            bool       `json:"bound,omitempty"`
	Failures         uint32     `json:"failures"`
	CooldownUntil    *time.Time `json:"cooldown_until,omitempty"`
	QuarantinedUntil *time.Time `json:"quarantined_until,omitempty"`
}

// ProxyStatus describes one proxy route: its redacted address, its health,
// whether a health check is in flight, and how many Zen and Go keys are bound
// to it. Anonymous reports whether the anonymous lane, which uses every
// proxy, is enabled.
type ProxyStatus struct {
	Index        int        `json:"index"`
	Address      string     `json:"address"`
	Healthy      bool       `json:"healthy"`
	Checking     bool       `json:"checking"`
	Pool         bool       `json:"pool,omitempty"`
	Relay        bool       `json:"relay,omitempty"`
	RelaySpent   int        `json:"relay_spent_exits,omitempty"`
	LastChecked  *time.Time `json:"last_checked,omitempty"`
	BlockedUntil *time.Time `json:"blocked_until,omitempty"`
	// LastError is why the last health check failed; Listening says a loopback
	// proxy listener still accepts connections. Together they separate "the
	// tunnel runs but carries no traffic" from "nothing is running".
	LastError string `json:"last_error,omitempty"`
	Listening bool   `json:"listening,omitempty"`
	ZenKeys   int    `json:"zen_keys"`
	GoKeys    int    `json:"go_keys"`
	Anonymous bool   `json:"anonymous"`
}

// liveLoopbackProxy reports whether a loopback proxy URL still has a listener.
// sing-box keeps a SOCKS port open even when its tunnel to the exit is dead,
// which is the "running but not usable" state the console names separately.
func liveLoopbackProxy(raw string) bool {
	trimmed := raw
	index := strings.Index(trimmed, "://")
	if index < 0 {
		return false
	}
	trimmed = trimmed[index+3:]
	if index = strings.IndexAny(trimmed, "/?#"); index >= 0 {
		trimmed = trimmed[:index]
	}
	if index = strings.LastIndex(trimmed, "@"); index >= 0 {
		trimmed = trimmed[index+1:]
	}
	address, err := netip.ParseAddrPort(trimmed)
	if err != nil || !address.Addr().IsLoopback() {
		return false
	}
	conn, err := net.DialTimeout("tcp", address.String(), 250*time.Millisecond)
	if err != nil {
		return false
	}
	conn.Close()
	return true
}

// Resources reports the current runtime's servable models, per-key state and
// per-proxy health and key bindings, or an empty snapshot when no runtime
// exists.
func (m *RuntimeController) Resources() ResourceSnapshot {
	runtime := m.current.Load()
	if runtime == nil {
		return ResourceSnapshot{}
	}
	gateway := runtime.gateway
	_, models := gateway.availableModels()
	result := ResourceSnapshot{Models: models, Anonymous: gateway.cfg.Anonymous}
	result.Metadata = gateway.catalog.MetadataSnapshot()
	result.Keys = append(result.Keys, keyStatuses("zen", gateway.zenNodes)...)
	result.Keys = append(result.Keys, keyStatuses("go", gateway.goNodes)...)
	gateway.zenNodes.bindingsMu.Lock()
	zenBindings := append([]int(nil), gateway.zenNodes.bindingCount...)
	gateway.zenNodes.bindingsMu.Unlock()
	gateway.goNodes.bindingsMu.Lock()
	goBindings := append([]int(nil), gateway.goNodes.bindingCount...)
	gateway.goNodes.bindingsMu.Unlock()
	for _, proxy := range gateway.transports.items {
		status := ProxyStatus{Index: proxy.index, Address: config.RedactURL(proxy.name), Healthy: proxy.healthy.Load(), Checking: proxy.checking.Load(), Pool: proxy.pool, Anonymous: gateway.cfg.Anonymous}
		if proxy.relay != nil {
			status.Relay = true
			status.RelaySpent = proxy.relay.SpentCount()
		}
		if checked := proxy.lastChecked.Load(); checked > 0 {
			at := time.Unix(0, checked)
			status.LastChecked = &at
		}
		if blocked := proxy.blockedUntil.Load(); blocked > time.Now().UnixNano() {
			until := time.Unix(0, blocked)
			status.BlockedUntil = &until
		}
		status.LastError = proxy.lastErrorString()
		if !status.Healthy {
			status.Listening = liveLoopbackProxy(proxy.name)
		}
		if proxy.index < len(zenBindings) {
			status.ZenKeys = zenBindings[proxy.index]
		}
		if proxy.index < len(goBindings) {
			status.GoKeys = goBindings[proxy.index]
		}
		result.Proxies = append(result.Proxies, status)
	}
	if gateway.warp != nil {
		snapshot := gateway.warp.Snapshot()
		result.Warp = &WarpStatus{
			Enabled: true, Stack: snapshot.Stack, Size: snapshot.Size,
			Active: snapshot.Active, Standby: snapshot.Standby,
			Cooling: snapshot.Cooling, CoolingPrefixes: snapshot.CoolingPrefixes,
			LaneGated: snapshot.LaneGated, LaneRefusals: snapshot.LaneRefusals,
			Egress: snapshot.Egress, Exits: gateway.warp.States(),
		}
		if !snapshot.LaneUntil.IsZero() {
			until := snapshot.LaneUntil
			result.Warp.LaneUntil = &until
		}
	}
	result.Peers = gateway.peers.peerSnapshot()
	return result
}

// CheckProxies probes every configured proxy route now and returns the number
// of routes that were available to check. The management console uses this to
// replace the old cached 8/8 reading with an on-demand end-to-end result.
func (m *RuntimeController) CheckProxies(ctx context.Context) int {
	runtime := m.current.Load()
	if runtime == nil || runtime.gateway == nil || runtime.gateway.transports == nil {
		return 0
	}
	return runtime.gateway.CheckProxiesNow(ctx)
}

// EgressQuota reports the free-tier bucket board: which egress /64s the pool
// holds, which ones the upstream has refused, and when their window resets.
func (m *RuntimeController) EgressQuota() EgressQuotaSnapshot {
	runtime := m.current.Load()
	if runtime == nil || runtime.gateway == nil {
		return EgressQuotaSnapshot{}
	}
	var states []warp.ExitState
	if runtime.gateway.warp != nil {
		states = runtime.gateway.warp.States()
	}
	snapshot := runtime.gateway.quota.snapshot(states, time.Now())
	if runtime.gateway.warp != nil {
		live := runtime.gateway.warp.Snapshot()
		snapshot.LaneGated = live.LaneGated
		snapshot.LaneUntil = live.LaneUntil
		snapshot.LaneRefusals = live.LaneRefusals
	}
	return snapshot
}

// WarpSnapshot reports the live pool state, or false when WARP is disabled.
func (m *RuntimeController) WarpSnapshot() (warp.Snapshot, bool) {
	runtime := m.current.Load()
	if runtime == nil || runtime.gateway == nil || runtime.gateway.warp == nil {
		return warp.Snapshot{}, false
	}
	return runtime.gateway.warp.Snapshot(), true
}

// ResetLaneGate clears the free-lane refusal pause and starts a refill, so the
// console can retry a registration after the upstream recovers.
func (m *RuntimeController) ResetLaneGate() error {
	runtime := m.current.Load()
	if runtime == nil || runtime.gateway == nil || runtime.gateway.warp == nil {
		return errors.New("WARP pool is not enabled")
	}
	runtime.gateway.warp.ResetLaneGate()
	return nil
}

// AcquireWARPLease borrows one live exit for a diagnostic probe; the caller
// must Release it. It registers nothing, so a gatecheck run costs no device.
func (m *RuntimeController) AcquireWARPLease(ctx context.Context) (*warp.Lease, error) {
	runtime := m.current.Load()
	if runtime == nil || runtime.gateway == nil || runtime.gateway.warp == nil {
		return nil, errors.New("WARP pool is not enabled")
	}
	return runtime.gateway.warp.Acquire(ctx)
}

// EgressAction retires or rotates every exit on a /64. "retire" keeps the
// prefix out of rotation until the free-tier reset; "rotate" replaces it
// immediately. It reports how many exits matched.
func (m *RuntimeController) EgressAction(prefix, action string) (int, error) {
	runtime := m.current.Load()
	if runtime == nil || runtime.gateway == nil || runtime.gateway.warp == nil {
		return 0, errors.New("WARP pool is not enabled")
	}
	switch action {
	case "retire":
		return runtime.gateway.warp.Retire(prefix, time.Now().Add(quotaResetAfter(time.Now()))), nil
	case "rotate":
		return runtime.gateway.warp.Rotate(prefix), nil
	default:
		return 0, fmt.Errorf("unknown egress action %q", action)
	}
}

// AgentModels reports the catalog models with advertised limits, for the agent
// config adapters.
func (m *RuntimeController) AgentModels() []agentcfg.Model {
	runtime := m.current.Load()
	if runtime == nil || runtime.gateway == nil {
		return nil
	}
	return runtime.gateway.AgentModels()
}

// EgressInsights bundles the live budget board with the multi-day usage ledger
// so the page needs one request for the table, the estimator and the heatmap.
func (m *RuntimeController) EgressInsights(days int) EgressInsights {
	quota := m.EgressQuota()
	return EgressInsights{Quota: quota, Usage: usageHistory.Snapshot(days, time.Now()), Days: days}
}

// CapacityEstimate is an intentionally rough read of the free-tier budget: the
// upstream never publishes a per-IP allowance, so this counts distinct observed
// /64 buckets and classifies them by the gateway's own quota hits. It is a
// planning aid, not a measurement of OpenCode's hidden counters.
type CapacityEstimate struct {
	// Model and Family show which quota family this estimate was computed for.
	// The free tier counts IP + model-family + UTC day, so a generic "IP green"
	// number is misleading for a specific model.
	Model  string `json:"model,omitempty"`
	Family string `json:"family,omitempty"`
	// IPsObserved is distinct egress prefixes seen today, across WARP, proxies
	// and direct. IPsUsable excludes prefixes already cooled or quota-walled.
	IPsObserved int `json:"ips_observed"`
	IPsUsable   int `json:"ips_usable"`
	IPsSpent    int `json:"ips_spent"`
	Active      int `json:"active"`
	Cooling     int `json:"cooling"`
	// RequestsToday is the sum of inference attempts recorded on those buckets.
	RequestsToday int `json:"requests_today"`
	QuotaHits     int `json:"quota_hits"`
	// Key quota is tracked separately: OMP on VPS2 was limited by an exhausted
	// Zen key while every /64 still looked green.
	KeyID         string    `json:"key_id,omitempty"`
	KeyExhausted  bool      `json:"key_exhausted"`
	KeyRetryAfter time.Time `json:"key_retry_after,omitempty"`
	// ResetAt is the next UTC window where the upstream counters refill.
	ResetAt time.Time `json:"reset_at"`
	Note    string    `json:"note"`
}

// CapacityEstimate derives a rough "used / remaining" view from the quota
// board. No upstream counter is queried: a /64 with a quota hit is treated as
// spent for the current UTC day, one without a hit as available. When model is
// given, only that model family is counted and key-tier exhaustion is reported.
func (m *RuntimeController) CapacityEstimateFor(model string) CapacityEstimate {
	quota := m.EgressQuota()
	family := model
	if len(family) > 2 {
		family = family[:2]
	}
	seen := make(map[string]bool)
	spent := make(map[string]bool)
	for _, entry := range quota.Exits {
		// With a model filter, ignore other families; an unlabelled cooling
		// exit stays in as conservative spent.
		if family != "" && entry.Family != "" && entry.Family != family {
			continue
		}
		key := prefixLabel(entry.Egress)
		if key == "" {
			continue
		}
		seen[key] = true
		if entry.State == "quota" || entry.State == "cooling" || entry.QuotaHits > 0 || entry.LastQuotaAt.After(time.Time{}) {
			spent[key] = true
		}
	}
	estimate := CapacityEstimate{
		Model: model, Family: family,
		IPsObserved: len(seen),
		IPsSpent:    len(spent),
		Active:      quota.Active,
		Cooling:     quota.Cooling,
		ResetAt:     quota.ResetAt,
		Note:        "rough: distinct observed egress buckets + local quota hits; not upstream counters",
	}
	estimate.IPsUsable = estimate.IPsObserved - estimate.IPsSpent
	if estimate.IPsUsable < 0 {
		estimate.IPsUsable = 0
	}
	for _, entry := range quota.Exits {
		if family != "" && entry.Family != "" && entry.Family != family {
			continue
		}
		estimate.RequestsToday += entry.Requests
		estimate.QuotaHits += entry.QuotaHits
	}
	for _, key := range quota.Keys {
		if family != "" && key.Family != "" && key.Family != family {
			continue
		}
		if key.QuotaHits > 0 {
			estimate.KeyExhausted = true
			estimate.KeyID = key.KeyID
			if key.RetryAfter.After(estimate.KeyRetryAfter) {
				estimate.KeyRetryAfter = key.RetryAfter
			}
			break
		}
	}
	return estimate
}

// CapacityEstimate is the unfiltered view used by the overview strip.
func (m *RuntimeController) CapacityEstimate() CapacityEstimate {
	return m.CapacityEstimateFor("")
}

// DebugModels returns a routing diagnostic for every catalog model, with no
// requested protocol, plus the catalog's metadata snapshot.
func (m *RuntimeController) DebugModels() ([]modelcatalog.RouteDiagnostic, modelcatalog.MetadataSnapshot) {
	runtime := m.current.Load()
	if runtime == nil {
		return nil, modelcatalog.MetadataSnapshot{}
	}
	gateway := runtime.gateway
	models := gateway.catalog.List()
	result := make([]modelcatalog.RouteDiagnostic, 0, len(models))
	for _, model := range models {
		result = append(result, gateway.catalog.Diagnostic(model, "", len(gateway.cfg.ZenKeys) > 0, len(gateway.cfg.GoKeys) > 0, gateway.cfg.Anonymous))
	}
	metadata := gateway.catalog.MetadataSnapshot()
	return result, metadata
}

// DebugRoute reports how the current runtime would route a request for model
// sent on the requested protocol.
func (m *RuntimeController) DebugRoute(model string, requested protocol.Protocol) modelcatalog.RouteDiagnostic {
	runtime := m.current.Load()
	if runtime == nil {
		return modelcatalog.RouteDiagnostic{Model: model, RequestedProtocol: requested, RouteError: "gateway runtime is unavailable"}
	}
	gateway := runtime.gateway
	return gateway.catalog.Diagnostic(model, requested, len(gateway.cfg.ZenKeys) > 0, len(gateway.cfg.GoKeys) > 0, gateway.cfg.Anonymous)
}

// PreviewPlan reports what the gateway would do for model right now, without
// sending a request or changing any cooldown.
func (m *RuntimeController) PreviewPlan(model string, requested protocol.Protocol) PreviewPlan {
	runtime := m.current.Load()
	if runtime == nil || runtime.gateway == nil {
		return PreviewPlan{
			Model: model, Protocol: string(requested), Ready: false,
			Blockers: []string{"gateway runtime is unavailable"},
			Note:     "kế hoạch chỉ đọc: runtime chưa sẵn sàng", GeneratedAt: time.Now().UTC(),
		}
	}
	return runtime.gateway.PreviewRoute(model, requested)
}

// Readiness is the console's copy of the /healthz verdict: the same status,
// ready flag and issue list, without the rest of the health document. The
// overview shows it so the "Ready" chip cannot claim more than the endpoint
// clients actually call.
type Readiness struct {
	Status string   `json:"status"`
	Ready  bool     `json:"ready"`
	Issues []string `json:"issues"`
}

// Readiness reports the readiness verdict of the live gateway.
func (m *RuntimeController) Readiness() Readiness {
	runtime := m.current.Load()
	if runtime == nil || runtime.gateway == nil {
		return Readiness{Status: "starting", Issues: []string{"gateway_runtime_unavailable"}}
	}
	report, _ := runtime.gateway.HealthReport()
	issues := report.Issues
	if issues == nil {
		issues = []string{}
	}
	return Readiness{Status: report.Status, Ready: report.Ready, Issues: issues}
}

// DebugKeyView is the operator-facing view of one configured upstream key. It
// carries only the redacted suffix and the stable fingerprint, never the value.
type DebugKeyView struct {
	ID            string      `json:"id"`
	Tier          config.Tier `json:"tier"`
	Display       string      `json:"display"`
	Index         int         `json:"index"`
	Failures      uint32      `json:"failures"`
	CooldownUntil *time.Time  `json:"cooldown_until,omitempty"`
}

// DebugRelays lists the anonymous relays in pool order for the Playground.
func (m *RuntimeController) DebugRelays() []string {
	rt := m.current.Load()
	if rt == nil || rt.gateway == nil {
		return nil
	}
	return rt.gateway.RelayNames()
}

// DebugKeys lists every configured upstream key per tier so the Playground can
// offer an explicit per-key test target.
func (m *RuntimeController) DebugKeys() map[string][]DebugKeyView {
	result := map[string][]DebugKeyView{"zen": {}, "go": {}}
	runtime := m.current.Load()
	if runtime == nil {
		return result
	}
	appendKeys := func(tier config.Tier, nodes []*upstreamNode) {
		for _, node := range nodes {
			view := DebugKeyView{
				ID: config.Fingerprint(node.key), Tier: tier, Display: config.MaskValue(node.key),
				Index: node.index, Failures: node.failures.Load(),
			}
			if until := node.cooldownUntil.Load(); until > time.Now().UnixNano() {
				value := time.Unix(0, until).UTC()
				view.CooldownUntil = &value
			}
			result[string(tier)] = append(result[string(tier)], view)
		}
	}
	appendKeys(config.TierZen, runtime.gateway.zenNodes.nodes)
	appendKeys(config.TierGo, runtime.gateway.goNodes.nodes)
	return result
}

// DebugKey returns the view of the tier's key whose fingerprint is id, or nil
// when no such key is configured.
func (m *RuntimeController) DebugKey(tier config.Tier, id string) *DebugKeyView {
	for _, view := range m.DebugKeys()[string(tier)] {
		if view.ID == id {
			selected := view
			return &selected
		}
	}
	return nil
}

// DebugRouteForTier reports the route a diagnostic against one tier would take.
func (m *RuntimeController) DebugRouteForTier(model string, tier config.Tier) (modelcatalog.RouteDiagnostic, error) {
	runtime := m.current.Load()
	if runtime == nil {
		return modelcatalog.RouteDiagnostic{Model: model, RouteError: "gateway runtime is unavailable"}, fmt.Errorf("gateway runtime is unavailable")
	}
	gateway := runtime.gateway
	hasZen, hasGo := len(gateway.cfg.ZenKeys) > 0, len(gateway.cfg.GoKeys) > 0
	diagnostic := gateway.catalog.Diagnostic(model, "", hasZen, hasGo, false)
	route, err := gateway.catalog.RouteForTier(model, tier, hasZen, hasGo)
	if err != nil {
		diagnostic.RouteError = err.Error()
		return diagnostic, err
	}
	diagnostic.RouteError = ""
	diagnostic.Tier, diagnostic.Anonymous = route.Tier, false
	diagnostic.KeyTiers = []config.Tier{tier}
	diagnostic.NativeProtocol, diagnostic.NativeProtocols = route.Protocol, route.Protocols
	return diagnostic, nil
}

func keyStatuses(tier string, pool *upstreamNodes) []KeyStatus {
	result := make([]KeyStatus, 0, len(pool.nodes))
	for _, node := range pool.nodes {
		status := KeyStatus{ID: config.KeyDisplayID(node.key), Tier: tier, Index: node.index, ProxyIndex: int(node.proxyIndex.Load()), Bound: node.bound, Failures: node.failures.Load()}
		if until := node.cooldownUntil.Load(); until > time.Now().UnixNano() {
			value := time.Unix(0, until).UTC()
			status.CooldownUntil = &value
		}
		if until := node.credUntil.Load(); until > time.Now().UnixNano() {
			value := time.Unix(0, until).UTC()
			status.QuarantinedUntil = &value
		}
		result = append(result, status)
	}
	return result
}
