package transcribe

import (
	"context"
	"fmt"
	"strings"
	"time"

	"vocis/internal/config"
	"vocis/internal/sessionlog"
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
	Name       string                       `json:"model_name"`
	Type       string                       `json:"type"` // audio | llm | tts | embedding | ...
	Device     string                       `json:"device"`
	Recipe     string                       `json:"recipe"`
	Checkpoint string                       `json:"checkpoint"`
	// RecipeOptions surfaces runtime knobs the recipe was loaded with.
	// CtxSize is the only field vocis currently reads — it's the actual
	// prompt-token budget the model will accept, which can be MUCH
	// smaller than the model's theoretical max_context_window (an FLM
	// recipe on an NPU typically pins it to 4096 even when the model
	// itself supports 128K).
	RecipeOptions LemonadeRecipeOptions `json:"recipe_options"`
}

type LemonadeRecipeOptions struct {
	CtxSize int `json:"ctx_size"`
}

// FetchLemonadeHealth returns the parsed /api/v1/health payload for the
// Lemonade instance at baseURL (e.g. "http://localhost:13305/api/v1").
// Uses a short timeout — the health endpoint is cheap and a slow
// response indicates Lemonade is busy loading something else, which
// we'd rather surface than hide behind a retry.
func FetchLemonadeHealth(ctx context.Context, baseURL string) (LemonadeHealth, error) {
	url, err := lemonadeURL(baseURL, "/health")
	if err != nil {
		return LemonadeHealth{}, err
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	var out LemonadeHealth
	if err := getJSON(ctx, url, &out); err != nil {
		return LemonadeHealth{}, err
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
	url, err := lemonadeURL(baseURL, "/models")
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	var payload struct {
		Data []LemonadeModelEntry `json:"data"`
	}
	if err := getJSON(ctx, url, &payload); err != nil {
		return nil, err
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

// EnsureModelCtxSizeFromConfig reads ChatAudio.CtxSize from cfg and
// pins the model's ctx_size via Lemonade /api/v1/load when set. The
// no-config-set case is a no-op so callers can invoke this
// unconditionally during startup. Applies a 2-minute deadline (NPU
// first-time loads can stall for 30+ s) and wraps errors with the
// requested size for log-readability.
//
// This is the helper the recall daemon, `serve`, and the standalone
// `vocis transcribe` CLI all call. Centralizing it ensures the same
// timeout, the same skip-when-empty-model behavior, and the same
// error-wrapping across entrypoints.
func EnsureModelCtxSizeFromConfig(ctx context.Context, cfg config.Config) error {
	cx := cfg.Transcription.ChatAudio.CtxSize
	if cx <= 0 {
		return nil
	}
	model := strings.TrimSpace(cfg.Transcription.Model)
	if model == "" {
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
	defer cancel()
	if err := EnsureModelCtxSize(ctx, cfg.Transcription.BaseURL, model, cx); err != nil {
		return fmt.Errorf("ensure ctx_size=%d: %w", cx, err)
	}
	return nil
}

// EnsureModelCtxSize checks Lemonade's /api/v1/health for the named
// model's current ctx_size and reloads it via POST /api/v1/load when
// the value doesn't match desired. Idempotent — calls /load only when
// a reload is actually needed, so daemon-restart loops don't churn
// the model.
//
// desired<=0 is a no-op (the user hasn't asked us to pin ctx_size).
// /health failures degrade to a warn-log and return nil — we'd rather
// proceed with whatever the model already has than refuse to start.
// /load failures DO return the error: the caller asked for a specific
// size, can't honor it, surfacing is the right call.
//
// Note the reload is slow (~5-30s on NPU) the first time it's needed.
// Subsequent daemon starts see a matching ctx_size and skip the call.
func EnsureModelCtxSize(ctx context.Context, baseURL, modelName string, desired int) error {
	if desired <= 0 {
		return nil
	}
	if strings.TrimSpace(modelName) == "" {
		return fmt.Errorf("EnsureModelCtxSize: modelName is empty")
	}

	health, err := FetchLemonadeHealth(ctx, baseURL)
	if err != nil {
		sessionlog.Warnf("chat-audio: ctx_size pin: /health probe failed (%v) — proceeding without enforcing ctx_size=%d", err, desired)
		return nil
	}
	for _, m := range health.Loaded {
		if m.Name == modelName && m.RecipeOptions.CtxSize == desired {
			sessionlog.Infof("chat-audio: ctx_size pin: model %q already loaded with ctx_size=%d, no reload needed",
				modelName, desired)
			return nil
		}
	}

	sessionlog.Infof("chat-audio: ctx_size pin: reloading model %q with ctx_size=%d via /api/v1/load", modelName, desired)
	return postLemonadeLoad(ctx, baseURL, modelName, desired)
}

// postLemonadeLoad triggers a model reload via Lemonade's /v1/load
// endpoint. Documented payload accepts model_name (required) plus
// recipe-specific options; ctx_size applies to llamacpp / FLM
// recipes.
func postLemonadeLoad(ctx context.Context, baseURL, modelName string, ctxSize int) error {
	url, err := lemonadeURL(baseURL, "/load")
	if err != nil {
		return err
	}
	// Generous timeout — loading a model on NPU can take 30+ seconds
	// the first time. The caller is paused on this anyway and won't
	// proceed until the right ctx_size is in place.
	ctx, cancel := context.WithTimeout(ctx, 90*time.Second)
	defer cancel()

	resp, err := postJSON(ctx, nil, url, map[string]any{
		"model_name": modelName,
		"ctx_size":   ctxSize,
	})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	excerpt := httpBodyExcerpt(resp)
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("POST %s: status %d: %s", url, resp.StatusCode, excerpt)
	}
	sessionlog.Infof("chat-audio: ctx_size pin: /load OK — %s", excerpt)
	return nil
}
