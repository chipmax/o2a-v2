package gateway

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
)

// handleMetrics exposes the monitor snapshot in the Prometheus text format. It
// is intentionally unauthenticated because the API listener is expected to be
// bound to localhost or a private network; do not expose it publicly.
func (g *Gateway) handleMetrics(w http.ResponseWriter, _ *http.Request) {
	snapshot := g.monitor.Snapshot()
	var builder strings.Builder

	writeGauge := func(name, help string, value float64) {
		fmt.Fprintf(&builder, "# HELP %s %s\n# TYPE %s gauge\n%s %g\n", name, help, name, name, value)
	}
	writeCounter := func(name, help string, series map[string]uint64) {
		fmt.Fprintf(&builder, "# HELP %s %s\n# TYPE %s counter\n", name, help, name)
		keys := make([]string, 0, len(series))
		for key := range series {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		for _, key := range keys {
			fmt.Fprintf(&builder, "%s{%s} %d\n", name, key, series[key])
		}
	}

	writeGauge("o2a_uptime_seconds", "Seconds since the process monitor started.", float64(snapshot.UptimeSeconds))
	writeGauge("o2a_active_requests", "Requests currently being served.", float64(snapshot.Active))
	writeGauge("o2a_active_streams", "Streams currently sending a response body.", float64(snapshot.ActiveStreams))
	writeGauge("o2a_requests_success_rate", "Lifetime successful request ratio.", snapshot.Lifetime.SuccessRate)
	writeGauge("o2a_usage_reported_ratio", "Share of routed requests whose upstream reported token usage.", snapshot.Usage.Lifetime.Coverage)

	writeCounter("o2a_requests_total", "Requests by period and result.", map[string]uint64{
		`period="lifetime",result="success"`:  snapshot.Lifetime.Success,
		`period="lifetime",result="error"`:    snapshot.Lifetime.Errors,
		`period="last_hour",result="success"`: snapshot.Window.Success,
		`period="last_hour",result="error"`:   snapshot.Window.Errors,
	})
	writeCounter("o2a_tokens_total", "Token usage reported by the upstream, by direction.", map[string]uint64{
		`period="lifetime",direction="input"`:     snapshot.Usage.Lifetime.Tokens.Input,
		`period="lifetime",direction="output"`:    snapshot.Usage.Lifetime.Tokens.Output,
		`period="lifetime",direction="cached"`:    snapshot.Usage.Lifetime.Tokens.Cached,
		`period="lifetime",direction="reasoning"`: snapshot.Usage.Lifetime.Tokens.Reasoning,
		`period="lifetime",direction="total"`:     snapshot.Usage.Lifetime.Tokens.Total,
	})
	if len(snapshot.Models) > 0 {
		writeCounter("o2a_model_requests_total", "Requests per model during the last hour.", prefixLabels("model", snapshot.Models))
	}
	if len(snapshot.Tiers) > 0 {
		writeCounter("o2a_tier_requests_total", "Requests per tier during the last hour.", prefixLabels("tier", snapshot.Tiers))
	}
	if len(snapshot.Statuses) > 0 {
		writeCounter("o2a_status_requests_total", "Requests per HTTP status during the last hour.", prefixLabels("status", snapshot.Statuses))
	}

	builder.WriteString("# HELP o2a_request_duration_seconds Request latency for the last hour.\n# TYPE o2a_request_duration_seconds summary\n")
	for _, quantile := range []struct{ label, value string }{
		{"0.5", fmt.Sprintf("%g", float64(snapshot.Window.P50MS)/1000)},
		{"0.95", fmt.Sprintf("%g", float64(snapshot.Window.P95MS)/1000)},
		{"0.99", fmt.Sprintf("%g", float64(snapshot.Window.P99MS)/1000)},
	} {
		fmt.Fprintf(&builder, "o2a_request_duration_seconds{period=\"last_hour\",quantile=%q} %s\n", quantile.label, quantile.value)
	}
	fmt.Fprintf(&builder, "o2a_request_duration_seconds_sum{period=\"last_hour\"} %g\n", snapshot.Window.AverageMS*float64(snapshot.Window.Total)/1000)
	fmt.Fprintf(&builder, "o2a_request_duration_seconds_count{period=\"last_hour\"} %d\n", snapshot.Window.Total)

	builder.WriteString("# HELP o2a_upstream_attempts_total Upstream calls by result since start.\n# TYPE o2a_upstream_attempts_total counter\n")
	fmt.Fprintf(&builder, "o2a_upstream_attempts_total{result=\"success\"} %d\n", snapshot.Upstream.Lifetime.Success)
	fmt.Fprintf(&builder, "o2a_upstream_attempts_total{result=\"failed\"} %d\n", snapshot.Upstream.Lifetime.Failed)

	proxyTotal, proxyHealthy := g.transports.healthCounts()
	builder.WriteString("# HELP o2a_proxies Configured proxy transports by health state.\n# TYPE o2a_proxies gauge\n")
	fmt.Fprintf(&builder, "o2a_proxies{state=\"healthy\"} %d\no2a_proxies{state=\"unhealthy\"} %d\n", proxyHealthy, proxyTotal-proxyHealthy)
	builder.WriteString("# HELP o2a_upstream_keys Configured upstream keys by tier.\n# TYPE o2a_upstream_keys gauge\n")
	fmt.Fprintf(&builder, "o2a_upstream_keys{tier=\"zen\"} %d\no2a_upstream_keys{tier=\"go\"} %d\n", g.zenNodes.Len(), g.goNodes.Len())

	_, models := g.availableModels()
	writeGauge("o2a_catalog_models", "Models reported by the upstream catalog.", float64(models.Total))
	writeGauge("o2a_catalog_exposed", "Models routed by the current configuration.", float64(models.Exposed))
	if g.warp != nil {
		warp := g.warp.Snapshot()
		builder.WriteString("# HELP o2a_warp_exits WARP exits by state.\n# TYPE o2a_warp_exits gauge\n")
		fmt.Fprintf(&builder, "o2a_warp_exits{state=\"active\"} %d\no2a_warp_exits{state=\"standby\"} %d\no2a_warp_exits{state=\"cooling\"} %d\n", warp.Active, warp.Standby, warp.Cooling)
	}

	w.Header().Set("Content-Type", "text/plain; version=0.0.4; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	fmt.Fprint(w, builder.String())
}

func prefixLabels(label string, values map[string]uint64) map[string]uint64 {
	result := make(map[string]uint64, len(values))
	for value, count := range values {
		result[label+`="`+escapeLabel(value)+`"`] = count
	}
	return result
}

func escapeLabel(value string) string {
	value = strings.ReplaceAll(value, `\`, `\\`)
	value = strings.ReplaceAll(value, "\n", `\n`)
	return strings.ReplaceAll(value, `"`, `\"`)
}
