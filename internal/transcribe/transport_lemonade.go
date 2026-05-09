package transcribe

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/gorilla/websocket"

	"vocis/internal/config"
)

const (
	lemonadeSampleRate        = 16000
	lemonadeDefaultRealtimeURL = "ws://localhost:9000"
)

type lemonadeTransport struct {
	cfg       config.TranscriptionConfig
	streaming config.StreamingConfig
	dialer    websocket.Dialer
	rawURL    string
}

func newLemonadeTransport(
	cfg config.TranscriptionConfig,
	streaming config.StreamingConfig,
	timeout time.Duration,
) *lemonadeTransport {
	raw := strings.TrimRight(cfg.RealtimeURL, "/")
	if raw == "" {
		raw = lemonadeDefaultRealtimeURL
	}
	return &lemonadeTransport{
		cfg:       cfg,
		streaming: streaming,
		dialer:    websocket.Dialer{HandshakeTimeout: minDuration(timeout, 5*time.Second)},
		rawURL:    raw,
	}
}

func (t *lemonadeTransport) SampleRate() int { return lemonadeSampleRate }

func (t *lemonadeTransport) Dial(ctx context.Context) (*websocket.Conn, error) {
	wsURL, err := t.buildURL()
	if err != nil {
		return nil, err
	}
	conn, resp, err := t.dialer.DialContext(ctx, wsURL, nil)
	if err != nil {
		return nil, formatDialError(err, resp)
	}
	return conn, nil
}

// buildURL converts the configured base into a Lemonade realtime WS URL of the
// form ws://host:port/realtime?model=<model>. Accepts http(s)/ws(s) schemes
// and adds /realtime + ?model= when missing.
func (t *lemonadeTransport) buildURL() (string, error) {
	u, err := url.Parse(t.rawURL)
	if err != nil {
		return "", fmt.Errorf("parse lemonade realtime_url %q: %w", t.rawURL, err)
	}
	switch u.Scheme {
	case "http":
		u.Scheme = "ws"
	case "https":
		u.Scheme = "wss"
	case "ws", "wss":
	default:
		return "", fmt.Errorf("lemonade realtime_url must use ws, wss, http, or https: %q", t.rawURL)
	}
	if u.Path == "" || u.Path == "/" {
		u.Path = "/realtime"
	}
	q := u.Query()
	if q.Get("model") == "" && strings.TrimSpace(t.cfg.Model) != "" {
		q.Set("model", t.cfg.Model)
	}
	u.RawQuery = q.Encode()
	return u.String(), nil
}

func (t *lemonadeTransport) SessionUpdate() map[string]any {
	session := map[string]any{
		"model": t.cfg.Model,
	}
	if td, include := t.turnDetectionPayload(); include {
		// Manual-commit mode sends turn_detection=null. Lemonade PR #1607
		// honors null by disabling server-side VAD and the interim
		// transcription work that otherwise ran in parallel with commit.
		session["turn_detection"] = td
	}
	if language := strings.TrimSpace(t.cfg.Language); language != "" {
		session["language"] = language
	}
	if prompt := strings.TrimSpace(t.cfg.PromptHint); prompt != "" {
		// Pass the prompt hint at the session level. Lemonade's schema
		// is flat (unlike OpenAI's nested audio.input.transcription.prompt),
		// so sibling to model/language is the natural placement. For
		// Whisper-family models this biases vocabulary (short keyword
		// lists work best). For LLM-based transcribers like Gemma-FLM,
		// full instructions can shape punctuation, capitalization, and
		// cleanup behavior.
		session["prompt"] = prompt
	}
	return map[string]any{
		"type":    "session.update",
		"session": session,
	}
}

// turnDetectionPayload builds Lemonade's VAD config. Returns (payload,
// include) so callers can distinguish three cases:
//   - (nil, true): send turn_detection=null to disable server VAD
//     (manual-commit mode).
//   - (map, true): send an explicit turn_detection with user-set fields.
//     Fields left at zero are omitted so Lemonade fills them with its
//     own defaults (threshold=0.01, silence_duration_ms=800,
//     prefix_padding_ms=250 as of Lemonade 10.2).
//   - (nil, false): omit the turn_detection key entirely so ALL Lemonade
//     defaults apply (no partial overrides).
//
// Lemonade's `threshold` is RMS energy (0-1), NOT the 0-1 probability
// OpenAI uses — they share a field name but not semantics. vocis warns
// separately if the configured value looks like an OpenAI-shaped
// threshold (> 0.1) that would reject all speech on Lemonade's scale.
func (t *lemonadeTransport) turnDetectionPayload() (any, bool) {
	if t.streaming.ManualCommit {
		return nil, true
	}
	payload := map[string]any{}
	if t.streaming.Threshold > 0 {
		payload["threshold"] = t.streaming.Threshold
	}
	if t.streaming.SilenceDurationMS > 0 {
		payload["silence_duration_ms"] = t.streaming.SilenceDurationMS
	}
	if t.streaming.PrefixPaddingMS > 0 {
		payload["prefix_padding_ms"] = t.streaming.PrefixPaddingMS
	}
	if len(payload) == 0 {
		return nil, false
	}
	return payload, true
}
