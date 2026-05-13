package transcribe

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync/atomic"
	"time"

	openaisdk "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"

	"vocis/internal/config"
)

// defaultRequestTimeoutSeconds is the per-HTTP-request timeout
// applied to the transcription SDK client. Used to live as
// `transcription.request_timeout_seconds`; pinned here once it was
// clear nobody tuned it. 45 s covers a cold local model load + a
// max-length chunk transcription comfortably; set to 0 in code to
// disable, but that's a rebuild-and-redeploy change.
const defaultRequestTimeoutSeconds = 45

// ErrInputAudioBufferCommitEmpty was previously emitted by the realtime
// WebSocket backend when a finalize-time commit found the audio buffer
// already drained. The chat-audio backend does not produce this error,
// but the sentinel stays so callers that wrap it in errors.Is checks
// keep compiling. Reachable only from legacy code paths that no longer
// exist; safe to remove once any external dependency on it is gone.
var ErrInputAudioBufferCommitEmpty = errors.New("input audio buffer commit empty")

// ---------------------------------------------------------------------------
// Client
// ---------------------------------------------------------------------------

type Client struct {
	cfg          config.TranscriptionConfig
	client       openaisdk.Client
	chatStreamer chatCompletionStreamer
	httpClient   *http.Client
	writeTimeout time.Duration
}

func New(cfg config.TranscriptionConfig) *Client {
	timeout := time.Duration(defaultRequestTimeoutSeconds) * time.Second

	baseURL := strings.TrimRight(cfg.BaseURL, "/")

	opts := []option.RequestOption{
		option.WithBaseURL(baseURL),
		option.WithRequestTimeout(timeout),
	}

	sdkClient := openaisdk.NewClient(opts...)

	// Plain http.Client for the chat-audio backend, sharing the same
	// per-request cap as the SDK above.
	httpClient := &http.Client{Timeout: timeout}

	return &Client{
		cfg:          cfg,
		client:       sdkClient,
		chatStreamer: &sdkChatStreamer{completions: &sdkClient.Chat.Completions},
		httpClient:   httpClient,
		writeTimeout: minDuration(timeout, 5*time.Second),
	}
}

// ---------------------------------------------------------------------------
// Dictation surface
// ---------------------------------------------------------------------------

type DictationEventType string

const (
	DictationEventPartial DictationEventType = "partial"
	DictationEventSegment DictationEventType = "segment"
	// DictationEventBeginReplace announces that the next chunk is a
	// continuation rebatch and the prior emitted segment is about to be
	// replaced. PrevLen is the rune count of the prior segment's text
	// as it was previously emitted. Fired BEFORE the rebatched POST so
	// the overlay can retract the prior segment (and animate the
	// deletion) before any streaming partial of the unified transcript
	// renders on top of the old text. A matching ReplaceSegment then
	// supplies the new text once the model returns. If the rebatched
	// POST fails, a CancelReplace restores the prior segment.
	DictationEventBeginReplace DictationEventType = "begin_replace"
	// DictationEventCancelReplace rolls back a prior BeginReplace
	// after the rebatched chunk failed (HTTP error, hallucination
	// filter, etc). Text carries the prior segment's formatted text so
	// the caller can re-add it to its displayed buffer; PrevLen is the
	// same rune count BeginReplace was sent with.
	DictationEventCancelReplace DictationEventType = "cancel_replace"
	// DictationEventReplaceSegment retracts the immediately-previous
	// segment and substitutes Text in its place. PrevLen is the rune
	// count of the prior segment's text as it was previously emitted
	// (used by the injector to know how many backspaces to send into
	// the target window). Emitted by the chat-audio backend when
	// continuation_rebatch is on and a multi-clip re-batch produced
	// a unified transcript covering the previously-emitted segment +
	// the current chunk.
	DictationEventReplaceSegment DictationEventType = "replace_segment"
)

type DictationEvent struct {
	Type    DictationEventType
	Text    string
	PrevLen int
}

type FinalizeResult struct {
	Text string
	// RetractFromLivePrevLen, when > 0, is the rune count the caller
	// must strip from its already-emitted live text (the running
	// concatenation of DictationEventSegment/ReplaceSegment events)
	// before concatenating Text. Used by the chat-audio
	// continuation_rebatch path when a rebatch fires AFTER liveSegments
	// flipped to false: the prior segment lives in the caller's
	// liveText (it was emitted before Finalize), not in the
	// trailing-collector's buffer, so the retraction has to be
	// forwarded up here for the caller to apply.
	RetractFromLivePrevLen int
}

// Dictation is the surface every backend's session must expose to the
// app and recall packages. The chat-audio path returns *chatAudioSession;
// callers consume Events() and Finalize() through this interface.
type Dictation interface {
	Events() <-chan DictationEvent
	Finalize(ctx context.Context) (FinalizeResult, error)
}

// ConnectCallbacks receives notifications about connection status.
type ConnectCallbacks struct {
	OnConnecting func(attempt, max int)
	OnConnected  func()
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
	// ExtraSystemPrompt, when non-empty, is appended to the chat-audio
	// session's system message with a blank-line separator. Used by
	// the serve path to append prompt_hint and (in combine-postprocess
	// mode) postprocess.prompt to the user's transcription.prompt lead.
	ExtraSystemPrompt string
}

// finalResult is the trailing-transcript message the chat-audio worker
// publishes to its finals channel after Finalize has flipped the session
// out of live mode. text/err carry the trailing chunk's output;
// replacePrevLen carries the continuation-rebatch retraction count when
// a rebatch lands after the live boundary.
type finalResult struct {
	text string
	err  error
	// replacePrevLen, when > 0, marks this result as a retraction-then-
	// replace of the previously-queued finalResult's text. Used by the
	// chat-audio continuation_rebatch path when a rebatch happens after
	// liveSegments has flipped to false (post-Finalize): we can't fire
	// a DictationEventReplaceSegment because the worker has switched to
	// the finals queue, so the trailing-collector drops the last
	// queued text and substitutes this one.
	replacePrevLen int
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

func minDuration(a, b time.Duration) time.Duration {
	if a <= 0 {
		return b
	}
	if a < b {
		return a
	}
	return b
}
