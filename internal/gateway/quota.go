package gateway

import (
	"encoding/json"
	"os"
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
	mu       sync.Mutex
	state    map[string]*keyQuotaState
	path     string
	lastSave time.Time
}

// quota is process-wide: a hot configuration reload must not hand a key a
// fresh daily budget. The path is attached once by RuntimeManager so the
// counters survive a restart as well.
var quota = &keyQuota{state: map[string]*keyQuotaState{}}

type quotaFile struct {
	Version int                       `json:"version"`
	SavedAt time.Time                 `json:"saved_at"`
	Keys    map[string]quotaFileEntry `json:"keys"`
}

type quotaFileEntry struct {
	Day  string `json:"day"`
	Used int    `json:"used"`
}

// ConfigureQuotaState attaches the persistence file and loads the counters of
// the current UTC day. A missing or malformed file starts empty; inflight
// requests are deliberately not restored.
func ConfigureQuotaState(path string) {
	quota.configurePath(path)
}

// SaveQuotaState flushes the counters; shutdown calls it after the listeners
// have closed.
func SaveQuotaState() { quota.save() }

func (q *keyQuota) configurePath(path string) {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.path == path {
		return
	}
	q.path = path
	if path == "" {
		return
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return
	}
	var file quotaFile
	if json.Unmarshal(data, &file) != nil || file.Version != 1 {
		return
	}
	today := utcDay(time.Now())
	for id, item := range file.Keys {
		used := item.Used
		if item.Day != today {
			used = 0
		}
		q.state[id] = &keyQuotaState{day: today, used: used}
	}
}

func (q *keyQuota) save() {
	q.mu.Lock()
	defer q.mu.Unlock()
	q.saveLocked(time.Now())
}

func (q *keyQuota) saveLocked(now time.Time) {
	if q.path == "" {
		return
	}
	file := quotaFile{Version: 1, SavedAt: now.UTC(), Keys: make(map[string]quotaFileEntry, len(q.state))}
	for id, item := range q.state {
		if item.used == 0 {
			continue
		}
		file.Keys[id] = quotaFileEntry{Day: item.day, Used: item.used}
	}
	data, err := json.Marshal(file)
	if err != nil {
		return
	}
	tmp := q.path + ".tmp"
	if os.WriteFile(tmp, data, 0o600) != nil {
		return
	}
	if err := os.Rename(tmp, q.path); err != nil {
		_ = os.Remove(tmp)
		return
	}
	q.lastSave = now
}

func (q *keyQuota) maybeSaveLocked(now time.Time) {
	if q.path == "" || now.Sub(q.lastSave) < 10*time.Second {
		return
	}
	q.saveLocked(now)
}

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
	q.maybeSaveLocked(now)
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
		q.maybeSaveLocked(time.Now())
	}, nil
}

// refund returns one daily request slot after the peer path failed without
// writing a response, so an unknown-model 404 does not consume the budget.
func (q *keyQuota) refund(id string) {
	now := time.Now()
	day := utcDay(now)
	q.mu.Lock()
	defer q.mu.Unlock()
	item, ok := q.state[id]
	if !ok {
		return
	}
	if item.day != day {
		item.day = day
		item.used = 0
		return
	}
	if item.used > 0 {
		item.used--
	}
	q.maybeSaveLocked(now)
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
