package transcribe

import (
	"context"
	"fmt"
	"strings"
	"time"

	"vocis/internal/config"
	"vocis/internal/sessionlog"
)

// LoadLemonadeModel POSTs to Lemonade's /load endpoint to force a
// model into its resident slot. Type-agnostic — works for both
// audio (whisper-FLM) and llm (gemma-FLM) models. Returns when
// Lemonade reports the load succeeded; propagates HTTP errors with
// body excerpts.
//
// Default 60 s deadline applied internally if ctx has none — a cold
// model load on NPU can run several seconds, plus possible queueing
// behind a current-loaded model swap.
func LoadLemonadeModel(ctx context.Context, baseURL, model string) error {
	if strings.TrimSpace(model) == "" {
		return fmt.Errorf("model name is empty")
	}
	url, err := lemonadeURL(baseURL, "/load")
	if err != nil {
		return err
	}
	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 60*time.Second)
		defer cancel()
	}
	resp, err := postJSON(ctx, nil, url, map[string]string{"model_name": model})
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode/100 != 2 {
		return fmt.Errorf("POST %s: status %d: %s", url, resp.StatusCode, httpBodyExcerpt(resp))
	}
	sessionlog.Debugf("lemonade /load %s ok", model)
	return nil
}

// loadLemonadeModelLogging is the fire-and-forget wrapper for
// callers that want errors logged and swallowed (the async warm
// path from EnsureLemonadeModelsLoaded).
func loadLemonadeModelLogging(ctx context.Context, cfg config.TranscriptionConfig) {
	if strings.TrimSpace(cfg.Model) == "" {
		return
	}
	if err := LoadLemonadeModel(ctx, cfg.BaseURL, cfg.Model); err != nil {
		sessionlog.Warnf("lemonade load %s: %v", cfg.Model, err)
	}
}

// EnsureTranscribeModelLoaded is the synchronous preflight called at
// transcribe time. For Lemonade, it fetches /api/v1/health and — if
// the configured transcription model isn't resident — POSTs to
// /api/v1/load inline. Returns when the model is loaded or when an
// error occurs; propagates ctx cancellation and the load request's
// error.
//
// onLoading, if non-nil, is invoked once with the model name just
// before the /load POST is issued. Callers use it to surface a
// "Loading <model>..." overlay state so the user knows why the
// session hasn't started yet.
//
// No-op (returns nil) for empty base_url or empty model.
func EnsureTranscribeModelLoaded(ctx context.Context, cfg config.TranscriptionConfig, onLoading func(model string)) error {
	model := strings.TrimSpace(cfg.Model)
	if model == "" {
		return nil
	}
	baseURL := strings.TrimRight(cfg.BaseURL, "/")
	if baseURL == "" {
		return fmt.Errorf("transcription.base_url is empty")
	}

	// Label guard only applies to backends where the configured model
	// must carry the `transcription` label — realtime WS silently
	// returns empty deltas otherwise. Chat-audio drives
	// /chat/completions, which works on llm-typed models like
	// gemma-FLM that don't carry the `transcription` label.
	if cfg.NeedsTranscriptionLabelGuard() {
		if entry, err := FetchLemonadeModel(ctx, baseURL, model); err != nil {
			// Catalog fetch failed — don't block on it. The user's model
			// might be a user-pulled custom that doesn't show up in the
			// catalog, or the /models endpoint hiccupped. Log and move on.
			sessionlog.Warnf("lemonade preflight: could not verify labels for %s (%v) — proceeding", model, err)
		} else if entry == nil {
			sessionlog.Debugf("lemonade preflight: %s not in catalog (custom user model?) — skipping label check", model)
		} else if !entry.HasLabel("transcription") {
			return fmt.Errorf(
				"transcription.model %q is not a transcription model on this Lemonade instance "+
					"(labels: %v) — pick a model carrying the `transcription` label "+
					"(e.g. whisper-v3-turbo-FLM) via `vocis config models`",
				model, entry.Labels,
			)
		}
	}

	health, err := FetchLemonadeHealth(ctx, baseURL)
	if err != nil {
		return fmt.Errorf("lemonade health check: %w", err)
	}
	if health.IsLoaded(model) {
		sessionlog.Debugf("lemonade preflight: %s already loaded", model)
		return nil
	}
	sessionlog.Infof("lemonade preflight: %s not loaded (resident: %v) — loading now", model, health.LoadedNames())
	if onLoading != nil {
		onLoading(model)
	}
	if err := LoadLemonadeModel(ctx, baseURL, model); err != nil {
		return fmt.Errorf("load %s: %w", model, err)
	}
	return nil
}

// EnsureLemonadeModelsLoaded checks that the configured transcribe and
// postprocess models are resident on the Lemonade instance. If either
// is missing it fires a /load request (async) without blocking the
// caller. Logs a concise warning per missing model.
//
// Returns an error when the Lemonade server is unreachable so callers
// can fail startup loudly instead of letting the user discover the
// problem on their first dictation, when transcription silently fails.
// The model-load requests themselves remain fire-and-forget — they
// take 5-10 s and would otherwise stall startup for no good reason.
func EnsureLemonadeModelsLoaded(ctx context.Context, cfg config.Config, transcribeClient *Client) error {
	baseURL := cfg.Transcription.BaseURL
	health, err := FetchLemonadeHealth(ctx, baseURL)
	if err != nil {
		return fmt.Errorf("lemonade not reachable at %s — start the Lemonade server (`lemonade-server serve`) and retry: %w", baseURL, err)
	}

	txModel := strings.TrimSpace(cfg.Transcription.Model)
	// Pin Lemonade's ctx_size before the background warm. If the
	// model is loaded with the wrong size, EnsureModelCtxSizeFromConfig
	// reloads it synchronously so the warm path doesn't race with
	// another reload. No-op when ctx_size is 0.
	if err := EnsureModelCtxSizeFromConfig(ctx, cfg); err != nil {
		return err
	}
	if cfg.Transcription.ChatAudio.CtxSize > 0 && txModel != "" {
		// Re-fetch health so the IsLoaded check below sees the
		// post-reload state.
		if fresh, err := FetchLemonadeHealth(ctx, baseURL); err == nil {
			health = fresh
		}
	}
	if txModel != "" && !health.IsLoaded(txModel) {
		sessionlog.Infof("lemonade: %s not loaded (resident: %v) — loading in background", txModel, health.LoadedNames())
		go loadLemonadeModelLogging(context.Background(), cfg.Transcription)
	} else if txModel != "" {
		sessionlog.Debugf("lemonade: transcription model %s already loaded", txModel)
	}

	// Skip warming the postprocess model on backends that combine
	// transcription + cleanup in a single call (chat-audio): app.go
	// never makes a separate postprocess request there, so loading a
	// separate llm model would just evict gemma from the single llm
	// slot Lemonade allows.
	if cfg.PostProcess.Enabled && cfg.Transcription.SkipPostProcessModelWarm() {
		ppModel := strings.TrimSpace(cfg.PostProcess.Model)
		if ppModel != "" && ppModel != txModel {
			sessionlog.Infof("postprocess.model=%s ignored — combine mode reuses transcription.model=%s for cleanup", ppModel, txModel)
		}
	} else if cfg.PostProcess.Enabled && transcribeClient != nil {
		ppModel := strings.TrimSpace(cfg.PostProcess.Model)
		if ppModel != "" && !health.IsLoaded(ppModel) {
			sessionlog.Infof("lemonade: %s not loaded (resident: %v) — warming in background", ppModel, health.LoadedNames())
			go transcribeClient.WarmPostProcess(context.Background(), ppModel)
		} else if ppModel != "" {
			sessionlog.Debugf("lemonade: postprocess model %s already loaded", ppModel)
		}
	}
	return nil
}
