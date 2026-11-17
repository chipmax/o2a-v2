package gateway

import (
	"io"
	"sync"
	"time"
)

// idleReadCloser closes its body when no bytes arrive for timeout. Every read
// that returns data restarts the timer, so a slow but progressing stream is
// never cut. The production timeout comes from
// performance.idle_timeout_seconds (default 5 minutes); a non-positive value
// disables the watchdog and returns the body unwrapped.
type idleReadCloser struct {
	body    io.ReadCloser
	timeout time.Duration
	onIdle  func()
	timer   *time.Timer
	once    sync.Once
}

func newIdleReadCloser(body io.ReadCloser, timeout time.Duration, onIdle func()) io.ReadCloser {
	if timeout <= 0 {
		// A disabled watchdog must not wrap the body: every read would pay for
		// a timer that can never fire.
		return body
	}
	reader := &idleReadCloser{body: body, timeout: timeout, onIdle: onIdle}
	reader.timer = time.AfterFunc(timeout, reader.expire)
	return reader
}

func (r *idleReadCloser) expire() {
	r.once.Do(func() {
		if r.onIdle != nil {
			r.onIdle()
		}
		_ = r.body.Close()
	})
}

func (r *idleReadCloser) Read(p []byte) (int, error) {
	n, err := r.body.Read(p)
	if n > 0 {
		r.timer.Reset(r.timeout)
	}
	return n, err
}

func (r *idleReadCloser) Close() error {
	r.timer.Stop()
	r.once.Do(func() {})
	return r.body.Close()
}
