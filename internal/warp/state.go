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
		// exited: Cloudflare still holds its tunnel sessions, so probing now
		// only spends the startup budget on failures. Skip straight to fresh
		// registrations and keep the file for the next start.
		if !file.SavedAt.IsZero() && time.Since(file.SavedAt) < quickRestartWindow {
			store.probeUntil = time.Now()
		}
	}
	return store
}

// quickRestartWindow is the age below which saved registrations are assumed
// still bound to the previous process's tunnel sessions.
const quickRestartWindow = 45 * time.Second

// savedProbeWindow is how long startup may spend trying saved registrations
// before it starts registering fresh exits.
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
	payload, err := json.MarshalIndent(struct {
		Version int                     `json:"version"`
		SavedAt time.Time               `json:"saved_at,omitempty"`
		Exits   []persistedRegistration `json:"exits"`
	}{Version: 1, SavedAt: time.Now().UTC(), Exits: kept}, "", "  ")
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
