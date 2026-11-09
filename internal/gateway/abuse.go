package gateway

import (
	"bytes"
	"errors"
	"io"
	"net/http"
	"strings"
	"time"
)

// errFreeTierIPBlocked reports that OpenCode's free gateway refused the request
// because it flagged the egress IP, not the request, the key or the session.
// Unlike the shape gates, another lane, key or retry cannot fix it: the same
// IP gets the same answer, and retrying only extends the penalty. The request
// must therefore leave the anonymous lane instead of walking its upstreams,
// and the exit must stay out of the cursor until the block expires.
var errFreeTierIPBlocked = errors.New("free tier blocked the egress IP")

// freeTierBlockedMessage is what a client receives while its exit is blocked.
// The upstream message tells people to turn off a VPN, which is wrong for a
// server that egresses through a proxy; this one names the real cause and the
// two ways out.
const freeTierBlockedMessage = "OpenCode is blocking free-model traffic from the IP this gateway leaves from, so retrying now cannot succeed. Route the request through another exit or wait for the block to expire."

// errAnonymousLanePaused reports that the anonymous lane's circuit breaker has
// paused the whole lane. Unlike an upstream rejection, another exit or a retry
// cannot succeed while the lane is paused; the caller should use an
// authenticated tier or report the pause itself.
var errAnonymousLanePaused = errors.New("anonymous lane paused by its circuit breaker")

// errFreeTierQuotaSpent reports that the upstream answered 200 with a quota
// error document instead of a completion: the free tier spends one bucket per
// egress IP and reports exhaustion that way once it is out. Treating that body
// as a success handed clients a broken response, so it counts as a retryable
// failure for this exit and lets the request fall through to a keyed tier.
var errFreeTierQuotaSpent = errors.New("free tier quota exhausted on this exit")

// freeTierQuotaMessage is what a client receives when every exit and tier the
// request could use has spent its free quota.
const freeTierQuotaMessage = "the upstream free quota is used up on every exit this gateway can reach right now. Add an upstream key, wait for the quota window to reset, or retry later."

// quotaErrorMarkers are the error types and wordings the free tier uses when it
// reports exhaustion in a 200 response body instead of a 429 status.
var quotaErrorMarkers = []string{
	"freeusagelimiterror",
	"freetiererror",
	"free usage limit",
	"usage limit reached",
	"rate_limit_exceeded",
	"too many requests",
} // anonymousLanePausedMessage is what a client receives while the circuit
// breaker holds the anonymous lane shut. It names the cause so the operator is
// not sent looking for a credential or model problem.
const anonymousLanePausedMessage = "The anonymous free tier is paused after repeated upstream failures. Configure an authenticated key, wait for the pause to expire, or restart the gateway after confirming the upstream block has lifted."

// freeTierBlockTTL keeps a blocked exit out of the anonymous cursor. Upstream
// does not publish the block duration, so this is a conservative backoff: long
// enough to stop the retry storm, short enough that an unblocked IP recovers
// without a restart.
const freeTierBlockTTL = 15 * time.Minute

// freeTierBlockTTL returns the backoff applied to an exit the free tier flagged.
func (g *Gateway) freeTierBlockTTL() time.Duration {
	if g != nil && g.freeTierBlock > 0 {
		return g.freeTierBlock
	}
	return freeTierBlockTTL
}

// abuseProbeLimit bounds how much of a 403 body is read to classify it.
const abuseProbeLimit = 16 << 10

// isNetworkAbuseBlock reports whether status and body are the upstream's IP
// abuse rejection. The response is a 403 whose message names the egress
// network, in one of the wordings the gateway has been served:
//
//   - "…free models aren't available from your current network because we've
//     detected abusive traffic from this IP address…"
//   - "Forbidden, OpenCode Free Model Gateway Detect Abuse Traffic"
//   - "Abuse of traffic"
//
// The shape gate's 403 ("free tier can only be used from within OpenCode") is
// deliberately not matched: that one is a request-shape problem, which another
// lane or a refreshed body can still fix.
func isNetworkAbuseBlock(status int, body []byte) bool {
	if status != http.StatusForbidden {
		return false
	}
	text := strings.ToLower(string(body))
	if strings.Contains(text, "abusive traffic") ||
		strings.Contains(text, "abuse traffic") ||
		strings.Contains(text, "abuse of traffic") {
		return true
	}
	return strings.Contains(text, "free models aren't available") &&
		strings.Contains(text, "from your current network")
}

// peekResponseBody reads the start of a response body and puts it back in
// front of the rest, so classification and downstream error handling both see
// the full payload.
func peekResponseBody(resp *http.Response) []byte {
	if resp == nil || resp.Body == nil {
		return nil
	}
	prefix, err := io.ReadAll(io.LimitReader(resp.Body, abuseProbeLimit))
	resp.Body = replayBody(prefix, resp.Body)
	if err != nil {
		return nil
	}
	return prefix
}

// peekNetworkAbuseBlock classifies a response without consuming its body.
func peekNetworkAbuseBlock(resp *http.Response) bool {
	if resp == nil || resp.StatusCode != http.StatusForbidden {
		return false
	}
	return isNetworkAbuseBlock(resp.StatusCode, peekResponseBody(resp))
}

// isQuotaErrorDocument reports a 2xx response that actually carries an error
// document. The v2 inference lane answers 200 with {"type":"error",...} once
// the free quota of the egress IP is spent, and forwarding that body as a
// success gives the client a response it cannot parse. Only JSON responses are
// inspected: an event stream must never be blocked on while headers are
// classified.
func isQuotaErrorDocument(resp *http.Response) bool {
	if resp == nil || resp.StatusCode/100 != 2 || resp.Body == nil {
		return false
	}
	if !strings.Contains(strings.ToLower(resp.Header.Get("Content-Type")), "json") {
		return false
	}
	text := strings.ToLower(string(peekResponseBody(resp)))
	if !strings.Contains(text, `"type":"error"`) &&
		!strings.Contains(text, `"type": "error"`) &&
		!strings.Contains(text, `"error":`) &&
		!strings.Contains(text, `"error": `) {
		return false
	}
	for _, marker := range quotaErrorMarkers {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

// freeTierQuotaWindowMin and freeTierQuotaWindowMax clamp quotaResetAfter. The
// free tier counts per egress /64 and per model family, and resets at midnight
// UTC; the bound only guards against clock skew.
const (
	freeTierQuotaWindowMin = 10 * time.Minute
	freeTierQuotaWindowMax = 24 * time.Hour
)

// quotaResetAfter reports how long the free tier's counting window still runs.
// A spent egress refills only when the counter resets at the next UTC midnight,
// so a spent exit must stay out of rotation until then instead of being retried
// (or re-registered onto) every cooldown period.
func quotaResetAfter(now time.Time) time.Duration {
	utc := now.UTC()
	next := time.Date(utc.Year(), utc.Month(), utc.Day()+1, 0, 0, 0, 0, time.UTC)
	remaining := next.Sub(utc)
	if remaining < freeTierQuotaWindowMin {
		return freeTierQuotaWindowMin
	}
	if remaining > freeTierQuotaWindowMax {
		return freeTierQuotaWindowMax
	}
	return remaining
}

// errDeterministicClientRejection reports that the upstream rejected the
// request itself: the body is malformed, too long for the model, or fails the
// same validation on every credential. Retrying on another key, lane or tier
// cannot change that, so the request must end and the original 4xx must reach
// the client. Without this, the first tier's real error is replaced by
// whatever the next tier answers — typically a 401 from an unconfigured key,
// which tells the client to re-authenticate instead of compacting.
var errDeterministicClientRejection = errors.New("upstream rejected the request deterministically")

// deterministicMarkers are the error types and codes an upstream uses for a
// request that will fail identically everywhere. The status must be 400 or 422
// as well: a 401, 403 or 429 can be specific to the credential or the moment.
var deterministicMarkers = []string{
	"invalid_request_error",
	"invalid_request",
	"context_length_exceeded",
	"maximum context length",
	"string_above_max_length",
	"prompt is too long",
}

// isDeterministicClientResponse reports whether status and body describe a
// request-level rejection rather than a credential or availability problem.
func isDeterministicClientResponse(status int, body []byte) bool {
	if status != http.StatusBadRequest && status != http.StatusUnprocessableEntity {
		return false
	}
	text := strings.ToLower(string(body))
	for _, marker := range deterministicMarkers {
		if strings.Contains(text, marker) {
			return true
		}
	}
	return false
}

// replayBody returns a body that yields prefix and then the rest of original.
func replayBody(prefix []byte, original io.ReadCloser) io.ReadCloser {
	return &prefixBody{Reader: io.MultiReader(bytes.NewReader(prefix), original), closer: original}
}

type prefixBody struct {
	io.Reader
	closer io.Closer
}

func (b *prefixBody) Close() error { return b.closer.Close() }
