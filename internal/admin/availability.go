package admin

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"opencode2api/internal/gateway"
	"opencode2api/internal/httpx"
)

// handleAvailability lists the free models with their probe state, so the
// availability page can show which ones are disabled and why.
func (a *Server) handleAvailability(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"models": a.manager.AvailabilitySnapshot()})
}

// handleProbeModels marks every automatic free model due and wakes the probe
// loop, so the availability page's refresh triggers a real re-probe instead
// of only reloading the cached snapshot. It answers with the current
// availability list; the fresh results land as the sweep records them.
func (a *Server) handleProbeModels(w http.ResponseWriter, r *http.Request) {
	a.manager.RequestAvailabilitySweep()
	a.handleAvailability(w, r)
}

// handleContextProbe measures the model's real context window by sending real
// requests through the gateway. It refuses while live streams are active (a
// probe is heavy and the free lane is shared) unless force is set, and it runs
// at most one probe at a time. The observation board is updated as attempts
// land, so the result also survives a cancelled HTTP client.
func (a *Server) handleContextProbe(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Model       string `json:"model"`
		MaxRequests int    `json:"max_requests"`
		Force       bool   `json:"force"`
	}
	if err := decodeAdminJSON(w, r, &input); err != nil {
		writeAdminError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	input.Model = strings.TrimSpace(input.Model)
	if input.Model == "" {
		writeAdminError(w, http.StatusBadRequest, "invalid_request", "model is required")
		return
	}
	if !input.Force && a.monitor != nil && a.monitor.Snapshot().ActiveStreams > 0 {
		writeAdminError(w, http.StatusConflict, "gateway_busy", "live streams are active; retry when idle or send force")
		return
	}
	if !a.probeRunning.CompareAndSwap(false, true) {
		writeAdminError(w, http.StatusTooManyRequests, "probe_running", "a context probe is already running")
		return
	}
	defer a.probeRunning.Store(false)
	ctx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), 20*time.Minute)
	defer cancel()
	result, err := a.manager.ProbeContext(ctx, input.Model, input.MaxRequests)
	if err != nil {
		// A spent free-tier bucket says nothing about the context window: answer
		// 429 with the reset hint so the WebUI can tell the operator to wait or
		// switch egress instead of showing a probe failure.
		if quota, ok := gateway.QuotaExhausted(err); ok {
			seconds := int(quota.RetryAfter.Seconds())
			if seconds > 0 {
				w.Header().Set("Retry-After", strconv.Itoa(seconds))
			}
			httpx.WriteJSON(w, http.StatusTooManyRequests, map[string]any{"error": map[string]any{
				"code":                "quota_exhausted",
				"message":             a.manager.Redact(quota.Error()),
				"model":               quota.Model,
				"retry_after_seconds": seconds,
			}})
			return
		}
		writeAdminError(w, http.StatusBadGateway, "context_probe_failed", a.manager.Redact(err.Error()))
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	httpx.WriteJSON(w, http.StatusOK, result)
}

// handleRestoreModel returns a disabled free model to manual control, which
// re-enables it and keeps automatic probes from disabling it again. It answers
// with the refreshed availability list.
func (a *Server) handleRestoreModel(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Model string `json:"model"`
	}
	if err := decodeAdminJSON(w, r, &input); err != nil {
		writeAdminError(w, 400, "invalid_request", err.Error())
		return
	}
	if err := a.manager.RestoreModel(input.Model); err != nil {
		writeAdminError(w, 400, "restore_failed", a.manager.Redact(err.Error()))
		return
	}
	a.handleAvailability(w, r)
}

// handleManualModel switches a free model between manual and automatic
// control: a manual model stays enabled and is not probed, while switching
// back to automatic schedules a probe. It answers with the refreshed list.
func (a *Server) handleManualModel(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Model  string `json:"model"`
		Manual *bool  `json:"manual"`
	}
	if err := decodeAdminJSON(w, r, &input); err != nil {
		writeAdminError(w, 400, "invalid_request", err.Error())
		return
	}
	if input.Manual == nil {
		writeAdminError(w, 400, "invalid_request", "manual is required")
		return
	}
	if err := a.manager.SetManualModel(input.Model, *input.Manual); err != nil {
		writeAdminError(w, 400, "manual_failed", a.manager.Redact(err.Error()))
		return
	}
	a.handleAvailability(w, r)
}
