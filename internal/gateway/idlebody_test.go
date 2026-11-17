package gateway

import (
	"io"
	"testing"
	"time"
)

// TestIdleReadCloserCutsAStalledStream covers the production shape the wrapper
// exists for: headers arrived, then the upstream stopped sending. The read must
// return instead of waiting out the whole request budget.
func TestIdleReadCloserCutsAStalledStream(t *testing.T) {
	reader, writer := io.Pipe()
	defer writer.Close()
	fired := make(chan struct{}, 1)
	body := newIdleReadCloser(reader, 50*time.Millisecond, func() { fired <- struct{}{} })
	defer body.Close()

	done := make(chan error, 1)
	go func() {
		_, err := body.Read(make([]byte, 16))
		done <- err
	}()
	select {
	case <-fired:
	case <-time.After(2 * time.Second):
		t.Fatal("idle timer did not fire")
	}
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("read returned nil after the body was closed for idling")
		}
	case <-time.After(2 * time.Second):
		t.Fatal("read did not return after the idle close")
	}
}

// TestIdleReadCloserKeepsProgressingStream pins that a slow stream is not cut:
// each byte restarts the timer.
func TestIdleReadCloserKeepsProgressingStream(t *testing.T) {
	reader, writer := io.Pipe()
	fired := make(chan struct{}, 1)
	body := newIdleReadCloser(reader, 100*time.Millisecond, func() { fired <- struct{}{} })
	defer body.Close()

	go func() {
		for index := 0; index < 4; index++ {
			_, _ = writer.Write([]byte("x"))
			time.Sleep(40 * time.Millisecond)
		}
		_ = writer.Close()
	}()
	payload, err := io.ReadAll(body)
	if err != nil {
		t.Fatalf("read: %v", err)
	}
	if len(payload) != 4 {
		t.Fatalf("read %d bytes, want 4", len(payload))
	}
	select {
	case <-fired:
		t.Fatal("idle timer fired during a progressing stream")
	default:
	}
}
