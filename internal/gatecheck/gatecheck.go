// Package gatecheck probes the free-tier gate contract through one live WARP
// exit: an accepted baseline plus one single-dimension violation per gate, so
// an upstream rule change shows up as a DIFF instead of an opaque 429.
//
// The command cmd/gatecheck uses it with a freshly registered exit; the admin
// console runs it on a lease from the live pool, which costs no registration.
package gatecheck

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"net"
	"net/http"
	"strings"
	"time"

	"opencode2api/internal/httpx"
	"opencode2api/internal/identity"
)

// BasePrompt is the smallest system prompt the free lane accepts: it carries
// the <env> block with a working directory, which the gate requires.
const BasePrompt = "You are opencode, an interactive CLI tool that helps users with software engineering tasks.\n<env>\n  Working directory: /workspace\n  Platform: linux\n</env>"

// Variant is one probe: a body dimension changed while everything else stays
// at the accepted baseline.
type Variant struct {
	Name    string
	User    string
	Session string
	System  *string
	Tools   []string
	Stream  bool
	Expect  int
}

// Check is the outcome of one probe.
type Check struct {
	Name  string `json:"name"`
	Want  int    `json:"want"`
	Got   int    `json:"got"`
	Match bool   `json:"match"`
}

// Report is the full matrix outcome for one exit.
type Report struct {
	Egress     string   `json:"egress,omitempty"`
	Checks     []Check  `json:"checks"`
	Enforced   []string `json:"enforced"`
	Mismatched int      `json:"mismatched"`
	BaselineOK bool     `json:"baseline_ok"`
}

// Variants returns the baseline and the single-dimension violations.
func Variants(model, session string) []Variant {
	accepted := http.StatusOK
	refused := http.StatusTooManyRequests
	noWorkdir := "You are opencode, an interactive CLI tool.\n<env>\n  Platform: linux\n</env>"
	full := BasePrompt
	return []Variant{
		{Name: "baseline", User: "Reply with exactly: OK", Session: session, System: &full, Tools: []string{"read", "bash"}, Stream: true, Expect: accepted},
		{Name: "no working directory", Session: session, System: &noWorkdir, Tools: []string{"read", "bash"}, Stream: true, Expect: refused},
		{Name: "no system message", Session: session, System: nil, Tools: []string{"read", "bash"}, Stream: true, Expect: refused},
		{Name: "no read tool", Session: session, System: &full, Tools: []string{"bash"}, Stream: true, Expect: refused},
		{Name: "no tools", Session: session, System: &full, Tools: nil, Stream: true, Expect: refused},
		{Name: "stream false", Session: session, System: &full, Tools: []string{"read", "bash"}, Stream: false, Expect: refused},
		{Name: "random session hex", Session: RandomSession(), System: &full, Tools: []string{"read", "bash"}, Stream: true, Expect: refused},
		{Name: "session 400 days old", Session: StaleSession(400), System: &full, Tools: []string{"read", "bash"}, Stream: true, Expect: refused},
		{Name: "ua 1.17.2", Session: session, System: &full, Tools: []string{"read", "bash"}, Stream: true, Expect: refused},
	}
}

// Run sends the matrix through dial, which must reach the free lane from one
// exit. logf receives one human-readable line per probe (may be nil).
func Run(ctx context.Context, dial func(context.Context, string, string) (net.Conn, error), egress, model, url string, timeout time.Duration, logf func(string, ...any)) Report {
	report := Report{Egress: egress}
	if logf == nil {
		logf = func(string, ...any) {}
	}
	if model == "" {
		model = "mimo-v2.6-flash-free"
	}
	if url == "" {
		url = "https://opencode.ai/inference/v1/chat/completions"
	}
	if timeout <= 0 {
		timeout = 45 * time.Second
	}
	client := NewClient(dial, timeout)
	defer client.CloseIdleConnections()
	session := identity.CanonicalSessionID("gatecheck")
	for _, variant := range Variants(model, session) {
		body := ProbeBody(model, variant.System, variant.Tools, variant.Stream)
		ua := httpx.V2UserAgent()
		if strings.HasPrefix(variant.Name, "ua 1.17") {
			ua = "opencode/1.17.2"
		}
		status, _ := Send(ctx, client, url, body, variant.Session, ua, timeout)
		match := status == variant.Expect
		if !match {
			report.Mismatched++
		}
		if status != http.StatusOK {
			report.Enforced = append(report.Enforced, variant.Name)
		}
		if variant.Name == "baseline" {
			report.BaselineOK = match
		}
		verdict := "MATCH"
		if !match {
			verdict = "DIFF"
		}
		logf("%-24s want %d got %d  %s", variant.Name, variant.Expect, status, verdict)
		report.Checks = append(report.Checks, Check{Name: variant.Name, Want: variant.Expect, Got: status, Match: match})
	}
	return report
}

// NewClient builds the HTTP client gatecheck uses over dial.
func NewClient(dial func(context.Context, string, string) (net.Conn, error), timeout time.Duration) *http.Client {
	transport, err := httpx.NewBunTransport(httpx.BunOptions{
		DialContext:           dial,
		ConnectTimeout:        10 * time.Second,
		ResponseHeaderTimeout: timeout,
		DisableKeepAlives:     true,
	})
	if err != nil {
		return &http.Client{Timeout: timeout}
	}
	return &http.Client{Transport: transport, Timeout: timeout}
}

// BaselineBody is the accepted request every probe is derived from.
func BaselineBody(model string) map[string]any {
	prompt := BasePrompt
	return ProbeBody(model, &prompt, []string{"read", "bash"}, true)
}

// ProbeBody builds one request body with the given dimensions.
func ProbeBody(model string, system *string, tools []string, stream bool) map[string]any {
	messages := []map[string]string{}
	if system != nil {
		messages = append(messages, map[string]string{"role": "system", "content": *system})
	}
	messages = append(messages, map[string]string{"role": "user", "content": "Reply with exactly: OK"})
	body := map[string]any{
		"model":      model,
		"stream":     stream,
		"max_tokens": 16,
		"messages":   messages,
	}
	if len(tools) > 0 {
		declared := make([]map[string]any, 0, len(tools))
		for _, name := range tools {
			declared = append(declared, map[string]any{
				"type": "function",
				"function": map[string]any{
					"name":        name,
					"description": name,
					"parameters":  map[string]any{"type": "object", "properties": map[string]any{}},
				},
			})
		}
		body["tools"] = declared
	}
	return body
}

// Send posts one probe body and returns the status and a short body excerpt.
func Send(ctx context.Context, client *http.Client, url string, body map[string]any, session, ua string, timeout time.Duration) (int, string) {
	payload, err := json.Marshal(body)
	if err != nil {
		return 0, err.Error()
	}
	reqCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(reqCtx, http.MethodPost, url, strings.NewReader(string(payload)))
	if err != nil {
		return 0, err.Error()
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "*/*")
	req.Header.Set("Authorization", "Bearer public")
	req.Header.Set("User-Agent", ua)
	req.Header.Set("x-opencode-session", session)
	req.Header.Set("x-opencode-session-id", session)
	resp, err := client.Do(req)
	if err != nil {
		return 0, err.Error()
	}
	defer resp.Body.Close()
	text, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<16))
	return resp.StatusCode, string(text)
}

// RandomSession returns a canonical-looking session whose hex does not decode
// to a plausible timestamp: the gate must refuse it.
func RandomSession() string {
	var stamp [6]byte
	if _, err := rand.Read(stamp[:]); err != nil {
		return "ses_0000000000000000000000000000"
	}
	return "ses_" + hex.EncodeToString(stamp[:]) + randomTail()
}

// StaleSession returns a session whose timestamp is days in the past.
func StaleSession(days int) string {
	ms := uint64(time.Now().Add(-time.Duration(days) * 24 * time.Hour).UnixMilli())
	packed := ^(ms<<12 | 1) & 0xFFFFFFFFFFFF
	return fmt.Sprintf("ses_%012x", packed) + randomTail()
}

func randomTail() string {
	const alphabet = "0123456789ABCDEFGHIJKLMNOPQRSTUVWXYZabcdefghijklmnopqrstuvwxyz"
	var raw [14]byte
	if _, err := rand.Read(raw[:]); err != nil {
		return "00000000000000"
	}
	var tail [14]byte
	for index := range tail {
		tail[index] = alphabet[int(raw[index])%len(alphabet)]
	}
	return string(tail[:])
}
