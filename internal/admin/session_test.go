package admin

import (
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// TestSessionsSurviveRestart covers the reason the operator had to log in
// after every deploy: the session map used to live only in memory.
func TestSessionsSurviveRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))

	first := New(nil, nil, nil, logger)
	first.SetSessionPath(path)
	first.mu.Lock()
	first.sessions["digest-live"] = adminSession{Username: "admin", AuthVersion: "v1", CSRF: "csrf", Expires: time.Now().Add(time.Hour)}
	first.sessions["digest-expired"] = adminSession{Username: "admin", AuthVersion: "v1", CSRF: "csrf", Expires: time.Now().Add(-time.Minute)}
	first.saveSessionsLocked()
	first.mu.Unlock()

	second := New(nil, nil, nil, logger)
	second.SetSessionPath(path)
	if _, ok := second.sessions["digest-live"]; !ok {
		t.Fatal("live session was not restored")
	}
	if _, ok := second.sessions["digest-expired"]; ok {
		t.Fatal("expired session was restored")
	}
}

// TestSessionFileIsPrivate keeps the CSRF tokens out of other users' reach.
func TestSessionFileIsPrivate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sessions.json")
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	server := New(nil, nil, nil, logger)
	server.SetSessionPath(path)
	server.mu.Lock()
	server.sessions["digest"] = adminSession{Username: "admin", Expires: time.Now().Add(time.Hour)}
	server.saveSessionsLocked()
	server.mu.Unlock()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat: %v", err)
	}
	if mode := info.Mode().Perm(); mode != 0o600 {
		t.Fatalf("mode = %o, want 600", mode)
	}
}
