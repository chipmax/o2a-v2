package gateway

import (
	"sync"
	"time"
)

// affinityTTL bounds how long a session is pinned to one egress /64. Pinning
// keeps the prompt-cache prefix warm, but a long session must eventually move
// so one bucket does not drain alone.
const affinityTTL = 30 * time.Minute

type affinityEntry struct {
	egress string
	at     time.Time
}

// egressAffinity remembers the last usable egress key per session.
type egressAffinity struct {
	mu      sync.Mutex
	entries map[string]affinityEntry
}

func newEgressAffinity() *egressAffinity {
	return &egressAffinity{entries: make(map[string]affinityEntry)}
}

func (a *egressAffinity) preferred(session string, now time.Time) string {
	if a == nil || session == "" {
		return ""
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	entry, ok := a.entries[session]
	if !ok || now.Sub(entry.at) > affinityTTL {
		delete(a.entries, session)
		return ""
	}
	return entry.egress
}

func (a *egressAffinity) remember(session, egress string, now time.Time) {
	if a == nil || session == "" || egress == "" {
		return
	}
	a.mu.Lock()
	a.entries[session] = affinityEntry{egress: egress, at: now}
	if len(a.entries) > 2048 {
		cutoff := now.Add(-affinityTTL)
		for key, entry := range a.entries {
			if entry.at.Before(cutoff) {
				delete(a.entries, key)
			}
		}
	}
	a.mu.Unlock()
}

func (a *egressAffinity) forget(session string) {
	if a == nil || session == "" {
		return
	}
	a.mu.Lock()
	delete(a.entries, session)
	a.mu.Unlock()
}
