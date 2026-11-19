package gateway

import (
	"context"
	"encoding/json"
	"io"
	"strings"
	"time"

	"opencode2api/internal/identity"
	"opencode2api/internal/models"
)

// controlProbeOn429 sends the smallest possible free-lane request through the
// SAME egress that just returned a 429. A 200 means the refused request was
// shaped wrongly (or was simply large), not that the /64 is spent. Rotating on
// that verdict throws away a healthy block. See refusalKind in abuse.go.
//
// It returns status 0 when the probe could not be completed, in which case the
// caller keeps the legacy "rotate on 429" behaviour.
func (g *Gateway) controlProbeOn429(ctx context.Context, egress string, route models.Route, baseURL string, ids identity.RequestIDs) int {
	if g == nil || g.warp == nil || egress == "" {
		return 0
	}
	key := strings.TrimPrefix(egress, "warp:")
	if key == "" {
		return 0
	}
	probeCtx, cancel := context.WithTimeout(ctx, 8*time.Second)
	defer cancel()

	lease, err := g.warp.AcquirePreferred(probeCtx, key)
	if err != nil || lease == nil {
		return 0
	}
	owned, err := g.newWarpLease(lease, g.warpUsesBun(baseURL, route.ID))
	if err != nil {
		lease.Release()
		return 0
	}
	// Minimal body with the free tier's mandatory `read` tool and forced stream.
	payload := map[string]interface{}{
		"model": route.ID,
		"messages": []map[string]string{
			{"role": "user", "content": "OK"},
		},
		"max_tokens": 1,
		"stream":     true,
		"tools": []map[string]interface{}{
			{"type": "function", "function": map[string]interface{}{
				"name":        "read",
				"description": "Read a file",
				"parameters":  map[string]interface{}{"type": "object", "properties": map[string]interface{}{}},
			}},
		},
	}
	body, err := json.Marshal(payload)
	if err != nil {
		owned.close()
		return 0
	}
	prepared, _ := prepareAnonymousBody(body, route.Protocol, models.IsV1Upstream(baseURL), g.cfg.Upstream.PromptEnvironment)
	req, err := upstreamRequest(probeCtx, baseURL, route.Protocol, prepared, ids, anonymousZenKey)
	if err != nil {
		owned.close()
		return 0
	}
	resp, err := doInferenceAttempt(owned.client(), req, 0, g.warpAttemptTimeout())
	if err != nil || resp == nil {
		owned.close()
		return 0
	}
	status := resp.StatusCode
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 4096))
	resp.Body.Close()
	owned.close()
	return status
}

// parseRetryAfter is already defined in the gateway package (warp.go/abuse.go).
// This file only groups the probe helper and its classification together.
