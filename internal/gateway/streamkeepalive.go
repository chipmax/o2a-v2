package gateway

import (
	"bytes"
	"io"
	"net/http"
	"sync"
	"sync/atomic"
	"time"

	wire "opencode2api/internal/protocol"
)

// streamFrames holds the two client-invisible frames a committed stream needs:
// the keepalive that proves the connection is alive during a long reasoning
// turn, and the in-band error that ends the turn when the upstream went
// silent. Both are per wire protocol because an Anthropic client reads named
// events while an OpenAI client reads data-only frames.
type streamFrames struct {
	keepalive string
	idleError string
}

func framesFor(protocol wire.Protocol, idle time.Duration) streamFrames {
	seconds := int(idle.Seconds())
	if protocol == wire.Anthropic {
		return streamFrames{
			keepalive: "event: ping\ndata: {\"type\":\"ping\"}\n\n",
			idleError: "event: error\ndata: {\"type\":\"error\",\"error\":{\"type\":\"upstream_idle_timeout\",\"message\":\"the upstream produced no data for " +
				itoa(seconds) + "s\"}}\n\n",
		}
	}
	return streamFrames{
		keepalive: ": keepalive\n\n",
		idleError: "data: {\"error\":{\"type\":\"upstream_idle_timeout\",\"code\":\"upstream_idle_timeout\",\"message\":\"the upstream produced no data for " +
			itoa(seconds) + "s\"}}\n\n",
	}
}

// itoa avoids pulling strconv in for two call sites while keeping the frame a
// compile-time-shaped literal.
func itoa(value int) string {
	if value <= 0 {
		return "0"
	}
	var digits [20]byte
	index := len(digits)
	for value > 0 {
		index--
		digits[index] = byte('0' + value%10)
		value /= 10
	}
	return string(digits[index:])
}

// streamKeepalive wraps the response writer of a committed stream. It writes a
// keepalive frame after interval of silence, and can end the turn with an
// in-band error when the upstream produced nothing. Every write holds the same
// mutex, so a keepalive frame can never interleave with an event the forwarder
// is writing.
type streamKeepalive struct {
	mu       sync.Mutex
	dst      http.ResponseWriter
	flusher  http.Flusher
	frames   streamFrames
	interval time.Duration
	timer    *time.Timer
	stopped  bool
	idle     atomic.Bool
	// pending keeps the trailing bytes of an incomplete SSE line. A keepalive
	// frame may only be inserted between complete lines; writing one in the
	// middle of an event (io.Copy hands over arbitrary chunk boundaries) would
	// turn the blank line that ends the frame into the end of a broken event.
	pending []byte
}

// newStreamKeepalive starts the keepalive timer when interval is positive and
// the writer can flush. A nil interval writer is still safe to use: every
// write passes through.
func newStreamKeepalive(w http.ResponseWriter, protocol wire.Protocol, interval, idle time.Duration) *streamKeepalive {
	keeper := &streamKeepalive{dst: w, frames: framesFor(protocol, idle), interval: interval}
	keeper.flusher, _ = w.(http.Flusher)
	if keeper.flusher != nil && interval > 0 {
		keeper.timer = time.AfterFunc(interval, keeper.tick)
	}
	return keeper
}

// Header keeps the wrapper a full http.ResponseWriter for the forwarders,
// which look at response headers (trailers, content type) while they stream.
func (k *streamKeepalive) Header() http.Header { return k.dst.Header() }

// WriteHeader forwards to the wrapped writer. The stream handler commits its
// headers before wrapping, so this is normally a no-op.
func (k *streamKeepalive) WriteHeader(status int) { k.dst.WriteHeader(status) }

func (k *streamKeepalive) Write(p []byte) (int, error) {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.stopped {
		return k.dst.Write(p)
	}
	k.pending = append(k.pending, p...)
	// Emit everything up to the last complete line; keep the tail until more
	// bytes arrive (or the stream ends), so a comment can never split a frame.
	if cut := bytes.LastIndexByte(k.pending, '\n'); cut >= 0 {
		if _, err := k.dst.Write(k.pending[:cut+1]); err != nil {
			return 0, err
		}
		k.pending = k.pending[:0]
	}
	if k.flusher != nil {
		k.flusher.Flush()
	}
	if k.timer != nil {
		k.timer.Reset(k.interval)
	}
	return len(p), nil
}

// Flush keeps the wrapper an http.Flusher, so a forwarder that flushes after
// each event keeps flushing through it.
func (k *streamKeepalive) Flush() {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.flusher != nil {
		k.flusher.Flush()
	}
}

func (k *streamKeepalive) tick() {
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.stopped {
		return
	}
	if len(k.pending) > 0 {
		// A partial line means bytes are still flowing; a comment here would
		// corrupt it. The next Write resets the timer anyway.
		k.timer.Reset(k.interval)
		return
	}
	_, _ = io.WriteString(k.dst, k.frames.keepalive)
	if k.flusher != nil {
		k.flusher.Flush()
	}
	k.timer.Reset(k.interval)
}

// WriteIdle ends the turn with an in-band upstream_idle_timeout error. It is
// called from the idle watchdog just before the upstream body is closed, so
// the caller sees a named failure instead of a stream that simply stops.
func (k *streamKeepalive) WriteIdle() {
	k.idle.Store(true)
	k.mu.Lock()
	defer k.mu.Unlock()
	if k.stopped {
		return
	}
	k.flushPendingLocked()
	_, _ = io.WriteString(k.dst, k.frames.idleError)
	if k.flusher != nil {
		k.flusher.Flush()
	}
}

// IdleTripped reports whether the watchdog ended this stream, so telemetry can
// name the outcome instead of calling it a client cancel.
func (k *streamKeepalive) IdleTripped() bool { return k.idle.Load() }

// Stop releases the timer and writes any retained tail. The handler defers it,
// so no frame is written after the stream is done.
func (k *streamKeepalive) Stop() {
	k.mu.Lock()
	defer k.mu.Unlock()
	k.stopped = true
	k.flushPendingLocked()
	if k.timer != nil {
		k.timer.Stop()
	}
}

// flushPendingLocked writes the retained tail, if any. The caller holds mu.
func (k *streamKeepalive) flushPendingLocked() {
	if len(k.pending) == 0 {
		return
	}
	_, _ = k.dst.Write(k.pending)
	k.pending = k.pending[:0]
}
