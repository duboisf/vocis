package transcribe

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"
)

// LemonadeHealth captures the subset of /api/v1/health vocis cares
// about: what models are currently resident in memory, and how many
// slots each type has. "Loaded" is the distinction that matters at
// runtime — "downloaded" (the /models endpoint) only tells you the
// model is on disk, not that it will respond to requests without a
// 5–10 s load stall.
type LemonadeHealth struct {
	Version   string                  `json:"version"`
	Status    string                  `json:"status"`
	WSPort    int                     `json:"websocket_port"`
	MaxModels map[string]int          `json:"max_models"`
	Loaded    []LemonadeLoadedModel   `json:"all_models_loaded"`
}

type LemonadeLoadedModel struct {
	Name       string `json:"model_name"`
	Type       string `json:"type"` // audio | llm | tts | embedding | ...
	Device     string `json:"device"`
	Recipe     string `json:"recipe"`
	Checkpoint string `json:"checkpoint"`
}

// FetchLemonadeHealth returns the parsed /api/v1/health payload for the
// Lemonade instance at baseURL (e.g. "http://localhost:13305/api/v1").
// Uses a short timeout — the health endpoint is cheap and a slow
// response indicates Lemonade is busy loading something else, which
// we'd rather surface than hide behind a retry.
func FetchLemonadeHealth(ctx context.Context, baseURL string) (LemonadeHealth, error) {
	baseURL = strings.TrimRight(baseURL, "/")
	if baseURL == "" {
		return LemonadeHealth{}, fmt.Errorf("lemonade base_url is empty")
	}
	url := baseURL + "/health"

	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return LemonadeHealth{}, fmt.Errorf("build request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return LemonadeHealth{}, fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return LemonadeHealth{}, fmt.Errorf("GET %s: status %d", url, resp.StatusCode)
	}
	var out LemonadeHealth
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return LemonadeHealth{}, fmt.Errorf("decode health: %w", err)
	}
	return out, nil
}

// IsLoaded reports whether a model with the given name is currently
// resident. Matching is exact — Lemonade's `model_name` and the name
// we configure must line up (e.g. "whisper-v3-turbo-FLM"). Case-
// sensitive to match the server's behavior.
func (h LemonadeHealth) IsLoaded(modelName string) bool {
	for _, m := range h.Loaded {
		if m.Name == modelName {
			return true
		}
	}
	return false
}

// LoadedNames returns the names of currently-resident models, useful
// for error messages that show what the user could pick from.
func (h LemonadeHealth) LoadedNames() []string {
	names := make([]string, 0, len(h.Loaded))
	for _, m := range h.Loaded {
		names = append(names, m.Name)
	}
	return names
}

// LemonadeModelEntry captures the labels-bearing slice of /api/v1/models
// that vocis needs to validate "is this model actually a transcription
// model?" — Lemonade 10.3 silently emits empty deltas/transcripts when
// the realtime WS is given a non-audio model, so we fail fast at
// preflight instead.
type LemonadeModelEntry struct {
	ID     string   `json:"id"`
	Labels []string `json:"labels"`
}

// FetchLemonadeModel returns the catalog entry for a specific model id
// from /api/v1/models. Returns (nil, nil) when the model is not in the
// catalog (caller decides whether that's fatal — typically it isn't,
// because user-pulled models lag the built-in catalog).
func FetchLemonadeModel(ctx context.Context, baseURL, modelID string) (*LemonadeModelEntry, error) {
	baseURL = strings.TrimRight(baseURL, "/")
	if baseURL == "" {
		return nil, fmt.Errorf("lemonade base_url is empty")
	}
	url := baseURL + "/models"

	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()

	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, fmt.Errorf("build request: %w", err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, fmt.Errorf("GET %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: status %d", url, resp.StatusCode)
	}
	var payload struct {
		Data []LemonadeModelEntry `json:"data"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&payload); err != nil {
		return nil, fmt.Errorf("decode models: %w", err)
	}
	for i := range payload.Data {
		if payload.Data[i].ID == modelID {
			return &payload.Data[i], nil
		}
	}
	return nil, nil
}

// HasLabel reports whether the catalog entry carries the given label.
// Comparison is case-insensitive to match Lemonade's relaxed label
// conventions ("transcription", "Transcription", etc).
func (m LemonadeModelEntry) HasLabel(label string) bool {
	for _, l := range m.Labels {
		if strings.EqualFold(l, label) {
			return true
		}
	}
	return false
}
