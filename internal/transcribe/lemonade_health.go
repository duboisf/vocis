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
