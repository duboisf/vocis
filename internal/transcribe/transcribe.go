package transcribe

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
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
	DictationEventPartial DictationEventType = "partial"
	DictationEventSegment DictationEventType = "segment"
)

type DictationEvent struct {
	Type DictationEventType
	Text string
}

type FinalizeResult struct {
	Text string
}

// Dictation is the surface every backend's session must expose to the
// app and recall packages. The chat-audio path returns *chatAudioSession;
// callers consume Events() and Finalize() through this interface.
type Dictation interface {
	Events() <-chan DictationEvent
	Finalize(ctx context.Context) (FinalizeResult, error)
}

// ConnectCallbacks receives notifications about connection status.
// OnConnected fires once, on the first audio chunk the session sees.
type ConnectCallbacks struct {
	OnConnected func()
}

// DictationOpts groups every parameter StartDictation needs. Pass-by-struct
// keeps call sites readable when the parameter list grows beyond ~3 args.
type DictationOpts struct {
	SampleRate int
	Channels   int
	Samples    <-chan []int16
	Callbacks  ConnectCallbacks
	// ExpectedAudioMS is the total audio duration (in ms) the caller
	// intends to feed through the session, when known upfront. Currently
	// informational — reserved for future per-call timeout scaling.
	// 0 = unknown.
	ExpectedAudioMS int
}

// finalResult is the trailing-transcript message the chat-audio worker
// publishes to its finals channel after Finalize has flipped the session
// out of live mode.
type finalResult struct {
	text string
	err  error
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
// Segment formatting / text utilities
// ---------------------------------------------------------------------------

// formatSegmentText increments the segment counter and adds a leading
// space when the new segment needs separation from the running text.
// Atomic.Add returns the new count, so first-segment is `n == 1`.
func formatSegmentText(count *atomic.Int32, text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	if count.Add(1) == 1 {
		return text
	}
	if strings.HasPrefix(text, " ") || strings.HasPrefix(text, "\n") {
		return text
	}
	if startsWithPunctuation(text) {
		return text
	}
	return " " + text
}

func appendSegmentText(current, next string) string {
	switch {
	case strings.TrimSpace(next) == "":
		return current
	case current == "":
		return next
	case strings.HasPrefix(next, " ") || strings.HasPrefix(next, "\n"):
		return current + next
	case startsWithPunctuation(next):
		return current + next
	default:
		return current + " " + next
	}
}

func startsWithPunctuation(text string) bool {
	if text == "" {
		return false
	}
	switch []rune(text)[0] {
	case '.', ',', ';', ':', '!', '?', ')', ']', '}':
		return true
	default:
		return false
	}
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
