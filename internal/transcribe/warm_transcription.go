package transcribe

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"strings"
	"time"

	"vocis/internal/config"
	"vocis/internal/sessionlog"
)

// WarmTranscription forces the audio model into memory by posting a
// tiny silent WAV to /api/v1/audio/transcriptions. Mirrors
// WarmPostProcess for the audio slot: on Lemonade the WS realtime
// path can sit with no audio model resident (the LLM stole the slot
// attention), and the first real dictation then pays a 5-10 s load
// stall which shows up as "no transcript ever arrives" from the
// user's POV. Firing a REST transcription on startup moves that
// stall off the dictation critical path.
//
// Only meaningful for the Lemonade backend — OpenAI's cloud always
// has the model warm. Returns nil for non-Lemonade backends and for
// empty model/base_url (nothing to warm). When the caller has not
// set a deadline on ctx, a 30 s default bound is applied internally.
func WarmTranscription(ctx context.Context, cfg config.TranscriptionConfig) error {
	if cfg.Backend != config.BackendLemonade {
		return nil
	}
	model := strings.TrimSpace(cfg.Model)
	if model == "" {
		return nil
	}
	baseURL := strings.TrimRight(cfg.BaseURL, "/")
	if baseURL == "" {
		return fmt.Errorf("transcription.base_url is empty")
	}

	if _, hasDeadline := ctx.Deadline(); !hasDeadline {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, 30*time.Second)
		defer cancel()
	}

	wav := silentWAV(100) // 100 ms is enough to trigger load
	body, contentType, err := multipartAudioForm(wav, model)
	if err != nil {
		return fmt.Errorf("build form: %w", err)
	}

	url := baseURL + "/audio/transcriptions"
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, url, body)
	if err != nil {
		return fmt.Errorf("build request: %w", err)
	}
	req.Header.Set("Content-Type", contentType)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return fmt.Errorf("POST %s: %w", url, err)
	}
	defer resp.Body.Close()
	if resp.StatusCode >= 400 {
		// Read up to 256 bytes of body for diagnostic context. Silent
		// audio sometimes yields a 4xx ("no speech detected") which is
		// fine — the model loaded anyway, but keep that classification
		// at the call site rather than hiding it here.
		snippet, _ := io.ReadAll(io.LimitReader(resp.Body, 256))
		return fmt.Errorf("POST %s: status %d: %s", url, resp.StatusCode, strings.TrimSpace(string(snippet)))
	}
	sessionlog.Debugf("transcription warm %s ok", model)
	return nil
}

// warmTranscriptionLogging wraps WarmTranscription for fire-and-forget
// callers that want the old behavior of logging and swallowing errors.
func warmTranscriptionLogging(ctx context.Context, cfg config.TranscriptionConfig) {
	if err := WarmTranscription(ctx, cfg); err != nil {
		sessionlog.Warnf("transcription warm %s: %v", cfg.Model, err)
	}
}

// EnsureTranscribeModelLoaded is the synchronous preflight called at
// transcribe time. For Lemonade, it fetches /api/v1/health and — if
// the configured transcription model isn't resident — fires the warm
// POST inline. Returns when the model is loaded or when an error
// occurs; propagates ctx cancellation and the warm request's error.
//
// onLoading, if non-nil, is invoked once with the model name just
// before the warm POST is issued. Callers use it to surface a
// "Loading <model>..." overlay state so the user knows why the
// session hasn't started yet.
//
// No-op (returns nil) for non-Lemonade backends, empty base_url, or
// empty model — matching WarmTranscription's shape.
func EnsureTranscribeModelLoaded(ctx context.Context, cfg config.TranscriptionConfig, onLoading func(model string)) error {
	if cfg.Backend != config.BackendLemonade {
		return nil
	}
	model := strings.TrimSpace(cfg.Model)
	if model == "" {
		return nil
	}
	baseURL := strings.TrimRight(cfg.BaseURL, "/")
	if baseURL == "" {
		return fmt.Errorf("transcription.base_url is empty")
	}

	// Label guard: catch the Lemonade 10.3 trap where
	// `gemma4-it-e2b-FLM` (and possibly other reclassified models) is
	// configured as transcription.model but Lemonade's audio router
	// rejects audio for it. The realtime WS would still accept the
	// session.update silently and return empty deltas, so the user
	// experiences "vocis just doesn't produce text" with no clear
	// reason. Fail loud at preflight instead.
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
	if err := WarmTranscription(ctx, cfg); err != nil {
		return fmt.Errorf("load %s: %w", model, err)
	}
	return nil
}

// EnsureLemonadeModelsLoaded checks that the configured transcribe and
// postprocess models are resident on the Lemonade instance. If either
// is missing it fires a warm request (async) to force-load without
// blocking the caller. Logs a concise warning per missing model.
// Safe to call from main; no-op on non-Lemonade backends.
func EnsureLemonadeModelsLoaded(ctx context.Context, cfg config.Config, transcribeClient *Client) {
	if cfg.Transcription.Backend != config.BackendLemonade {
		return
	}
	health, err := FetchLemonadeHealth(ctx, cfg.Transcription.BaseURL)
	if err != nil {
		sessionlog.Warnf("lemonade health check failed: %v", err)
		return
	}

	txModel := strings.TrimSpace(cfg.Transcription.Model)
	if txModel != "" && !health.IsLoaded(txModel) {
		sessionlog.Infof("lemonade: %s not loaded (resident: %v) — warming in background", txModel, health.LoadedNames())
		go warmTranscriptionLogging(context.Background(), cfg.Transcription)
	} else if txModel != "" {
		sessionlog.Debugf("lemonade: transcription model %s already loaded", txModel)
	}

	if cfg.PostProcess.Enabled && transcribeClient != nil {
		ppModel := strings.TrimSpace(cfg.PostProcess.Model)
		if ppModel != "" && !health.IsLoaded(ppModel) {
			sessionlog.Infof("lemonade: %s not loaded (resident: %v) — warming in background", ppModel, health.LoadedNames())
			go transcribeClient.WarmPostProcess(context.Background(), ppModel)
		} else if ppModel != "" {
			sessionlog.Debugf("lemonade: postprocess model %s already loaded", ppModel)
		}
	}
}

// silentWAV returns a canonical 16-bit mono 16 kHz WAV buffer filled
// with `durationMs` of silence. Minimal on purpose — just enough to
// be a valid multipart audio payload that triggers the server-side
// model load.
func silentWAV(durationMs int) []byte {
	const (
		sampleRate    = 16000
		bitsPerSample = 16
		channels      = 1
	)
	numSamples := sampleRate * durationMs / 1000
	dataSize := numSamples * channels * bitsPerSample / 8

	var buf bytes.Buffer
	write := func(v any) { binary.Write(&buf, binary.LittleEndian, v) }

	// RIFF chunk descriptor
	buf.WriteString("RIFF")
	write(uint32(36 + dataSize)) // chunk size
	buf.WriteString("WAVE")

	// fmt sub-chunk
	buf.WriteString("fmt ")
	write(uint32(16))                                  // sub-chunk size (PCM)
	write(uint16(1))                                   // audio format = 1 (PCM)
	write(uint16(channels))                            // channels
	write(uint32(sampleRate))                          // sample rate
	write(uint32(sampleRate * channels * bitsPerSample / 8)) // byte rate
	write(uint16(channels * bitsPerSample / 8))        // block align
	write(uint16(bitsPerSample))                       // bits per sample

	// data sub-chunk
	buf.WriteString("data")
	write(uint32(dataSize))
	buf.Write(make([]byte, dataSize)) // silence

	return buf.Bytes()
}

// multipartAudioForm builds the body for POST /audio/transcriptions
// with the given WAV bytes and model name. Returns the body reader,
// the Content-Type header value (which includes the multipart
// boundary), and any build error.
func multipartAudioForm(wav []byte, model string) (io.Reader, string, error) {
	var buf bytes.Buffer
	w := multipart.NewWriter(&buf)

	if err := w.WriteField("model", model); err != nil {
		return nil, "", fmt.Errorf("write model field: %w", err)
	}
	filePart, err := w.CreateFormFile("file", "warm.wav")
	if err != nil {
		return nil, "", fmt.Errorf("create file part: %w", err)
	}
	if _, err := filePart.Write(wav); err != nil {
		return nil, "", fmt.Errorf("write wav bytes: %w", err)
	}
	if err := w.Close(); err != nil {
		return nil, "", fmt.Errorf("close multipart: %w", err)
	}
	return &buf, w.FormDataContentType(), nil
}
