package admin

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"time"

	"opencode2api/internal/config"
	"opencode2api/internal/httpx"
	"opencode2api/internal/telemetry"
)

// APIKeyView is a server key as the API keys page lists it: its Fingerprint
// ID, its name (a numbered placeholder when none is stored), the masked value,
// whether it is enabled, its policy and its usage.
type APIKeyView struct {
	ID             string      `json:"id"`
	Name           string      `json:"name"`
	Display        string      `json:"display"`
	Enabled        bool        `json:"enabled"`
	CreatedAt      *time.Time  `json:"created_at,omitempty"`
	AllowModels    []string    `json:"allow_models"`
	MaxConcurrency int         `json:"max_concurrency"`
	DailyRequests  int         `json:"daily_requests"`
	Usage          APIKeyUsage `json:"usage"`
}

// APIKeyUsage is in-memory like the rest of telemetry.Metrics: it restarts
// from zero with the process and survives hot reloads. Window covers the last
// hour.
type APIKeyUsage struct {
	Lifetime telemetry.ServerKeyUsage `json:"lifetime"`
	Window   telemetry.ServerKeyUsage `json:"last_hour"`
}

func (a *Server) apiKeyViews(cfg config.Config) []APIKeyView {
	lifetime, window := a.monitor.ServerKeyUsage()
	items := make([]APIKeyView, 0, len(cfg.ServerKeys))
	for i, key := range cfg.ServerKeys {
		id := config.Fingerprint(key)
		meta := cfg.ServerKeyMetadata[id]
		name := meta.Name
		if name == "" {
			name = fmt.Sprintf("已有 Key %d", i+1)
		}
		item := APIKeyView{
			ID: id, Name: name, Display: config.MaskValue(key), Enabled: !meta.Disabled,
			AllowModels:    meta.AllowModels,
			MaxConcurrency: meta.MaxConcurrency,
			DailyRequests:  meta.DailyRequests,
			Usage:          APIKeyUsage{Lifetime: lifetime[id], Window: window[id]},
		}
		if item.AllowModels == nil {
			item.AllowModels = []string{}
		}
		if !meta.CreatedAt.IsZero() {
			created := meta.CreatedAt
			item.CreatedAt = &created
		}
		items = append(items, item)
	}
	return items
}

func (a *Server) handleListAPIKeys(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Cache-Control", "no-store")
	httpx.WriteJSON(w, http.StatusOK, map[string]any{"keys": a.apiKeyViews(a.manager.Config())})
}

// validateKeyName requires a name of 1-80 characters.
func validateKeyName(name string) error {
	if name == "" || len([]rune(name)) > 80 {
		return errors.New("name must be 1-80 characters")
	}
	return nil
}

// cleanAllowModels trims, de-duplicates and bounds a model allow-list.
func cleanAllowModels(values []string) ([]string, error) {
	seen := make(map[string]bool, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if len([]rune(value)) > 120 {
			return nil, errors.New("model ids in the allow list must be at most 120 characters")
		}
		if seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	if len(out) > 64 {
		return nil, errors.New("an allow list holds at most 64 models")
	}
	return out, nil
}

// addAPIKey appends key as a new server key named name. The name must have
// 1-80 characters and the key 16-256 characters from [A-Za-z0-9._-]; at most
// 200 keys are allowed, and a key whose fingerprint already exists is refused.
func addAPIKey(cfg *config.Config, name, key string) error {
	if err := validateKeyName(name); err != nil {
		return err
	}
	if len(key) < 16 || len(key) > 256 {
		return errors.New("a custom key must be 16-256 characters")
	}
	for _, ch := range key {
		if !((ch >= 'a' && ch <= 'z') || (ch >= 'A' && ch <= 'Z') || (ch >= '0' && ch <= '9') || ch == '-' || ch == '_' || ch == '.') {
			return errors.New("a key accepts letters, digits, '-', '_' and '.' only")
		}
	}
	if len(cfg.ServerKeys) >= 200 {
		return errors.New("at most 200 API keys are allowed")
	}
	id := config.Fingerprint(key)
	for _, existing := range cfg.ServerKeys {
		if config.Fingerprint(existing) == id {
			return errors.New("this API key already exists")
		}
	}
	if cfg.ServerKeyMetadata == nil {
		cfg.ServerKeyMetadata = map[string]config.ServerKeyMetadata{}
	}
	cfg.ServerKeys = append(cfg.ServerKeys, key)
	cfg.ServerKeyMetadata[id] = config.ServerKeyMetadata{Name: name, CreatedAt: time.Now().UTC()}
	return nil
}

// handleCreateAPIKey adds a server key, generating "sk-local-" plus 48 hex
// digits when the request carries no value, and answers with the plaintext
// key next to the updated, masked list.
func (a *Server) handleCreateAPIKey(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Name  string `json:"name"`
		Value string `json:"value"`
	}
	if err := decodeAdminJSON(w, r, &input); err != nil {
		writeAdminError(w, 400, "invalid_request", err.Error())
		return
	}
	key := input.Value
	if key == "" {
		var random [24]byte
		if _, err := rand.Read(random[:]); err != nil {
			writeAdminError(w, 500, "generation_failed", "cannot generate a key")
			return
		}
		key = "sk-local-" + hex.EncodeToString(random[:])
	}
	_, err := a.manager.Update(func(cfg *config.Config) error { return addAPIKey(cfg, strings.TrimSpace(input.Name), key) })
	if err != nil {
		writeAdminError(w, 400, "key_create_failed", a.manager.Redact(err.Error()))
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	httpx.WriteJSON(w, 201, map[string]any{"key": key, "keys": a.apiKeyViews(a.manager.Config())})
}

// keyPolicyPatch carries the optional policy fields of a PATCH request. A nil
// field leaves the stored value unchanged.
type keyPolicyPatch struct {
	AllowModels    *[]string
	MaxConcurrency *int
	DailyRequests  *int
}

// changeAPIKey renames, enables or disables, updates the policy of, or (with
// remove) deletes the server key whose fingerprint is id. A change that would
// leave no enabled key is refused.
func changeAPIKey(cfg *config.Config, id string, name *string, enabled *bool, patch keyPolicyPatch, remove bool) error {
	index := -1
	for i, key := range cfg.ServerKeys {
		if config.Fingerprint(key) == id {
			index = i
			break
		}
	}
	if index < 0 {
		return errors.New("API key not found; refresh the list")
	}
	if cfg.ServerKeyMetadata == nil {
		cfg.ServerKeyMetadata = map[string]config.ServerKeyMetadata{}
	}
	if remove {
		cfg.ServerKeys = append(cfg.ServerKeys[:index], cfg.ServerKeys[index+1:]...)
		delete(cfg.ServerKeyMetadata, id)
	} else {
		meta := cfg.ServerKeyMetadata[id]
		if name != nil {
			value := strings.TrimSpace(*name)
			if err := validateKeyName(value); err != nil {
				return err
			}
			meta.Name = value
		}
		if enabled != nil {
			meta.Disabled = !*enabled
		}
		if patch.AllowModels != nil {
			allowed, err := cleanAllowModels(*patch.AllowModels)
			if err != nil {
				return err
			}
			meta.AllowModels = allowed
		}
		if patch.MaxConcurrency != nil {
			value := *patch.MaxConcurrency
			if value < 0 || value > 256 {
				return errors.New("max concurrency must be between 0 and 256 (0 = unlimited)")
			}
			meta.MaxConcurrency = value
		}
		if patch.DailyRequests != nil {
			value := *patch.DailyRequests
			if value < 0 || value > 1000000 {
				return errors.New("daily requests must be between 0 and 1000000 (0 = unlimited)")
			}
			meta.DailyRequests = value
		}
		cfg.ServerKeyMetadata[id] = meta
	}
	if cfg.FirstEnabledServerKey() == "" {
		return errors.New("keep at least one enabled API key; create a replacement first")
	}
	return nil
}

func (a *Server) handleUpdateAPIKey(w http.ResponseWriter, r *http.Request) {
	var input struct {
		Name           *string   `json:"name"`
		Enabled        *bool     `json:"enabled"`
		AllowModels    *[]string `json:"allow_models"`
		MaxConcurrency *int      `json:"max_concurrency"`
		DailyRequests  *int      `json:"daily_requests"`
	}
	if err := decodeAdminJSON(w, r, &input); err != nil {
		writeAdminError(w, 400, "invalid_request", err.Error())
		return
	}
	patch := keyPolicyPatch{AllowModels: input.AllowModels, MaxConcurrency: input.MaxConcurrency, DailyRequests: input.DailyRequests}
	_, err := a.manager.Update(func(cfg *config.Config) error {
		return changeAPIKey(cfg, r.PathValue("id"), input.Name, input.Enabled, patch, false)
	})
	if err != nil {
		writeAdminError(w, 400, "key_update_failed", a.manager.Redact(err.Error()))
		return
	}
	a.handleListAPIKeys(w, r)
}

func (a *Server) handleDeleteAPIKey(w http.ResponseWriter, r *http.Request) {
	_, err := a.manager.Update(func(cfg *config.Config) error {
		return changeAPIKey(cfg, r.PathValue("id"), nil, nil, keyPolicyPatch{}, true)
	})
	if err != nil {
		writeAdminError(w, 400, "key_delete_failed", a.manager.Redact(err.Error()))
		return
	}
	a.handleListAPIKeys(w, r)
}
