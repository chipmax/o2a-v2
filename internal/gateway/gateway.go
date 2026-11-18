// Package gateway routes inference requests through managed upstream pools.
//
// Upstreams fall into two lanes, named after the OpenCode client generation
// whose request shape they expect, not after their URL path: both lanes
// append the /v1/... route segment. The v1 lane is the https://opencode.ai/zen
// origin (models.V1Base, also the default upstream.zen), which
// models.IsV1Upstream detects. It expects the OpenCode 1.x client shape: the
// 1.x User-Agent, the x-opencode-request header, a git-commit-shaped project
// and, on the free tier, the 1.x toolset. The v2 lane is the /inference
// origin (upstream.anonymous) and the Go plan's /zen/go origin, which expect
// the OpenCode 2.x CLI shape. The anonymous free tier uses the configured v2
// /inference origin only; the retired v1 fallback is kept in
// anonymousUpstreams but commented out.
package gateway

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"opencode2api/internal/config"
	"opencode2api/internal/httpx"
	"opencode2api/internal/identity"
	"opencode2api/internal/jsonutil"
	"opencode2api/internal/models"
	wire "opencode2api/internal/protocol"
	"opencode2api/internal/rotation"
	"opencode2api/internal/telemetry"
	"opencode2api/internal/warp"
)

const maxRequestBody = 32 << 20

// anonymousZenKey is the shared credential the anonymous free tier sends as
// its API key.
const anonymousZenKey = "public"

// attemptDeadline returns the per-attempt deadline, capped at the request
// timeout. It is zero unless attempt_timeout_seconds is set: AttemptTimeout
// would otherwise fall back to the request timeout, which the request context
// already enforces.
func attemptDeadline(cfg config.Config) time.Duration {
	if cfg.Performance.AttemptTimeoutSeconds <= 0 {
		return 0
	}
	return cfg.Performance.AttemptTimeout(time.Duration(cfg.Retry.TimeoutSeconds) * time.Second)
}

// attemptTimeoutFor returns the per-attempt deadline for one model, honouring
// performance.model_timeouts before the global attempt timeout.
func (g *Gateway) attemptTimeoutFor(model string) time.Duration {
	if seconds := g.cfg.Performance.ModelTimeouts[model]; seconds > 0 {
		return g.cfg.Performance.AttemptTimeoutFor(model, time.Duration(g.cfg.Retry.TimeoutSeconds)*time.Second)
	}
	return g.attemptTimeout
}

// statusClientClosedRequest is nginx's 499: the client went away before the
// gateway could answer, so the failure is neither upstream nor ours.
const statusClientClosedRequest = 499

// proxyUnavailableMessage is the 503 message sent when the error a request
// finally failed with is tagged errProxyUnavailable: its last attempt never
// reached the upstream because the proxy route failed, or the anonymous lane
// had no proxy to try while none was healthy. Only that final error is
// inspected; earlier attempts may have failed for other reasons. The message
// points operators at their proxies instead of the upstream.
const proxyUnavailableMessage = "all proxies are unavailable; the upstream could not be reached"

// Gateway serves the local inference API for one configuration. It owns the
// proxy transports and the key and anonymous node pools built from that
// config; applying a new config builds a new Gateway (see RuntimeController)
// instead of mutating this one.
type Gateway struct {
	rotation   *rotation.Manager
	cfg        config.Config
	logger     *slog.Logger
	transports *proxyTransports
	zenNodes   *upstreamNodes
	goNodes    *upstreamNodes
	anonymous  *anonymousNodes
	// warp, when non-nil, is the shared userspace WARP pool. Free-model
	// traffic on the anonymous lane and the Zen key tier leaves through it
	// instead of the configured proxies and transports.
	warp *warp.Pool
	// warpPausedUntil is the Unix-nano instant before which free-model traffic
	// skips WARP and uses the configured proxies. The anonymous phase sets it
	// when its exits are all quota-walled, so the key tiers do not inherit the
	// same slow path.
	warpPausedUntil atomic.Int64
	catalog         *models.ModelCatalog
	monitor         *telemetry.Metrics
	availability    *models.AvailabilityRegistry
	// attemptTimeout bounds a whole upstream attempt (dial + write + headers
	// and, when the first-event wait is enabled, the first event), not just
	// the header wait. Zero arms no per-attempt deadline: each attempt is then
	// bounded only by the request-level budget.
	attemptTimeout time.Duration
	// freeTierBlock is how long an exit the free tier flagged stays out of the
	// anonymous cursor. Zero uses freeTierBlockTTL.
	freeTierBlock time.Duration
	// refreshMu prevents overlapping catalog refreshes across concurrent or scheduled callers.
	refreshMu sync.Mutex
	// rotationSyncMu guards the cached catalog revision and Go key count used
	// to skip rebuilding the rotation availability map on every request.
	rotationSyncMu sync.Mutex
	rotationSynced bool
	rotationAt     time.Time
	rotationGoKeys int
	rotationAvail  uint64
	// peers are other o2a gateways this one falls back to when every local
	// lane fails, so a single listen address serves the whole fleet.
	peers *peerPool
	// quota records per-egress free-tier spend for the budget page.
	quota *quotaBoard
	// affinity pins a session to one egress /64 so prompt caching survives
	// rotation; see affinity.go.
	affinity *egressAffinity
	// contextLimits persists what live traffic has proven about each model's
	// usable context. The controller shares one board across hot reloads.
	contextLimits *models.ContextLimits
	// kilo is the independent Kilo free-gateway lane. nil when no Kilo model
	// is configured, so existing deployments keep their exact routing.
	kilo *kiloLane
	// peerFirst holds, per model family, the moment its local free-tier bucket
	// resets after the upstream reported it spent. While it is in the future,
	// requests for that family go to a peer first.
	peerMu    sync.Mutex
	peerFirst map[string]time.Time
	// probeMu guards probedAt, the last probe of each cooling credential or
	// relay, so the quota probe rotates instead of retrying one target.
	probeMu  sync.Mutex
	probedAt map[string]time.Time
	// inflight is the global admission gate: a nil channel means unbounded.
	// queueWait is how long an overflow request may wait for a slot.
	inflight      chan struct{}
	queueWait     time.Duration
	inflightNow   atomic.Int64
	inflightTotal atomic.Uint64
	queuedTotal   atomic.Uint64
}

// New builds a Gateway for cfg: one proxy transport per runtime proxy (a
// single direct transport when none is configured), Zen and Go key pools
// bound to those transports, the anonymous node pool and a model catalog. Its
// rotation manager is not persisted; RuntimeController replaces it with the
// shared one.
func New(cfg config.Config, logger *slog.Logger, monitor *telemetry.Metrics, warpPool *warp.Pool) (*Gateway, error) {
	if logger == nil {
		logger = slog.Default()
	}
	cfg.Rotation.Defaults()
	rotationManager, err := rotation.New("", cfg.Rotation)
	if err != nil {
		return nil, err
	}
	transports, err := transportPool(cfg.RuntimeProxies(), cfg.Performance, cfg.Egress, cfg.Performance.AttemptTimeout(time.Duration(cfg.Retry.TimeoutSeconds)*time.Second), logger)
	if err != nil {
		return nil, err
	}
	cooldown := time.Duration(cfg.Performance.FailureCooldownSeconds) * time.Second
	zenNodes, err := nodePool(cfg.ZenKeys, transports, cooldown, cfg.ProxyBindings)
	if err != nil {
		return nil, fmt.Errorf("zen node pool: %w", err)
	}
	goNodes, err := nodePool(cfg.GoKeys, transports, cooldown, cfg.ProxyBindings)
	if err != nil {
		return nil, fmt.Errorf("go node pool: %w", err)
	}
	quarantine := cfg.Performance.CredentialQuarantine()
	zenNodes.quarantine = quarantine
	goNodes.quarantine = quarantine
	var inflight chan struct{}
	if cfg.Performance.MaxInflight > 0 {
		inflight = make(chan struct{}, cfg.Performance.MaxInflight)
	}
	catalog := models.Catalog(cfg.Prefer, cfg.Models.Protocols)
	catalog.SetRefreshInterval(time.Duration(cfg.Models.RefreshSeconds) * time.Second)
	catalog.SetFreeOnly(cfg.Models.FreeOnly)
	catalog.SetDeadModels(cfg.Models.DeadIDs)
	contextLimits, _ := models.OpenContextLimits("")
	g := &Gateway{
		cfg:            cfg,
		rotation:       rotationManager,
		logger:         logger,
		transports:     transports,
		zenNodes:       zenNodes,
		goNodes:        goNodes,
		anonymous:      anonymousPool(cfg.Anonymous, transports, cooldown, anonymousBackoffFromConfig(cfg.Models.AnonymousBackoff)),
		warp:           warpPool,
		catalog:        catalog,
		monitor:        monitor,
		attemptTimeout: attemptDeadline(cfg),
		freeTierBlock:  time.Duration(cfg.Performance.FreeTierBlockSeconds) * time.Second,
		peers:          newPeerPool(cfg.Peers),
		quota:          egressQuota,
		affinity:       newEgressAffinity(),
		contextLimits:  contextLimits,
		kilo:           newKiloLane(cfg.Kilo),
		inflight:       inflight,
		queueWait:      time.Duration(cfg.Performance.QueueWaitSeconds) * time.Second,
	}
	g.seedKilo()
	return g, nil
}

// seedKilo installs the configured Kilo allowlist before the first request,
// so a freshly started gateway can route those models even before the catalog
// refresh completes. The live refresh replaces the seed if it succeeds.
func (g *Gateway) seedKilo() {
	if g == nil || !g.kilo.enabled() {
		return
	}
	g.catalog.SetKiloModels(g.kilo.cfg.Models)
	seed := make([]models.KiloModel, 0, len(g.kilo.cfg.Models))
	for _, id := range g.kilo.cfg.Models {
		shared := false
		for _, flagged := range g.kilo.cfg.DataSharingIDs {
			if flagged == id {
				shared = true
				break
			}
		}
		seed = append(seed, models.KiloModel{ID: id, DataSharing: shared})
	}
	g.catalog.SetKiloMetadata(seed)
}

// anonymousBackoffFromConfig converts the configured hours into the breaker's
// thresholds and durations. A nil block disables the breaker (threshold 0).
func anonymousBackoffFromConfig(cfg *config.AnonymousBackoffConfig) anonymousBackoff {
	if cfg == nil {
		return anonymousBackoff{}
	}
	return anonymousBackoff{
		threshold: int32(cfg.Threshold),
		base:      time.Duration(cfg.BaseHours) * time.Hour,
		max:       time.Duration(cfg.MaxHours) * time.Hour,
	}
}

// Handler returns the gateway's HTTP API: the server-key authenticated /v1
// model, inference and System One routes plus the open /healthz, wrapped in
// panic recovery.
func (g *Gateway) Handler() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /v1/models", g.authenticate(g.handleModels))
	mux.HandleFunc("POST /v1/chat/completions", g.authenticate(g.gated(wire.Chat, g.handleInference(wire.Chat))))
	mux.HandleFunc("POST /v1/responses", g.authenticate(g.gated(wire.Responses, g.handleInference(wire.Responses))))
	mux.HandleFunc("POST /v1/messages", g.authenticate(g.gated(wire.Anthropic, g.handleInference(wire.Anthropic))))
	mux.HandleFunc("POST /v1/systemone", g.authenticate(g.gated(wire.SystemOne, g.handleSystemOne)))
	mux.HandleFunc("GET /healthz", g.handleHealth)
	mux.HandleFunc("GET /metrics", g.handleMetrics)
	return telemetry.Recover(g.logger, mux)
}

// authenticate admits a request whose x-api-key header or Bearer token
// matches an enabled server key, and stores the caller's fingerprint in the
// request context and telemetry metadata. Anything else gets a 401 shaped for
// the endpoint: Anthropic for /v1/messages, Chat otherwise.
func (g *Gateway) authenticate(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		candidates := []string{strings.TrimSpace(r.Header.Get("x-api-key"))}
		if auth := r.Header.Get("Authorization"); strings.HasPrefix(strings.ToLower(auth), "bearer ") {
			candidates = append(candidates, strings.TrimSpace(auth[7:]))
		}
		matched := ""
		for _, key := range g.cfg.ServerKeys {
			if !g.cfg.ServerKeyEnabled(key) {
				continue
			}
			for _, candidate := range candidates {
				if len(candidate) == len(key) && subtle.ConstantTimeCompare([]byte(candidate), []byte(key)) == 1 {
					matched = key
				}
			}
		}
		if matched == "" {
			protocol := wire.Chat
			if r.URL.Path == "/v1/messages" {
				protocol = wire.Anthropic
			}
			wire.WriteError(w, protocol, http.StatusUnauthorized, "invalid local API key", "authentication_error", "")
			return
		}
		// The Playground borrows the first enabled key to pass this check, so
		// its traffic gets its own bucket instead of being billed to that key.
		caller := config.Fingerprint(matched)
		if isDiagnosticRequest(r.Context()) {
			caller = telemetry.PlaygroundServerKey
		}
		if meta := telemetry.MetaFromRequest(r); meta != nil {
			meta.ServerKeyID = caller
		}
		next(w, r.WithContext(context.WithValue(r.Context(), callerContextKey{}, caller)))
	}
}

// acquireQuota applies the caller's per-key policy to one inference request.
// It is called by the inference handlers after the model-name checks, not by
// authenticate: listing models, rejected requests and malformed bodies must
// not consume a daily budget or hold a concurrency slot.
func (g *Gateway) acquireQuota(w http.ResponseWriter, r *http.Request, external wire.Protocol) (func(), bool) {
	caller := callerScope(r.Context())
	release, refusal := quota.acquire(caller, g.cfg.PolicyByID(caller))
	if refusal == nil {
		return release, true
	}
	seconds := int((refusal.retryAfter + time.Second - 1) / time.Second)
	w.Header().Set("Retry-After", strconv.Itoa(seconds))
	wire.WriteError(w, external, http.StatusTooManyRequests, refusal.message, refusal.code, "")
	return nil, false
}

type callerContextKey struct{}

// callerScope returns the authenticated caller that request identities are
// scoped to, so different server keys never share an upstream session.
func callerScope(ctx context.Context) string {
	caller, _ := ctx.Value(callerContextKey{}).(string)
	return caller
}

// handleInference serves a message endpoint whose wire protocol is external.
// A rotation group alias is handed to handleRotation. Any other model is
// routed through the catalog, encoded per upstream tier, sent through
// doUpstream and translated back into external, streaming or not.
type requestShapeContextKey struct{}

type requestShape struct {
	shaped  bool
	renames *wire.ToolRenames
}

func setRequestShape(ctx context.Context, shaped bool, renames *wire.ToolRenames) {
	if state, ok := ctx.Value(requestShapeContextKey{}).(*requestShape); ok {
		state.shaped, state.renames = shaped, renames
	}
	if meta := telemetry.MetaFromContext(ctx); meta != nil {
		meta.Shaped = shaped
		meta.ToolRenames = renames.Count()
	}
}

func requestShapeFrom(ctx context.Context) requestShape {
	if state, ok := ctx.Value(requestShapeContextKey{}).(*requestShape); ok && state != nil {
		return *state
	}
	return requestShape{}
}

func (g *Gateway) handleInference(external wire.Protocol) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		r = r.WithContext(context.WithValue(r.Context(), requestShapeContextKey{}, &requestShape{}))
		body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBody))
		if err != nil {
			wire.WriteError(w, external, http.StatusBadRequest, "request body is too large or unreadable", "invalid_request_error", "")
			return
		}
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			wire.WriteError(w, external, http.StatusBadRequest, "request body must be a JSON object", "invalid_request_error", "")
			return
		}
		model := jsonutil.StringAt(payload, "model")
		meta := telemetry.MetaFromRequest(r)
		if meta != nil {
			meta.Model = model
			meta.RequestBytes = int64(len(body))
		}
		if model == "" {
			wire.WriteError(w, external, http.StatusBadRequest, "model is required", "invalid_request_error", "model")
			return
		}
		if !g.cfg.PolicyByID(callerScope(r.Context())).ModelAllowed(model) {
			wire.WriteError(w, external, http.StatusForbidden, fmt.Sprintf("this API key is not allowed to use model %q", model), "model_not_allowed", "model")
			return
		}
		if id := g.rotation.GroupID(model); id != "" {
			release, ok := g.acquireQuota(w, r, external)
			if !ok {
				return
			}
			defer release()
			g.handleRotation(w, r, external, payload, id, model)
			return
		}
		ids := identity.DeriveRequestIDs(r, payload, callerScope(r.Context()))
		if meta != nil {
			meta.Request = ids.Request
			meta.Session = ids.Session
		}
		if !g.catalog.Supported(model) {
			// A peer may know a model this instance does not: forward before
			// answering "unknown model", so one address stays authoritative.
			// Only that forwarding path is an inference request; a local 404
			// does not spend the caller's budget.
			if g.peers.Len() > 0 {
				release, ok := g.acquireQuota(w, r, external)
				if !ok {
					return
				}
				defer release()
				if served, status := g.tryPeersStatus(r.Context(), w, r, body, model, config.Tier(""), ids.Request); served {
					if status/100 != 2 {
						quota.refund(callerScope(r.Context()))
					}
					return
				}
				quota.refund(callerScope(r.Context()))
			}
			// A model the catalog has never heard of is a bad model NAME: 404
			// tells a client to pick a different model. A model the catalog
			// knows but cannot serve is a capability or routing problem, and
			// the old 400 phrasing ("the model uses an upstream protocol that
			// opencode2api does not expose") is accurate for it. The old code
			// returned that 400 for BOTH, so a request for a model the
			// upstream retired (it answers 410 Gone) looked like a gateway
			// capability bug and clients retried forever.
			if g.catalog.Known(model) {
				wire.WriteError(w, external, http.StatusBadRequest, "the model uses an upstream protocol that opencode2api does not expose", "invalid_request_error", "model")
			} else {
				wire.WriteError(w, external, http.StatusNotFound, fmt.Sprintf("model %q was not found in the upstream model catalog", model), "model_not_found", "model")
			}
			return
		}
		route, err := g.catalog.Route(model, len(g.cfg.ZenKeys) > 0, len(g.cfg.GoKeys) > 0, g.cfg.Anonymous)
		if override, selected := debugKeyOverrideFrom(r.Context()); selected {
			// A per-key diagnostic must not silently be served by another key,
			// another tier or the anonymous lane.
			route, err = g.catalog.RouteForTier(model, override.Tier, len(g.cfg.ZenKeys) > 0, len(g.cfg.GoKeys) > 0)
		}
		if err != nil {
			wire.WriteError(w, external, http.StatusBadRequest, err.Error(), "invalid_request_error", "model")
			return
		}
		if meta != nil {
			meta.Tier = string(route.Tier)
			meta.Protocol = route.Protocol
		}
		// A System One model has no message-shaped equivalent, so a decision
		// payload submitted on a message endpoint is relayed verbatim instead of
		// being converted. This keeps the model reachable for clients that can
		// only address /v1/chat/completions or /v1/responses — for example a
		// gateway whose OpenAI platform pins every request to Responses.
		if route.Protocol == wire.SystemOne {
			if len(jsonutil.MapAt(payload, "questions")) == 0 || payload["state"] == nil {
				wire.WriteError(w, external, http.StatusBadRequest, "this model uses System One decisions, not chat; send state and typed questions to /v1/systemone", "invalid_request_error", "model")
				return
			}
			release, ok := g.acquireQuota(w, r, external)
			if !ok {
				return
			}
			defer release()
			g.forwardSystemOne(w, r, body, payload, model, route)
			return
		}
		bodies, err := g.prepareRouteBodies(external, route, payload)
		if err != nil {
			wire.WriteError(w, external, http.StatusBadRequest, err.Error(), "invalid_request_error", "")
			return
		}
		release, ok := g.acquireQuota(w, r, external)
		if !ok {
			return
		}
		defer release()
		stream := jsonutil.BoolAt(payload, "stream")
		// The local bucket of this model family is spent for the rest of its
		// window: ask a peer first instead of rotating exits that cannot serve.
		if g.peers.Len() > 0 && !isDiagnosticRequest(r.Context()) && g.localBucketSpent(model) {
			if g.tryPeers(r.Context(), w, r, body, model, route.Tier, ids.Request) {
				return
			}
		}
		requestCtx, cancel := context.WithTimeout(r.Context(), time.Duration(g.cfg.Retry.TimeoutSeconds)*time.Second)
		defer cancel()
		resp, upstreamRoute, err := g.doUpstream(requestCtx, route, bodies, ids)
		if err != nil {
			// The error branch never forwards the last response body, so close
			// it here; otherwise a block or a paused lane leaks one upstream
			// connection on every request.
			if resp != nil && resp.Body != nil {
				httpx.DrainAndClose(resp.Body)
			}
			// The request itself is fine but no local lane could serve it:
			// hand it to the peers before answering an upstream failure.
			if errors.Is(err, errFreeTierQuotaSpent) {
				now := time.Now()
				g.markLocalQuotaSpent(model, now, now.Add(quotaResetAfter(now)))
			}
			if !errors.Is(err, errDeterministicClientRejection) && !isDiagnosticRequest(requestCtx) {
				// Peer forwarding has its own header wait; the local request
				// budget must not cut a peer's long stream.
				if g.tryPeers(r.Context(), w, r, body, model, upstreamRoute.Tier, ids.Request) {
					return
				}
			}
			finalTier := route.Tier
			if meta != nil && meta.Tier != "" {
				finalTier = config.Tier(meta.Tier)
			}
			keyID, channel, anonymous := requestCredential(requestCtx)
			// A downstream disconnect is not an upstream failure: report the
			// real cause so telemetry and clients see a cancel, not a 502.
			if !errors.Is(err, context.DeadlineExceeded) && wire.ClientCanceled(r.Context(), err) {
				g.logger.Debug("client canceled before upstream response", "component", "upstream", "event", "request_canceled", "request_id", ids.Request, "tier", finalTier, "key_id", keyID, "channel", channel, "anonymous", anonymous)
				if meta != nil {
					meta.Outcome = "client_canceled"
				}
				wire.WriteError(w, external, statusClientClosedRequest, "client closed request", "client_canceled", ids.Request)
				return
			}
			g.logger.Warn("all upstream attempts failed", "component", "upstream", "event", "request_failed", "request_id", ids.Request, "tier", finalTier, "key_id", keyID, "channel", channel, "anonymous", anonymous, "error", err)
			// The final attempt never reached the upstream because its proxy
			// route failed; report that instead of blaming the upstream with
			// a 502.
			if errors.Is(err, errProxyUnavailable) {
				wire.WriteError(w, external, http.StatusServiceUnavailable, proxyUnavailableMessage, "proxy_error", ids.Request)
				return
			}
			if errors.Is(err, errWARPUnavailable) {
				wire.WriteError(w, external, http.StatusServiceUnavailable, warpUnavailableMessage, "warp_unavailable", ids.Request)
				return
			}
			// The key pool answered from its own state: every key was cooling
			// (429) or every key was rejected as a credential (503). Both
			// carry the real retry horizon, so the client backs off with a
			// number instead of replaying the request.
			if retryAfter, ok := keyPoolRetryAfter(err); ok {
				status, message, code := http.StatusServiceUnavailable, keyPoolCredentialMessage, "key_pool_credential_exhausted"
				if errors.Is(err, errKeyPoolCooling) {
					status, message, code = http.StatusTooManyRequests, keyPoolCoolingMessage, "key_pool_cooling"
				}
				if seconds := int(retryAfter.Seconds()); seconds > 0 {
					w.Header().Set("Retry-After", strconv.Itoa(seconds))
				}
				wire.WriteError(w, external, status, message, code, ids.Request)
				return
			}
			// The upstream refuses this model from this country. Report the
			// region as the cause: rotating keys or retrying cannot fix it.
			if errors.Is(err, errModelUnavailableInRegion) {
				wire.WriteError(w, external, http.StatusNotFound, modelUnavailableInRegionMessage, "model_not_available_in_region", ids.Request)
				return
			}
			// An exhausted request budget is a timeout, not a bad gateway: the
			// distinction matters to clients that retry on 502.
			if errors.Is(err, context.DeadlineExceeded) {
				// A write-phase failure means the upstream never finished
				// reading the request body: the payload is too large. Report
				// it as 413 so a client that can shrink the request (a harness
				// that strips oversized inline images) knows to shrink it
				// instead of retrying the same oversized payload forever. A
				// header- or first-event-phase failure is the upstream being
				// slow, which is not the client's fault, and stays a 504.
				status, message, code := timeoutStatus(err, requestCtx.Err() == nil)
				wire.WriteError(w, external, status, message, code, ids.Request)
				return
			}
			// Every exit this request could use is under the free tier's IP
			// block. A 502 would read as a transient upstream fault and invite
			// the client to retry into the same penalty, so answer with the
			// block itself.
			if errors.Is(err, errFreeTierIPBlocked) {
				wire.WriteError(w, external, http.StatusServiceUnavailable, freeTierBlockedMessage, "free_tier_ip_blocked", ids.Request)
				return
			}
			// Every lane answered 200 with the free tier's quota error
			// document: the quota itself is gone, so a retry now cannot help
			// and the client should back off instead of replaying the body.
			if errors.Is(err, errFreeTierQuotaSpent) {
				// The free tier counts per egress /64 and resets at midnight
				// UTC, so hand the client the real remaining window instead of
				// a one-minute hint that only produces retry churn.
				w.Header().Set("Retry-After", strconv.Itoa(int(quotaResetAfter(time.Now()).Seconds())))
				wire.WriteError(w, external, http.StatusTooManyRequests, freeTierQuotaMessage, "free_tier_quota_exhausted", ids.Request)
				return
			}
			if errors.Is(err, errKiloQuotaSpent) {
				wire.WriteError(w, external, http.StatusTooManyRequests, "all Kilo free models are cooling down after a provider-shared-pool limit", "kilo_quota_spent", ids.Request)
				return
			}
			// The anonymous lane's circuit breaker is open. Report the pause
			// distinctly so the operator does not look for a credential or a
			// model problem in the upstream error.
			if errors.Is(err, errAnonymousLanePaused) {
				wire.WriteError(w, external, http.StatusServiceUnavailable, anonymousLanePausedMessage, "free_tier_lane_paused", ids.Request)
				return
			}
			wire.WriteError(w, external, http.StatusBadGateway, "all upstream attempts failed", "upstream_error", ids.Request)
			return
		}
		defer resp.Body.Close()
		if meta != nil {
			meta.Tier = string(upstreamRoute.Tier)
			meta.Protocol = upstreamRoute.Protocol
		}
		w.Header().Set("x-request-id", ids.Request)
		if resp.StatusCode/100 != 2 {
			if resp.StatusCode == http.StatusTooManyRequests && upstreamRoute.Anonymous {
				// The free lane refused: its bucket for this family is spent
				// until the window resets, so later requests skip the rotation.
				now := time.Now()
				g.markLocalQuotaSpent(model, now, now.Add(quotaResetAfter(now)))
			}
			// A spent free-tier bucket or a credential the upstream refuses is
			// exactly what the peer list exists for: a peer with its own WARP
			// exits and keys can still serve the request. The local response
			// stays untouched, so it is still the answer if every peer fails.
			if g.peers.Len() > 0 && !isDiagnosticRequest(r.Context()) &&
				(resp.StatusCode == http.StatusTooManyRequests || resp.StatusCode >= 500 ||
					resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden) {
				if g.tryPeers(r.Context(), w, r, body, model, upstreamRoute.Tier, ids.Request) {
					return
				}
			}
			// A 401 or 403 here is the upstream rejecting the gateway's own
			// key (invalid, expired, or blocked in this region), not the
			// caller's: authentication already succeeded. Forwarding the
			// upstream status would send the client looking for a credential
			// problem on its side, so name the real cause and let it retry.
			if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
				g.logger.Warn("upstream rejected the gateway credential", "component", "upstream", "event", "upstream_credential_rejected", "request_id", ids.Request, "tier", upstreamRoute.Tier, "status", resp.StatusCode)
				wire.WriteError(w, external, http.StatusBadGateway, "the upstream rejected the gateway's own credential; check the upstream key configured for this tier", "upstream_credential_rejected", ids.Request)
				return
			}
			if bodyPrefix := peekResponseBody(resp); isContextLengthResponse(resp.StatusCode, bodyPrefix) {
				advertised := g.contextLimit(model, upstreamRoute.Tier)
				if advertised > 0 {
					// Agents that read headers can compact immediately instead
					// of replaying the same oversized body.
					w.Header().Set("X-Context-Limit", strconv.Itoa(advertised))
				}
				requestBytes := int64(len(body))
				if meta != nil && meta.RequestBytes > 0 {
					requestBytes = meta.RequestBytes
				}
				estimate := 0
				applied := false
				if !isDiagnosticRequest(r.Context()) && g.contextLimits != nil {
					estimate, applied = g.contextLimits.ObserveRejection(model, upstreamRoute.Tier, requestBytes, bodyPrefix, advertised)
					if applied {
						w.Header().Set("X-Context-Limit", strconv.Itoa(g.contextLimit(model, upstreamRoute.Tier)))
					}
				}
				g.logger.Warn("upstream context limit exceeded", "component", "upstream", "event", "context_limit_exceeded", "request_id", ids.Request, "model", model, "tier", upstreamRoute.Tier, "advertised", advertised, "estimated_tokens", estimate, "advertisement_updated", applied)
			}
			copyErrorResponse(w, external, resp, ids.Request)
			return
		}
		shape := requestShapeFrom(r.Context())
		toolRenames := shape.renames
		if stream {
			if meta != nil {
				meta.Stream = true
			}
			if g.monitor != nil {
				g.monitor.BeginStream()
				defer g.monitor.EndStream()
			}
			w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
			w.Header().Set("Cache-Control", "no-cache")
			w.Header().Set("X-Accel-Buffering", "no")
			w.WriteHeader(resp.StatusCode)
			keepalive := newStreamKeepalive(w, external, g.cfg.Performance.Heartbeat(), g.cfg.Performance.StreamIdleTimeout())
			defer keepalive.Stop()
			idleTimeout := g.cfg.Performance.StreamIdleTimeout()
			streamBody := wire.RenameStreamReader(newIdleReadCloser(resp.Body, idleTimeout, func() {
				g.logger.Warn("upstream stream idle", "component", "upstream", "event", "stream_idle", "request_id", ids.Request, "timeout_seconds", int(idleTimeout.Seconds()))
				keepalive.WriteIdle()
			}), upstreamRoute.Protocol, toolRenames)
			var usage wire.Usage
			var usageReported bool
			if external == upstreamRoute.Protocol {
				usage, usageReported, err = wire.ForwardStream(r.Context(), keepalive, streamBody, upstreamRoute.Protocol, model)
			} else {
				usage, usageReported, err = wire.TranscodeStream(r.Context(), keepalive, streamBody, upstreamRoute.Protocol, external, model)
			}
			if meta != nil {
				meta.Usage, meta.UsageReported = usage, usageReported
				if err == nil && usageReported && usage.Input > 0 && g.contextLimits != nil && !isDiagnosticRequest(r.Context()) {
					g.contextLimits.ObserveSuccess(model, upstreamRoute.Tier, usage.Input, meta.RequestBytes)
				}
				if err != nil {
					meta.Outcome = "stream_error"
					if wire.ClientCanceled(r.Context(), err) {
						meta.Outcome = "client_canceled"
					}
					if keepalive.IdleTripped() {
						// The watchdog closed the upstream body, so the read
						// error looks like a cancel. Name the real cause.
						meta.Outcome = "upstream_idle_timeout"
					}
				}
			}
			if err != nil && !errors.Is(err, context.Canceled) {
				g.logger.Warn("downstream stream ended with an error", "component", "stream", "event", "stream_failed", "request_id", ids.Request, "model", model, "tier", upstreamRoute.Tier, "error", err)
			}
			return
		}
		responseBody, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
		if err != nil {
			wire.WriteError(w, external, http.StatusBadGateway, "failed to read upstream response", "upstream_error", ids.Request)
			return
		}
		if upstreamRoute.Anonymous || shape.shaped {
			// The anonymous lane, and a key-tier request shaped like it, is
			// force-streamed upstream (see prepareAnonymousBody); collapse
			// the events back into the single document this non-streaming
			// client asked for.
			collapsed, err := wire.CollapseStream(bytes.NewReader(responseBody), upstreamRoute.Protocol, model)
			if err != nil {
				g.logger.Warn("forced stream collapse failed", "component", "conversion", "event", "stream_collapse_failed", "request_id", ids.Request, "model", model, "anonymous", upstreamRoute.Anonymous, "source_protocol", upstreamRoute.Protocol, "error", err)
				wire.WriteError(w, external, http.StatusBadGateway, "unsupported upstream response", "upstream_error", ids.Request)
				return
			}
			responseBody = collapsed
		}
		responseBody = wire.RenameResponseTool(responseBody, upstreamRoute.Protocol, toolRenames)
		if usage, reported := wire.ResponseUsage(upstreamRoute.Protocol, responseBody); meta != nil {
			meta.Usage, meta.UsageReported = usage, reported
			if reported && usage.Input > 0 && g.contextLimits != nil && !isDiagnosticRequest(r.Context()) {
				g.contextLimits.ObserveSuccess(model, upstreamRoute.Tier, usage.Input, meta.RequestBytes)
			}
		}
		if external != upstreamRoute.Protocol {
			responseBody, err = wire.ConvertResponse(upstreamRoute.Protocol, external, responseBody)
			if err != nil {
				g.logger.Warn("response protocol conversion failed", "component", "conversion", "event", "response_conversion_failed", "request_id", ids.Request, "model", model, "source_protocol", upstreamRoute.Protocol, "target_protocol", external, "error", err)
				wire.WriteError(w, external, http.StatusBadGateway, "unsupported upstream response", "upstream_error", ids.Request)
				return
			}
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(resp.StatusCode)
		_, _ = w.Write(responseBody)
	}
}

// handleSystemOne serves System One decision requests on their own endpoint.
// Only models whose upstream protocol is System One can be served here; a chat
// or responses model belongs on its own endpoint, where the bridge can convert
// it.
func (g *Gateway) handleSystemOne(w http.ResponseWriter, r *http.Request) {
	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, maxRequestBody))
	if err != nil {
		wire.WriteError(w, wire.SystemOne, http.StatusBadRequest, "request body is too large or unreadable", "invalid_request_error", "")
		return
	}
	var payload map[string]any
	if err := json.Unmarshal(body, &payload); err != nil {
		wire.WriteError(w, wire.SystemOne, http.StatusBadRequest, "request body must be a JSON object", "invalid_request_error", "")
		return
	}
	model := jsonutil.StringAt(payload, "model")
	if meta := telemetry.MetaFromRequest(r); meta != nil {
		meta.Model = model
		meta.RequestBytes = int64(len(body))
	}
	if model == "" {
		wire.WriteError(w, wire.SystemOne, http.StatusBadRequest, "model is required", "invalid_request_error", "model")
		return
	}
	if !g.cfg.PolicyByID(callerScope(r.Context())).ModelAllowed(model) {
		wire.WriteError(w, wire.SystemOne, http.StatusForbidden, fmt.Sprintf("this API key is not allowed to use model %q", model), "model_not_allowed", "model")
		return
	}
	if !g.catalog.Supported(model) {
		// Same split as the chat path: a model the catalog has never heard of
		// is a bad model NAME (404), a model it knows but cannot serve is a
		// capability or routing problem (400).
		if g.catalog.Known(model) {
			wire.WriteError(w, wire.SystemOne, http.StatusBadRequest, "the model uses an upstream protocol that opencode2api does not expose", "invalid_request_error", "model")
		} else {
			wire.WriteError(w, wire.SystemOne, http.StatusNotFound, fmt.Sprintf("model %q was not found in the upstream model catalog", model), "model_not_found", "model")
		}
		return
	}
	route, err := g.catalog.Route(model, len(g.cfg.ZenKeys) > 0, len(g.cfg.GoKeys) > 0, g.cfg.Anonymous)
	if err != nil {
		wire.WriteError(w, wire.SystemOne, http.StatusBadRequest, err.Error(), "invalid_request_error", "model")
		return
	}
	if route.Protocol != wire.SystemOne {
		wire.WriteError(w, wire.SystemOne, http.StatusBadRequest, fmt.Sprintf("the model does not use the %s protocol", wire.SystemOne), "invalid_request_error", "model")
		return
	}
	release, ok := g.acquireQuota(w, r, wire.SystemOne)
	if !ok {
		return
	}
	defer release()
	g.forwardSystemOne(w, r, body, payload, model, route)
}

// forwardSystemOne relays a System One decision payload verbatim to the
// upstream systemone endpoint and returns the typed answer document unchanged.
// The payload pairs a free-form state with typed questions, which no
// message-shaped upstream protocol accepts, so it is never translated. Answers
// are non-streaming today; a streaming upstream reply is still relayed.
func (g *Gateway) forwardSystemOne(w http.ResponseWriter, r *http.Request, body []byte, payload map[string]any, model string, route models.Route) {
	meta := telemetry.MetaFromRequest(r)
	if meta != nil {
		meta.Model = model
		meta.Tier = string(route.Tier)
		meta.Protocol = route.Protocol
	}
	// One verbatim body serves every tier: a decision payload has no per-tier
	// encoding, so no protocol conversion is attempted.
	bodies := make(map[config.Tier][]byte, len(route.KeyTiers)+1)
	bodies[route.Tier] = body
	for _, tier := range route.KeyTiers {
		bodies[tier] = body
	}
	ids := identity.DeriveRequestIDs(r, payload, callerScope(r.Context()))
	if meta != nil {
		meta.Request = ids.Request
		meta.Session = ids.Session
	}
	requestCtx, cancel := context.WithTimeout(r.Context(), time.Duration(g.cfg.Retry.TimeoutSeconds)*time.Second)
	defer cancel()
	resp, upstreamRoute, err := g.doUpstream(requestCtx, route, bodies, ids)
	if err != nil {
		finalTier := route.Tier
		if meta != nil && meta.Tier != "" {
			finalTier = config.Tier(meta.Tier)
		}
		keyID, channel, anonymous := requestCredential(requestCtx)
		if !errors.Is(err, context.DeadlineExceeded) && wire.ClientCanceled(r.Context(), err) {
			g.logger.Debug("client canceled before upstream response", "component", "upstream", "event", "request_canceled", "request_id", ids.Request, "tier", finalTier, "key_id", keyID, "channel", channel, "anonymous", anonymous)
			if meta != nil {
				meta.Outcome = "client_canceled"
			}
			wire.WriteError(w, wire.SystemOne, statusClientClosedRequest, "client closed request", "client_canceled", ids.Request)
			return
		}
		g.logger.Warn("all upstream attempts failed", "component", "upstream", "event", "request_failed", "request_id", ids.Request, "tier", finalTier, "key_id", keyID, "channel", channel, "anonymous", anonymous, "error", err)
		if errors.Is(err, errProxyUnavailable) {
			wire.WriteError(w, wire.SystemOne, http.StatusServiceUnavailable, proxyUnavailableMessage, "proxy_error", ids.Request)
			return
		}
		if errors.Is(err, errWARPUnavailable) {
			wire.WriteError(w, wire.SystemOne, http.StatusServiceUnavailable, warpUnavailableMessage, "warp_unavailable", ids.Request)
			return
		}
		if retryAfter, ok := keyPoolRetryAfter(err); ok {
			status, message, code := http.StatusServiceUnavailable, keyPoolCredentialMessage, "key_pool_credential_exhausted"
			if errors.Is(err, errKeyPoolCooling) {
				status, message, code = http.StatusTooManyRequests, keyPoolCoolingMessage, "key_pool_cooling"
			}
			if seconds := int(retryAfter.Seconds()); seconds > 0 {
				w.Header().Set("Retry-After", strconv.Itoa(seconds))
			}
			wire.WriteError(w, wire.SystemOne, status, message, code, ids.Request)
			return
		}
		if errors.Is(err, errModelUnavailableInRegion) {
			wire.WriteError(w, wire.SystemOne, http.StatusNotFound, modelUnavailableInRegionMessage, "model_not_available_in_region", ids.Request)
			return
		}
		if errors.Is(err, context.DeadlineExceeded) {
			// Same split as the chat path: a write-phase failure means the
			// payload is too large (413), every other phase is a slow
			// upstream (504).
			status, message, code := timeoutStatus(err, requestCtx.Err() == nil)
			wire.WriteError(w, wire.SystemOne, status, message, code, ids.Request)
			return
		}
		wire.WriteError(w, wire.SystemOne, http.StatusBadGateway, "all upstream attempts failed", "upstream_error", ids.Request)
		return
	}
	defer resp.Body.Close()
	if meta != nil {
		meta.Tier = string(upstreamRoute.Tier)
		meta.Protocol = upstreamRoute.Protocol
	}
	w.Header().Set("x-request-id", ids.Request)
	if resp.StatusCode/100 != 2 {
		prefix := peekResponseBody(resp)
		if !isDiagnosticRequest(r.Context()) {
			g.noteContextRejection(w, model, upstreamRoute.Tier, resp.StatusCode, prefix, int64(len(body)))
		}
		copyErrorResponse(w, wire.SystemOne, resp, ids.Request)
		return
	}
	if contentType := resp.Header.Get("Content-Type"); strings.HasPrefix(contentType, "text/event-stream") {
		if meta != nil {
			meta.Stream = true
		}
		w.Header().Set("Content-Type", contentType)
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(resp.StatusCode)
		keepalive := newStreamKeepalive(w, wire.SystemOne, g.cfg.Performance.Heartbeat(), g.cfg.Performance.StreamIdleTimeout())
		defer keepalive.Stop()
		idleTimeout := g.cfg.Performance.StreamIdleTimeout()
		_, _ = io.Copy(keepalive, newIdleReadCloser(resp.Body, idleTimeout, func() {
			g.logger.Warn("upstream stream idle", "component", "upstream", "event", "stream_idle", "request_id", ids.Request, "timeout_seconds", int(idleTimeout.Seconds()))
			keepalive.WriteIdle()
		}))
		return
	}
	responseBody, err := io.ReadAll(io.LimitReader(resp.Body, 64<<20))
	if err != nil {
		wire.WriteError(w, wire.SystemOne, http.StatusBadGateway, "failed to read upstream response", "upstream_error", ids.Request)
		return
	}
	if usage, reported := wire.ResponseUsage(upstreamRoute.Protocol, responseBody); meta != nil {
		meta.Usage, meta.UsageReported = usage, reported
		if reported && usage.Input > 0 && g.contextLimits != nil && !isDiagnosticRequest(r.Context()) {
			g.contextLimits.ObserveSuccess(model, upstreamRoute.Tier, usage.Input, meta.RequestBytes)
		}
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(resp.StatusCode)
	_, _ = w.Write(responseBody)
}

// prepareRouteBodies encodes input once for each key tier the route may use,
// the preferred tier first and then its KeyTiers, in that tier's upstream
// protocol and with any forced reasoning effort applied. A fallback tier the
// request cannot be converted for is skipped; only a conversion failure on
// the preferred tier is an error.
func (g *Gateway) prepareRouteBodies(from wire.Protocol, route models.Route, input map[string]any) (map[config.Tier][]byte, error) {
	tiers := make([]config.Tier, 0, len(route.KeyTiers)+1)
	seen := make(map[config.Tier]bool, len(route.KeyTiers)+1)
	addTier := func(tier config.Tier) {
		if tier == config.TierKilo || seen[tier] {
			if tier == config.TierKilo {
				seen[tier] = true
				tiers = append(tiers, tier)
			}
			return
		}
		if tier != config.TierZen && tier != config.TierGo || seen[tier] {
			return
		}
		seen[tier] = true
		tiers = append(tiers, tier)
	}
	addTier(route.Tier)
	for _, tier := range route.KeyTiers {
		addTier(tier)
	}
	if len(tiers) == 0 {
		return nil, errors.New("no usable upstream tier")
	}
	bodies := make(map[config.Tier][]byte, len(tiers))
	for _, tier := range tiers {
		protocol := route.ProtocolFor(tier)
		baseURL := g.cfg.Upstream.Zen
		if tier == config.TierGo {
			baseURL = g.cfg.Upstream.Go
		} else if tier == config.TierKilo && g.kilo != nil {
			baseURL = g.kilo.cfg.BaseURL
		}
		upstreamPayload, err := wire.PrepareRequest(from, protocol, input, baseURL)
		if err != nil {
			if tier != route.Tier {
				// A fallback tier may use a stricter wire format than the
				// preferred tier. Do not reject a request before the preferred
				// upstream has even been tried; that tier is attempted only if
				// the request actually falls back.
				g.logger.Debug("skipping fallback tier with incompatible request shape", "component", "conversion", "event", "fallback_tier_skipped", "model", jsonutil.StringAt(input, "model"), "tier", tier, "protocol", protocol, "error", err)
				continue
			}
			return nil, fmt.Errorf("prepare %s upstream request: %w", tier, err)
		}
		if effort := g.resolvedEffort(jsonutil.StringAt(upstreamPayload, "model"), tier, protocol); effort != "" {
			wire.ForcedEffort(protocol, upstreamPayload, effort)
		}
		encoded, err := jsonutil.Marshal(upstreamPayload)
		if err != nil {
			return nil, errors.New("request contains unsupported JSON values")
		}
		bodies[tier] = encoded
	}
	return bodies, nil
}

// copyErrorResponse relays a non-2xx upstream reply as an error in the
// client's protocol. It keeps the status and any Retry-After, and takes the
// message from a JSON body's error.message or message, falling back to the
// status text.
func copyErrorResponse(w http.ResponseWriter, protocol wire.Protocol, resp *http.Response, requestID string) {
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4<<20))
	if retryAfter := resp.Header.Get("Retry-After"); retryAfter != "" {
		w.Header().Set("Retry-After", retryAfter)
	}
	message := http.StatusText(resp.StatusCode)
	var value map[string]any
	if json.Unmarshal(body, &value) == nil {
		message = jsonutil.FirstString(jsonutil.StringAt(value, "error", "message"), jsonutil.StringAt(value, "message"), message)
	}
	wire.WriteError(w, protocol, resp.StatusCode, message, "upstream_error", requestID)
}
