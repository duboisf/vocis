package transcribe

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"vocis/internal/config"
)

// defaultRequestTimeoutSeconds is the per-HTTP-request timeout
// applied to the transcription SDK client. Used to live as
// `transcription.request_timeout_seconds`; pinned here once it was
// clear nobody tuned it. 45 s covers a cold local model load + a
// max-length chunk transcription comfortably; set to 0 in code to
// disable, but that's a rebuild-and-redeploy change.
const defaultRequestTimeoutSeconds = 45

// ---------------------------------------------------------------------------
// Client
// ---------------------------------------------------------------------------

type Client struct {
	cfg        config.TranscriptionConfig
	httpClient *http.Client
}

func New(cfg config.TranscriptionConfig) *Client {
	return &Client{
		cfg:        cfg,
		httpClient: &http.Client{Timeout: time.Duration(defaultRequestTimeoutSeconds) * time.Second},
	}
}

// ---------------------------------------------------------------------------
// Dictation surface
// ---------------------------------------------------------------------------

type DictationEventType string

const (
	// DictationEventPartial carries the cumulative SSE text of the
	// in-flight response, for live display only.
	DictationEventPartial DictationEventType = "partial"
)

type DictationEvent struct {
	Type DictationEventType
	Text string
}

// FinalizeResult carries the whole transcript of the session and, when
// audio capture is on, the path of the WAV the model heard.
type FinalizeResult struct {
	Text      string
	AudioPath string
}

// Dictation is the surface the app and recall packages consume.
// Events() streams partials for live display and closes when Finalize
// returns; Finalize() sends the whole dictation and returns the text.
type Dictation interface {
	Events() <-chan DictationEvent
	Finalize(ctx context.Context) (FinalizeResult, error)
}

// DictationOpts groups every parameter StartDictation needs.
type DictationOpts struct {
	SampleRate int
	Channels   int
	Samples    <-chan []int16
	// OnConnected, when non-nil, fires once on the first audio chunk
	// the session sees. There is no handshake to await; this is the
	// sync point that lets the overlay flip from "Connecting" to
	// "Ready" after the caller finished its own setup.
	OnConnected func()
}

// StartDictation begins a chat-audio dictation session. The backend is
// fixed (chat-audio is the only one supported); the function name
// preserves the historical surface.
func (c *Client) StartDictation(ctx context.Context, opts DictationOpts) (Dictation, error) {
	if opts.SampleRate <= 0 {
		return nil, errors.New("recording.sample_rate must be greater than zero")
	}
	if opts.Channels <= 0 {
		return nil, errors.New("recording.channels must be greater than zero")
	}
	return startChatAudioSession(ctx, c.cfg, c.httpClient, opts)
}

// ---------------------------------------------------------------------------
// Hallucination filter
// ---------------------------------------------------------------------------

// buildHallucinationSet normalizes the configured filter list into a set
// keyed by lowercased+trimmed text for O(1) case-insensitive lookup.
// Empty entries are ignored.
func buildHallucinationSet(filters []string) map[string]bool {
	if len(filters) == 0 {
		return nil
	}
	set := make(map[string]bool, len(filters))
	for _, f := range filters {
		key := strings.ToLower(strings.TrimSpace(f))
		if key == "" {
			continue
		}
		set[key] = true
	}
	return set
}

// ---------------------------------------------------------------------------
// Misc helpers
// ---------------------------------------------------------------------------

// truncate returns at most max bytes of s with a trailing "…" if it was
// truncated. Log lines should not carry full transcripts (could be
// arbitrarily long); 80 chars is enough to recognize the text at a glance.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}
