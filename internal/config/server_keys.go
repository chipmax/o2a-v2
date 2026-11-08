package config

import "time"

// ServerKeyMetadata holds the WebUI-managed details of one server key: its
// display name, whether it is disabled, and when it was created. It is keyed
// by the existing key fingerprint; secret values remain in server_keys for
// compatibility with existing configuration and clients.
//
// AllowModels, MaxConcurrency and DailyRequests form the key's policy. Every
// policy field is optional and empty (or zero) means unlimited, so existing
// configurations keep their behaviour.
type ServerKeyMetadata struct {
	Name      string    `json:"name"`
	Disabled  bool      `json:"disabled,omitempty"`
	CreatedAt time.Time `json:"created_at,omitempty"`
	// AllowModels restricts the key to the listed model ids (and rotation
	// group ids). Empty means every model the gateway exposes.
	AllowModels []string `json:"allow_models,omitempty"`
	// MaxConcurrency caps the number of requests the key may have in flight.
	MaxConcurrency int `json:"max_concurrency,omitempty"`
	// DailyRequests caps how many requests the key may make per UTC day.
	DailyRequests int `json:"daily_requests,omitempty"`
}

// Policy returns the metadata of key, whether or not one is stored.
func (c Config) Policy(key string) ServerKeyMetadata {
	return c.ServerKeyMetadata[Fingerprint(key)]
}

// PolicyByID returns the metadata stored under a key fingerprint, which is
// what the gateway keeps in the request context after authentication.
func (c Config) PolicyByID(id string) ServerKeyMetadata {
	return c.ServerKeyMetadata[id]
}

// ModelAllowed reports whether the key's AllowModels policy admits model. An
// empty list admits every model.
func (m ServerKeyMetadata) ModelAllowed(model string) bool {
	if len(m.AllowModels) == 0 {
		return true
	}
	for _, allowed := range m.AllowModels {
		if allowed == model {
			return true
		}
	}
	return false
}

// ServerKeyEnabled reports whether key may authenticate clients. A key
// without metadata is enabled.
func (c Config) ServerKeyEnabled(key string) bool {
	return !c.ServerKeyMetadata[Fingerprint(key)].Disabled
}

// FirstEnabledServerKey returns the first enabled server key, or "" when every
// key is disabled. The Playground sends its requests with this key.
func (c Config) FirstEnabledServerKey() string {
	for _, key := range c.ServerKeys {
		if c.ServerKeyEnabled(key) {
			return key
		}
	}
	return ""
}
