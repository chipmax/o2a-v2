package gateway

import (
	"sync"
	"time"

	"opencode2api/internal/config"
)

// keyQuotaState holds one server key's live counters. The state is keyed by
// fingerprint and kept for the life of the process: a configuration reload
// must not hand a key a fresh budget.
type keyQuotaState struct {
	inflight int
	day      string
	used     int
}

// keyQuota enforces the per-key limit fields of config.ServerKeyMetadata.
type keyQuota struct {
	mu    sync.Mutex
	state map[string]*keyQuotaState
}

var quota = &keyQuota{state: map[string]*keyQuotaState{}}

// quotaRefusal describes why a request was refused and how long the client
// should wait before trying again.
type quotaRefusal struct {
	message    string
	code       string
	retryAfter time.Duration
}

func (q *keyQuota) stateLocked(id string) *keyQuotaState {
	item, ok := q.state[id]
	if !ok {
		item = &keyQuotaState{}
		q.state[id] = item
	}
	return item
}

func utcDay(now time.Time) string {
	return now.UTC().Format("2006-01-02")
}

// acquire counts one request against key id and returns the function that
// releases it, or a refusal when a limit is already reached. An unlimited
// policy still returns a no-op release.
func (q *keyQuota) acquire(id string, meta config.ServerKeyMetadata) (func(), *quotaRefusal) {
	if meta.MaxConcurrency <= 0 && meta.DailyRequests <= 0 {
		return func() {}, nil
	}
	now := time.Now()
	day := utcDay(now)
	q.mu.Lock()
	defer q.mu.Unlock()
	item := q.stateLocked(id)
	if item.day != day {
		item.day = day
		item.used = 0
	}
	if meta.DailyRequests > 0 && item.used >= meta.DailyRequests {
		next := time.Date(now.UTC().Year(), now.UTC().Month(), now.UTC().Day(), 0, 0, 0, 0, time.UTC).Add(24 * time.Hour)
		wait := next.Sub(now.UTC())
		if wait < time.Second {
			wait = time.Second
		}
		return nil, &quotaRefusal{
			message:    "daily request limit for this API key reached",
			code:       "key_daily_limit",
			retryAfter: wait,
		}
	}
	if meta.MaxConcurrency > 0 && item.inflight >= meta.MaxConcurrency {
		return nil, &quotaRefusal{
			message:    "too many concurrent requests for this API key",
			code:       "key_concurrency_limit",
			retryAfter: 2 * time.Second,
		}
	}
	item.inflight++
	item.used++
	released := false
	return func() {
		q.mu.Lock()
		defer q.mu.Unlock()
		if released {
			return
		}
		released = true
		if item.inflight > 0 {
			item.inflight--
		}
	}, nil
}

// snapshot reports the counters of key id, for the admin API.
func (q *keyQuota) snapshot(id string) (inflight, used int) {
	day := utcDay(time.Now())
	q.mu.Lock()
	defer q.mu.Unlock()
	item, ok := q.state[id]
	if !ok {
		return 0, 0
	}
	if item.day != day {
		return item.inflight, 0
	}
	return item.inflight, item.used
}
