package models

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"opencode2api/internal/config"
)

// KiloModel is one free model as the Kilo gateway advertises it. Pricing is
// part of the discovery contract: a :free ID with non-zero pricing is not a
// free model and must not be routed to the keyless lane.
type KiloModel struct {
	ID          string
	Name        string
	Context     int
	MaxOutput   int
	DataSharing bool
}

type kiloModelsResponse struct {
	Data []struct {
		ID           string `json:"id"`
		Name         string `json:"name"`
		Architecture struct {
			Input  []string `json:"input_modalities"`
			Output []string `json:"output_modalities"`
		} `json:"architecture"`
		TopProvider struct {
			Context   int `json:"context_length"`
			MaxOutput int `json:"max_completion_tokens"`
		} `json:"top_provider"`
		Pricing struct {
			Prompt     any `json:"prompt"`
			Completion any `json:"completion"`
		} `json:"pricing"`
	} `json:"data"`
}

func zeroPricing(value any) bool {
	switch v := value.(type) {
	case nil:
		return true
	case string:
		return v == "" || v == "0" || v == "0.0"
	case float64:
		return v == 0
	case int:
		return v == 0
	}
	return false
}

// FetchKiloModels lists Kilo's free models. The endpoint is public keyless;
// an optional configured key is sent only so a private deployment can narrow
// the catalog. No OpenCode headers or User-Agent are used here: Kilo is a
// separate provider, not an OpenCode CLI impersonation.
func FetchKiloModels(ctx context.Context, client *http.Client, cfg config.KiloConfig) ([]KiloModel, int, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, cfg.CatalogURL, nil)
	if err != nil {
		return nil, 0, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "o2a-v2")
	if cfg.Key != "" {
		req.Header.Set("Authorization", "Bearer "+cfg.Key)
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return nil, resp.StatusCode, fmt.Errorf("kilo models endpoint returned HTTP %d", resp.StatusCode)
	}
	var payload kiloModelsResponse
	dec := json.NewDecoder(io.LimitReader(resp.Body, 12<<20))
	if err := dec.Decode(&payload); err != nil {
		return nil, resp.StatusCode, err
	}
	shared := make(map[string]bool, len(cfg.DataSharingIDs))
	for _, id := range cfg.DataSharingIDs {
		shared[id] = true
	}
	out := make([]KiloModel, 0, len(payload.Data))
	for _, item := range payload.Data {
		id := strings.TrimSpace(item.ID)
		if id == "" || (!strings.HasSuffix(id, ":free") && !strings.HasSuffix(id, "/free")) {
			continue
		}
		if !zeroPricing(item.Pricing.Prompt) || !zeroPricing(item.Pricing.Completion) {
			continue
		}
		out = append(out, KiloModel{
			ID: id, Name: strings.TrimSpace(item.Name),
			Context: item.TopProvider.Context, MaxOutput: item.TopProvider.MaxOutput,
			DataSharing: shared[id] || id == "kilo-auto/free",
		})
	}
	if len(out) == 0 {
		return nil, resp.StatusCode, errors.New("kilo models endpoint returned no free models")
	}
	return out, resp.StatusCode, nil
}
