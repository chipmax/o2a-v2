package admin

import (
	"net/http"

	"opencode2api/internal/httpx"
	"opencode2api/internal/surfshark"
)

func (a *Server) handleSurfsharkStatus(w http.ResponseWriter, _ *http.Request) {
	if a.surfshark == nil {
		httpx.WriteJSON(w, http.StatusOK, map[string]any{"running": false, "tunnels": 0, "available": false})
		return
	}
	state := a.surfshark.Status()
	httpx.WriteJSON(w, http.StatusOK, state)
}

func (a *Server) handleSurfsharkSetup(w http.ResponseWriter, r *http.Request) {
	if a.surfshark == nil {
		writeAdminError(w, http.StatusBadRequest, "surfshark_unavailable", "Surfshark provisioning is not configured on this instance")
		return
	}
	var input surfshark.SetupRequest
	if err := decodeAdminJSON(w, r, &input); err != nil {
		writeAdminError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	state, err := a.surfshark.Setup(r.Context(), input)
	if err != nil {
		writeAdminError(w, http.StatusBadRequest, "surfshark_setup_failed", err.Error())
		return
	}
	a.logger.Info("surfshark tunnels provisioned", "component", "surfshark", "event", "tunnels_ready", "count", state.Tunnels)
	httpx.WriteJSON(w, http.StatusOK, state)
}

func (a *Server) handleSurfsharkStop(w http.ResponseWriter, _ *http.Request) {
	if a.surfshark == nil {
		writeAdminError(w, http.StatusBadRequest, "surfshark_unavailable", "Surfshark provisioning is not configured on this instance")
		return
	}
	state, err := a.surfshark.Stop()
	if err != nil {
		writeAdminError(w, http.StatusBadRequest, "surfshark_stop_failed", err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, state)
}
