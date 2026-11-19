package warp

import (
	"encoding/json"
	"os"
	"sync"
	"time"
)

// persistedRegistration is one WARP registration kept across restarts. A cold
// start otherwise has to register every exit again, and Cloudflare rate-limits
// registration, so a restart used to leave the pool empty for minutes. Reusing
// a saved registration skips the API call entirely; the tunnel is still probed
// by newExitWithStack, so a stale registration is discarded instead of used.
type persistedRegistration struct {
	ID            string   `json:"id"`
	Token         string   `json:"token,omitempty"`
	PrivateKey    string   `json:"private_key"`
	PublicKey     string   `json:"public_key,omitempty"`
	AddressV4     string   `json:"address_v4,omitempty"`
	AddressV6     string   `json:"address_v6"`
	PeerPublicKey string   `json:"peer_public_key"`
	Endpoint      string   `json:"endpoint,omitempty"`
	Endpoints     []string `json:"endpoints,omitempty"`
	// Drain marker: why and when this registration's egress was retired. It
	// survives a restart so a replacement does not land on a known-spent /64
	// simply because the cooling map lived only in memory.
	EgressKey   string    `json:"egress_key,omitempty"`
	DrainReason string    `json:"drain_reason,omitempty"`
	DrainedAt   time.Time `json:"drained_at,omitempty"`
}

func persistRegistration(reg registration) persistedRegistration {
	return persistedRegistration{
		ID:            reg.ID,
		Token:         reg.Token,
		PrivateKey:    reg.PrivateKey,
		PublicKey:     reg.PublicKey,
		AddressV4:     reg.AddressV4,
		AddressV6:     reg.AddressV6,
		PeerPublicKey: reg.PeerPublicKey,
		Endpoint:      reg.Endpoint,
		Endpoints:     reg.Endpoints,
	}
}

// markDrain records a spent/duplicate egress on the saved registration so a
// later process can avoid reusing the same prefix. The tunnel identity is
// untouched; only the marker fields change.
func (s *registrationStore) markDrain(id, egressKey, reason string, at time.Time) {
	if s.path == "" || id == "" || egressKey == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	for i := range s.known {
		if s.known[i].ID != id {
			continue
		}
		s.known[i].EgressKey = egressKey
		s.known[i].DrainReason = reason
		s.known[i].DrainedAt = at.UTC()
		s.writeLocked()
		return
	}
}

// DrainedKeys returns egress prefixes marked spent by an earlier process. The
// pool uses them to seed its cooling map at startup.
func (s *registrationStore) DrainedKeys(now time.Time, ttl time.Duration) map[string]time.Time {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make(map[string]time.Time)
	for _, saved := range s.known {
		if saved.EgressKey == "" || saved.DrainedAt.IsZero() || now.Sub(saved.DrainedAt) > ttl {
			continue
		}
		until := saved.DrainedAt.Add(ttl)
		if until.After(out[saved.EgressKey]) {
			out[saved.EgressKey] = until
		}
	}
	return out
}

func (p persistedRegistration) registration() registration {
	return registration{
		ID:            p.ID,
		Token:         p.Token,
		PrivateKey:    p.PrivateKey,
		PublicKey:     p.PublicKey,
		AddressV4:     p.AddressV4,
		AddressV6:     p.AddressV6,
		PeerPublicKey: p.PeerPublicKey,
		Endpoint:      p.Endpoint,
		Endpoints:     p.Endpoints,
	}
}

// registrationStore keeps WARP registrations on disk so the next process start
// can rebuild the pool without registering again. An empty path keeps the
// store purely in memory.
type registrationStore struct {
	mu      sync.Mutex
	path    string
	limit   int
	pending []registration
	known   []persistedRegistration
	// probeUntil bounds how long saved registrations are tried at startup. A
	// registration saved by the process that just exited is often still bound
	// to that process's tunnel session, so probing all of them one by one can
	// delay the first listener by minutes. After this window the pool
	// registers fresh exits instead, and the saved ones stay on disk for the
	// next start.
	probeUntil time.Time
}

func newRegistrationStore(path string, limit int) *registrationStore {
	store := &registrationStore{path: path, limit: limit}
	if path == "" {
		return store
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return store
	}
	var file struct {
		Version int                     `json:"version"`
		SavedAt time.Time               `json:"saved_at,omitempty"`
		Exits   []persistedRegistration `json:"exits"`
	}
	if err := json.Unmarshal(data, &file); err != nil {
		return store
	}
	store.known = file.Exits
	store.pending = make([]registration, 0, len(file.Exits))
	for _, saved := range file.Exits {
		if saved.PrivateKey == "" || saved.AddressV6 == "" || saved.PeerPublicKey == "" {
			continue
		}
		store.pending = append(store.pending, saved.registration())
	}
	if len(store.pending) > 0 {
		store.probeUntil = time.Now().Add(savedProbeWindow)
		// A saved file written moments ago belongs to the process that just
		// exited: its tunnel sessions are gone, so probing now only spends the
		// startup budget on certain failures. Skip straight to fresh
		// registrations and keep the file for the next start.
		if !file.SavedAt.IsZero() && time.Since(file.SavedAt) < quickRestartWindow {
			store.probeUntil = time.Now()
		}
	}
	return store
}

// writeLocked persists the known registrations. The caller must hold s.mu.
func (s *registrationStore) writeLocked() {
	payload, err := json.MarshalIndent(struct {
		Version int                     `json:"version"`
		SavedAt time.Time               `json:"saved_at,omitempty"`
		Exits   []persistedRegistration `json:"exits"`
	}{Version: 1, SavedAt: time.Now().UTC(), Exits: s.known}, "", "  ")
	if err != nil {
		return
	}
	tmp := s.path + ".tmp"
	if err := os.WriteFile(tmp, payload, 0o600); err != nil {
		return
	}
	if err := os.Rename(tmp, s.path); err != nil {
		_ = os.Remove(tmp)
	}
}

// quickRestartWindow is the age below which saved registrations are known to be
// bound to the previous process's tunnel sessions.
const quickRestartWindow = 45 * time.Second

// savedProbeWindow bounds how long startup spends re-probing saved
// registrations. Measured on this deployment, a registration saved by a process
// that has exited does not carry traffic again (its WireGuard session is gone),
// so each probe costs its full timeout and buys nothing: the window is small and
// the pool registers fresh exits after it.
const savedProbeWindow = 4 * time.Second

// pop returns a saved registration, if one is still queued and the startup
// probe window is still open.
func (s *registrationStore) pop() (registration, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.pending) == 0 {
		return registration{}, false
	}
	if !s.probeUntil.IsZero() && time.Now().After(s.probeUntil) {
		return registration{}, false
	}
	reg := s.pending[0]
	s.pending = s.pending[1:]
	return reg, true
}

// anyKnown returns one saved registration usable as a key-rotation anchor.
// Unlike pop() it ignores the startup probe window: rotating a device's key is
// an API call, not a tunnel probe, so an old saved registration is still a valid
// anchor. The caller rotates it before building an exit from the result.
func (s *registrationStore) anyKnown() (registration, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, saved := range s.known {
		if saved.Token == "" || saved.PrivateKey == "" || saved.AddressV6 == "" {
			continue
		}
		return saved.registration(), true
	}
	return registration{}, false
}

// remember records a registration that produced a working exit so the next
// start can reuse it. It is a no-op without a path.
func (s *registrationStore) remember(reg registration) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.path == "" || reg.PrivateKey == "" || reg.AddressV6 == "" || reg.PeerPublicKey == "" {
		return
	}
	saved := persistRegistration(reg)
	kept := make([]persistedRegistration, 0, len(s.known)+1)
	kept = append(kept, saved)
	for _, old := range s.known {
		if old.ID != saved.ID {
			kept = append(kept, old)
		}
	}
	if s.limit > 0 && len(kept) > s.limit {
		kept = kept[:s.limit]
	}
	s.known = kept
	s.writeLocked()
}
