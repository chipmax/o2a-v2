package admin

import (
	"net/http"

	"opencode2api/internal/httpx"
)

// handleEgressQuota reports the free-tier budget board: one row per egress /64
// and model family, with the pool's live cooling state and the next UTC reset.
func (a *Server) handleEgressQuota(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	httpx.WriteJSON(w, http.StatusOK, a.manager.EgressQuota())
}
