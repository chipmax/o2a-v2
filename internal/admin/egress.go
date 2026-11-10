package admin

import (
	"net/http"
	"strconv"
	"strings"

	"opencode2api/internal/httpx"
)

// handleEgressInsights returns the live /64 board plus the multi-day ledger and
// the fingerprint canary in one payload.
func (a *Server) handleEgressInsights(w http.ResponseWriter, r *http.Request) {
	days := 30
	if raw := r.URL.Query().Get("days"); raw != "" {
		if parsed, err := strconv.Atoi(raw); err == nil && parsed >= 1 && parsed <= 365 {
			days = parsed
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	httpx.WriteJSON(w, http.StatusOK, a.manager.EgressInsights(days))
}

// handleEgressAction retires or rotates one /64. It refuses an empty prefix so
// a malformed UI action cannot touch the whole pool.
func (a *Server) handleEgressAction(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Prefix string `json:"prefix"`
		Action string `json:"action"`
	}
	if err := decodeAdminJSON(w, r, &input); err != nil {
		writeAdminError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	input.Prefix = strings.TrimSpace(input.Prefix)
	input.Action = strings.TrimSpace(input.Action)
	if input.Prefix == "" || len(input.Prefix) > 64 {
		writeAdminError(w, http.StatusBadRequest, "invalid_request", "prefix is required")
		return
	}
	count, err := a.manager.EgressAction(input.Prefix, input.Action)
	if err != nil {
		writeAdminError(w, http.StatusBadRequest, "egress_action_failed", err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"matched": count, "prefix": input.Prefix, "action": input.Action})
}
