package admin

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"opencode2api/internal/gatecheck"
	"opencode2api/internal/httpx"
	protocol "opencode2api/internal/protocol"
	"opencode2api/internal/telemetry"
	"opencode2api/internal/warp"
)

// gateState is the one gatecheck run the console may have in flight. A probe
// borrows a live exit and sends ten small requests, so runs are serialized.
type gateState struct {
	mu       sync.Mutex
	running  bool
	started  time.Time
	finished time.Time
	report   *gatecheck.Report
	lines    []string
	err      string
}

const gateLineLimit = 200

// handleLaneReset clears the free-lane pause so the pool retries a
// registration now instead of waiting for LaneUntil.
func (a *Server) handleLaneReset(w http.ResponseWriter, _ *http.Request) {
	if err := a.manager.ResetLaneGate(); err != nil {
		writeAdminError(w, http.StatusConflict, "warp_disabled", err.Error())
		return
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"ok": true})
}

// handlePreview explains the routing plan for one model without sending a
// request: which lane goes first, which is skipped, and the live state that
// decided it. The console shows it before an operator starts changing config.
func (a *Server) handlePreview(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Model    string `json:"model"`
		Protocol string `json:"protocol"`
	}
	if err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(&body); err != nil {
		writeAdminError(w, http.StatusBadRequest, "invalid_request", err.Error())
		return
	}
	model := strings.TrimSpace(body.Model)
	if model == "" {
		writeAdminError(w, http.StatusBadRequest, "invalid_request", "model must not be empty")
		return
	}
	requested := protocol.Chat
	switch strings.ToLower(strings.TrimSpace(body.Protocol)) {
	case "responses":
		requested = protocol.Responses
	case "anthropic", "messages":
		requested = protocol.Anthropic
	}
	httpx.WriteJSON(w, http.StatusOK, a.manager.PreviewPlan(model, requested))
}

// handleEvents returns the newest structured log events that describe the
// pool and the free lane, newest first: rotations, lane refusals, quota
// answers and registration failures. It is the overview's incident feed.
func (a *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	events, _ := a.logs.Recent(0, 2000)
	out := make([]telemetry.LogEvent, 0, limit)
	for index := len(events) - 1; index >= 0 && len(out) < limit; index-- {
		if poolEvent(events[index]) {
			out = append(out, events[index])
		}
	}
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"events": out})
}

func poolEvent(event telemetry.LogEvent) bool {
	if event.Level == "warn" || event.Level == "error" {
		return true
	}
	component := strings.ToLower(event.Component)
	if component == "warp" || component == "rotation" {
		return true
	}
	if event.Fields["event"] == "tool_call_audit" {
		return true
	}
	message := strings.ToLower(event.Message)
	for _, word := range []string{"warp", "lane", "egress", "rotate", "registration"} {
		if strings.Contains(message, word) {
			return true
		}
	}
	return false
}

// handleGateCheckStart runs the gate matrix on one borrowed exit in the
// background; the console polls handleGateCheckStatus for the result.
func (a *Server) handleGateCheckStart(w http.ResponseWriter, _ *http.Request) {
	a.gate.mu.Lock()
	if a.gate.running {
		a.gate.mu.Unlock()
		writeAdminError(w, http.StatusConflict, "gatecheck_running", "a gatecheck run is already in progress")
		return
	}
	a.gate.running = true
	a.gate.started = time.Now().UTC()
	a.gate.finished = time.Time{}
	a.gate.report = nil
	a.gate.lines = nil
	a.gate.err = ""
	a.gate.mu.Unlock()

	go a.runGateCheck()
	httpx.WriteJSON(w, http.StatusAccepted, map[string]any{"running": true})
}

func (a *Server) runGateCheck() {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()
	lease, err := a.manager.AcquireWARPLease(ctx)
	if err != nil {
		a.finishGateCheck(nil, err.Error())
		return
	}
	defer lease.Release()
	report := gatecheck.Run(ctx, lease.DialContext, lease.EgressKey(), warp.DefaultLaneProbeModel, "", 45*time.Second, func(format string, args ...any) {
		a.appendGateLine(format, args...)
	})
	a.finishGateCheck(&report, "")
}

func (a *Server) appendGateLine(format string, args ...any) {
	a.gate.mu.Lock()
	defer a.gate.mu.Unlock()
	if len(a.gate.lines) >= gateLineLimit {
		return
	}
	a.gate.lines = append(a.gate.lines, strings.TrimSpace(fmt.Sprintf(format, args...)))
}

func (a *Server) finishGateCheck(report *gatecheck.Report, errText string) {
	a.gate.mu.Lock()
	defer a.gate.mu.Unlock()
	a.gate.running = false
	a.gate.finished = time.Now().UTC()
	a.gate.report = report
	a.gate.err = errText
}

// handleGateCheckStatus reports the last run: whether one is in flight, its
// streamed lines, and the finished matrix when there is one.
func (a *Server) handleGateCheckStatus(w http.ResponseWriter, _ *http.Request) {
	a.gate.mu.Lock()
	payload := map[string]any{
		"running": a.gate.running, "started_at": a.gate.started,
		"lines": a.gate.lines, "error": a.gate.err,
	}
	if !a.gate.finished.IsZero() {
		payload["finished_at"] = a.gate.finished
	}
	if a.gate.report != nil {
		payload["report"] = a.gate.report
	}
	a.gate.mu.Unlock()
	httpx.WriteJSON(w, http.StatusOK, payload)
}
