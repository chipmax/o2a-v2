package telemetry

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"time"

	_ "modernc.org/sqlite"
)

// usageSchema keeps one row per routed request. It is deliberately independent
// from the in-memory monitor, so the last week of usage survives restarts and
// can be queried without holding the monitor lock.
const usageSchema = `
CREATE TABLE IF NOT EXISTS usage_requests (
	id INTEGER PRIMARY KEY AUTOINCREMENT,
	ts_ms INTEGER NOT NULL,
	request_id TEXT NOT NULL,
	session TEXT NOT NULL DEFAULT '',
	server_key_id TEXT NOT NULL DEFAULT '',
	model TEXT NOT NULL,
	tier TEXT NOT NULL DEFAULT '',
	key_id TEXT NOT NULL DEFAULT '',
	channel TEXT NOT NULL DEFAULT '',
	anonymous INTEGER NOT NULL DEFAULT 0,
	proxy TEXT NOT NULL DEFAULT '',
	attempts INTEGER NOT NULL DEFAULT 0,
	status INTEGER NOT NULL DEFAULT 0,
	duration_ms INTEGER NOT NULL DEFAULT 0,
	success INTEGER NOT NULL DEFAULT 0,
	outcome TEXT NOT NULL DEFAULT '',
	input_tokens INTEGER NOT NULL DEFAULT 0,
	output_tokens INTEGER NOT NULL DEFAULT 0,
	cached_tokens INTEGER NOT NULL DEFAULT 0,
	reasoning_tokens INTEGER NOT NULL DEFAULT 0,
	total_tokens INTEGER NOT NULL DEFAULT 0
);
CREATE INDEX IF NOT EXISTS usage_requests_ts ON usage_requests(ts_ms);
CREATE INDEX IF NOT EXISTS usage_requests_model_ts ON usage_requests(model, ts_ms);
`

const (
	usageQueueCapacity = 2048
	usageBatchSize     = 100
	usageFlushInterval = time.Second
)

// UsageStore persists completed requests to SQLite. Appends go through a
// bounded, non-blocking queue so a slow disk never stalls an inference
// response; when the queue is full the newest record is dropped and counted.
type UsageStore struct {
	db      *sql.DB
	logger  *slog.Logger
	queue   chan UpstreamRequest
	closeMu sync.Mutex
	closed  bool
	wg      sync.WaitGroup
	dropped atomic.Uint64
}

// OpenUsageStore opens path, creates the schema, and starts the writer. An
// empty path returns nil without an error, which disables persistence.
func OpenUsageStore(path string, logger *slog.Logger) (*UsageStore, error) {
	if path == "" {
		return nil, nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." && path != ":memory:" {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return nil, fmt.Errorf("create usage database directory: %w", err)
		}
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open usage database: %w", err)
	}
	db.SetMaxOpenConns(1)
	for _, pragma := range []string{
		"PRAGMA journal_mode=WAL",
		"PRAGMA synchronous=NORMAL",
		"PRAGMA busy_timeout=5000",
	} {
		if _, err := db.Exec(pragma); err != nil {
			db.Close()
			return nil, fmt.Errorf("configure usage database: %w", err)
		}
	}
	if _, err := db.Exec(usageSchema); err != nil {
		db.Close()
		return nil, fmt.Errorf("create usage schema: %w", err)
	}
	if path != ":memory:" {
		for _, suffix := range []string{"", "-wal", "-shm"} {
			if err := os.Chmod(path+suffix, 0o600); err != nil && !os.IsNotExist(err) {
				db.Close()
				return nil, fmt.Errorf("restrict usage database permissions: %w", err)
			}
		}
	}
	store := &UsageStore{
		db:     db,
		logger: logger,
		queue:  make(chan UpstreamRequest, usageQueueCapacity),
	}
	store.wg.Add(1)
	go store.run()
	return store, nil
}

// Append queues a completed request without blocking the caller.
func (s *UsageStore) Append(request UpstreamRequest) {
	if s == nil {
		return
	}
	s.closeMu.Lock()
	defer s.closeMu.Unlock()
	if s.closed {
		return
	}
	select {
	case s.queue <- request:
	default:
		s.dropped.Add(1)
	}
}

// Dropped reports how many records were discarded because the writer could not
// keep up. A sustained non-zero value means the disk is too slow for the load.
func (s *UsageStore) Dropped() uint64 {
	if s == nil {
		return 0
	}
	return s.dropped.Load()
}

// Close stops the writer after draining queued records.
func (s *UsageStore) Close() error {
	if s == nil {
		return nil
	}
	s.closeMu.Lock()
	if s.closed {
		s.closeMu.Unlock()
		return nil
	}
	s.closed = true
	close(s.queue)
	s.closeMu.Unlock()
	s.wg.Wait()
	return s.db.Close()
}

func (s *UsageStore) run() {
	defer s.wg.Done()
	ticker := time.NewTicker(usageFlushInterval)
	defer ticker.Stop()
	batch := make([]UpstreamRequest, 0, usageBatchSize)
	flush := func() {
		if len(batch) == 0 {
			return
		}
		if err := s.insert(batch); err != nil {
			s.logger.Warn("failed to persist usage", "component", "usage", "event", "usage_write_failed", "rows", len(batch), "error", err)
		}
		batch = batch[:0]
	}
	for {
		select {
		case request, ok := <-s.queue:
			if !ok {
				flush()
				return
			}
			batch = append(batch, request)
			if len(batch) >= usageBatchSize {
				flush()
			}
		case <-ticker.C:
			flush()
		}
	}
}

func (s *UsageStore) insert(batch []UpstreamRequest) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	statement, err := tx.PrepareContext(ctx, `
		INSERT INTO usage_requests (
			ts_ms, request_id, session, server_key_id, model, tier, key_id, channel,
			anonymous, proxy, attempts, status, duration_ms, success, outcome,
			input_tokens, output_tokens, cached_tokens, reasoning_tokens, total_tokens
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
	`)
	if err != nil {
		tx.Rollback()
		return err
	}
	defer statement.Close()
	for _, request := range batch {
		anonymous := 0
		if request.Anonymous {
			anonymous = 1
		}
		success := 0
		if request.Success {
			success = 1
		}
		if _, err := statement.ExecContext(ctx,
			request.Time.UnixMilli(), request.RequestID, request.Session, request.ServerKeyID,
			request.Model, request.Tier, request.KeyID, request.Channel, anonymous,
			request.Proxy, request.Attempts, request.Status, request.DurationMS, success,
			request.Outcome, request.InputTokens, request.OutputTokens, request.CachedTokens,
			request.ReasoningTokens, request.TotalTokens,
		); err != nil {
			tx.Rollback()
			return err
		}
	}
	return tx.Commit()
}
