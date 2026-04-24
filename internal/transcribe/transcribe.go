package transcribe

import (
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/gorilla/websocket"
	openaisdk "github.com/openai/openai-go/v3"
	"github.com/openai/openai-go/v3/option"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"vocis/internal/config"
	"vocis/internal/sessionlog"
	"vocis/internal/telemetry"
)

const (
	defaultBaseURL    = "https://api.openai.com/v1"
	connectTimeout    = 2 * time.Second
	maxConnectRetries = 3
)

var ErrInputAudioBufferCommitEmpty = errors.New("input audio buffer commit empty")

// ---------------------------------------------------------------------------
// Client
// ---------------------------------------------------------------------------

type Client struct {
	cfg          config.TranscriptionConfig
	streaming    config.StreamingConfig
	client       openaisdk.Client
	chatStreamer chatCompletionStreamer
	transport    Transport
	writeTimeout time.Duration
}

func New(apiKey string, cfg config.TranscriptionConfig, streaming config.StreamingConfig) *Client {
	timeout := time.Duration(cfg.RequestLimit) * time.Second

	baseURL := strings.TrimRight(cfg.BaseURL, "/")
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	if cfg.Backend != config.BackendLemonade && baseURL != defaultBaseURL {
		sessionlog.Warnf("openai base_url is non-default: %s", baseURL)
	}

	opts := []option.RequestOption{
		option.WithBaseURL(baseURL),
	}
	// RequestLimit=0 disables the HTTP request timeout entirely — useful
	// when pointing at Lemonade, whose first-request postprocess call
	// can take minutes on a cold local model load. Only apply a timeout
	// when the user picked a positive value. The WS write / handshake
	// timeouts further down still get a 5 s floor via minDuration.
	if timeout > 0 {
		opts = append(opts, option.WithRequestTimeout(timeout))
	} else {
		sessionlog.Infof("transcription: request timeout disabled (request_timeout_seconds=0)")
	}
	// Only attach an API key when one is actually configured. Passing
	// WithAPIKey("") makes the SDK send `Authorization: Bearer ` (empty
	// token) which Lemonade returns 500 on. Lemonade is unauthenticated;
	// OpenAI rejects empty keys at the auth layer with a 401 anyway.
	if apiKey != "" {
		opts = append(opts, option.WithAPIKey(apiKey))
	}
	if org := organization(cfg); org != "" {
		opts = append(opts, option.WithOrganization(org))
	}
	if project := project(cfg); project != "" {
		opts = append(opts, option.WithProject(project))
	}

	sdkClient := openaisdk.NewClient(opts...)

	var transport Transport
	switch cfg.Backend {
	case config.BackendLemonade:
		if streaming.Threshold > 0.1 {
			sessionlog.Warnf("streaming.threshold=%.2f is likely too high for Lemonade (RMS energy 0-1, default 0.01) — speech may never trigger transcription; try 0.01-0.05",
				streaming.Threshold)
		}
		transport = newLemonadeTransport(cfg, streaming, timeout)
	default:
		transport = newOpenAITransport(cfg, streaming, sdkClient, baseURL, timeout)
	}

	return &Client{
		cfg:          cfg,
		streaming:    streaming,
		client:       sdkClient,
		chatStreamer: &sdkChatStreamer{completions: &sdkClient.Chat.Completions},
		transport:    transport,
		writeTimeout: minDuration(timeout, 5*time.Second),
	}
}

// ---------------------------------------------------------------------------
// Stream — low-level WebSocket wrapper
// ---------------------------------------------------------------------------

type StreamEventType string

const (
	StreamEventPartial StreamEventType = "partial"
	StreamEventFinal   StreamEventType = "final"
	StreamEventError   StreamEventType = "error"
)

type StreamEvent struct {
	Type StreamEventType
	Text string
	Err  error
}

type Stream struct {
	conn         *websocket.Conn
	encoder      *pcmEncoder
	writeTimeout time.Duration

	// sessionSpan stays open for the full lifetime of the WebSocket so every
	// interesting inbound/outbound message can be recorded as a span event.
	// High-rate messages (input_audio_buffer.append) are intentionally skipped
	// to keep the trace readable.
	sessionSpan  trace.Span
	sessionStart time.Time
	targetRate   int

	writeMu   sync.Mutex
	closeOnce sync.Once
	readyOnce sync.Once

	readyCh  chan error
	events   chan StreamEvent
	readDone chan struct{}

	partialMu sync.Mutex
	partial   string
	activeID  string
	partials  map[string]string

	// mergeDelta decides how a transcription.delta event combines with the
	// running partial — incremental (append) for gpt-4o-transcribe et al.,
	// cumulative (replace) for Whisper variants. Chosen from cfg.Model via
	// deltaStrategyForModel at stream construction; tests may set it
	// directly. A nil value falls back to incremental in appendPartial.
	mergeDelta func(existing, delta string) string

	// postCommitSpan receives one event per post-commit inbound message
	// (delta/completed) so the wait_final span shows exactly when Whisper
	// emitted each result. Set by SetPostCommitSpan just before commit and
	// cleared when wait_final ends. Atomic pointer lets readLoop read it
	// without racing the caller that sets/clears it.
	postCommitSpan atomic.Pointer[postCommitRef]

	// statsMu guards the entire stats value. Counts/totals here feed the
	// session-span closing attributes plus the PreCommitContext /
	// PostCommitStats projections used by the commit / wait_final spans.
	statsMu sync.Mutex
	stats   Stats
}

// Stats accumulates everything the session needs to report at the end of
// a dictation: per-message counts, audio totals, and the timestamps that
// drive PreCommitContext / PostCommitStats. Mutated only via Stream
// helpers under statsMu; snapshots are returned by value.
type Stats struct {
	InboundCounts        map[string]int
	OutboundCounts       map[string]int
	AppendBytes          int64
	LastInboundType      string
	LastInboundAt        time.Time
	FirstSpeechStartedAt time.Time
	LastSpeechStoppedAt  time.Time
	SpeechStartedCount   int
	SpeechStoppedCount   int
	// InPostCommittedSilence is true while vocis is in the silent window
	// that follows a server-VAD-driven input_audio_buffer.committed and
	// before the next speech_started. Finalize consults it to skip an
	// unnecessary final commit (and the "buffer too small" empty-commit
	// error log) when the user releases the hotkey during that gap.
	InPostCommittedSilence bool
	CommitAt               time.Time
	FirstPostCommitType    string
	FirstPostCommitAt      time.Time
	FirstPostCommitDelta   string
	DeltaCountPostCommit   int
	CompletedText          string
	CompletedAt            time.Time
}

func newStats() Stats {
	return Stats{
		InboundCounts:  make(map[string]int),
		OutboundCounts: make(map[string]int),
	}
}

// PreCommitContext is the snapshot of inbound state at the moment the caller
// is about to send input_audio_buffer.commit. It lets the commit span answer
// "did Lemonade's VAD already fire speech_stopped?" without having to crawl
// span events by hand.
type PreCommitContext struct {
	LastInboundType         string
	MsSinceLastInbound      int64
	SpeechStoppedSeenBefore bool
	MsSinceSpeechStopped    int64
	AppendBytes             int64
	AppendApproxMs          int64
	// BufferDrainedByServerVAD is true when server VAD has already fired
	// an input_audio_buffer.committed for the most recent utterance and
	// no new speech_started has been observed since — i.e. the buffer is
	// empty and sending a final commit would just produce the harmless
	// "buffer too small" error. Lets Finalize skip the commit entirely.
	BufferDrainedByServerVAD bool
}

// PostCommitStats summarizes everything that arrived between commit and the
// completed/failed/timeout terminal event. The wait_final span attaches it
// so a single trace tells you whether the commit→completed gap is the
// redundant "second Whisper inference" (delta_eq_completed=true) or
// something else.
type PostCommitStats struct {
	FirstEventType    string
	FirstEventMs      int64
	FirstDeltaText    string
	FirstDeltaTextLen int
	CompletedText     string
	CompletedTextLen  int
	DeltaEqCompleted  bool
	DeltaCount        int
	CompletedMs       int64
}

func (c *Client) StartStream(ctx context.Context, sampleRate, channels int) (*Stream, error) {
	if sampleRate <= 0 {
		return nil, errors.New("recording.sample_rate must be greater than zero")
	}
	if channels <= 0 {
		return nil, errors.New("recording.channels must be greater than zero")
	}

	// Parent span: lives from connect through Close(), one per dictation
	// session. All realtime message events are attached here.
	sessionCtx, sessionSpan := telemetry.StartSpan(ctx, "vocis.transcribe.session",
		attribute.String("transcribe.backend", string(c.backendName())),
		attribute.String("transcribe.model", c.cfg.Model),
		attribute.Int("audio.sample_rate", sampleRate),
		attribute.Int("audio.channels", channels),
	)

	connectCtx, connectCancel := context.WithTimeout(sessionCtx, connectTimeout)
	defer connectCancel()

	connectCtx, connectSpan := telemetry.StartSpan(connectCtx, "vocis.transcribe.connect")

	conn, err := c.transport.Dial(connectCtx)
	if err != nil {
		telemetry.EndSpan(connectSpan, err)
		telemetry.EndSpan(sessionSpan, err)
		return nil, err
	}
	connectSpan.AddEvent("websocket.dialed")

	stream := &Stream{
		conn:         conn,
		encoder:      newPCMEncoder(sampleRate, c.transport.SampleRate(), channels),
		writeTimeout: c.writeTimeout,
		sessionSpan:  sessionSpan,
		sessionStart: time.Now(),
		targetRate:   c.transport.SampleRate(),
		readyCh:      make(chan error, 1),
		events:       make(chan StreamEvent, 16),
		readDone:     make(chan struct{}),
		stats:        newStats(),
		mergeDelta:   deltaStrategyForModel(c.cfg.Model),
	}
	go stream.readLoop()

	sessionUpdate := c.transport.SessionUpdate()
	if raw, err := json.Marshal(sessionUpdate); err == nil {
		sessionlog.Debugf("realtime: sending session.update payload=%s", string(raw))
	}
	if err := stream.sendJSON(connectCtx, sessionUpdate); err != nil {
		stream.Close()
		telemetry.EndSpan(connectSpan, err)
		return nil, err
	}
	connectSpan.AddEvent("session.update.sent")
	stream.recordOutbound("session.update")

	telemetry.EndSpan(connectSpan, nil)
	return stream, nil
}

func (c *Client) backendName() string {
	if c.cfg.Backend == "" {
		return config.BackendOpenAI
	}
	return c.cfg.Backend
}

func (s *Stream) Append(ctx context.Context, samples []int16) error {
	payload := s.encoder.Encode(samples)
	if len(payload) == 0 {
		return nil
	}
	if err := s.sendJSON(ctx, map[string]any{
		"type":  "input_audio_buffer.append",
		"audio": base64.StdEncoding.EncodeToString(payload),
	}); err != nil {
		return err
	}
	s.statsMu.Lock()
	s.stats.AppendBytes += int64(len(payload))
	s.stats.OutboundCounts["input_audio_buffer.append"]++
	s.statsMu.Unlock()
	return nil
}

// AppendSilence sends zero-valued PCM16 for the given duration at the
// backend's target sample rate. Used to pad the audio buffer with a
// silent tail before the finalize commit so Whisper-family models can
// segment the last word — without the pad, the trailing word is often
// dropped because the model treats a cut-off frame as indeterminate.
// A zero or negative duration is a no-op.
func (s *Stream) AppendSilence(ctx context.Context, durationMS int) error {
	if durationMS <= 0 || s.targetRate <= 0 {
		return nil
	}
	sampleCount := int64(durationMS) * int64(s.targetRate) / 1000
	if sampleCount <= 0 {
		return nil
	}
	// Mono PCM16: 2 bytes per sample. A zeroed byte slice serializes
	// as digital silence at any bit depth we care about.
	payload := make([]byte, sampleCount*2)
	if err := s.sendJSON(ctx, map[string]any{
		"type":  "input_audio_buffer.append",
		"audio": base64.StdEncoding.EncodeToString(payload),
	}); err != nil {
		return err
	}
	s.statsMu.Lock()
	s.stats.AppendBytes += int64(len(payload))
	s.stats.OutboundCounts["input_audio_buffer.append"]++
	s.statsMu.Unlock()
	return nil
}

func (s *Stream) Commit(ctx context.Context) error {
	err := s.sendJSON(ctx, map[string]any{"type": "input_audio_buffer.commit"})
	if err == nil {
		s.statsMu.Lock()
		s.stats.CommitAt = time.Now()
		s.statsMu.Unlock()
		s.recordOutbound("input_audio_buffer.commit")
	}
	return err
}

func (s *Stream) Partial() string {
	s.partialMu.Lock()
	defer s.partialMu.Unlock()
	return s.partial
}

// HasPendingTranscription is true when speech_started events outnumber
// completed events — i.e., Lemonade is still running Whisper on a turn
// we already streamed. Lets callers skip a fixed wait when no turn is
// in flight.
func (s *Stream) HasPendingTranscription() bool {
	s.statsMu.Lock()
	defer s.statsMu.Unlock()
	completed := s.stats.InboundCounts["conversation.item.input_audio_transcription.completed"]
	return s.stats.SpeechStartedCount > completed
}

func (s *Stream) Events() <-chan StreamEvent { return s.events }

func (s *Stream) Close() error {
	var err error
	s.closeOnce.Do(func() {
		_ = s.conn.WriteControl(
			websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseNormalClosure, ""),
			time.Now().Add(2*time.Second),
		)
		err = s.conn.Close()
		if s.sessionSpan != nil {
			s.setSessionSummaryAttrs()
			telemetry.EndSpan(s.sessionSpan, nil)
		}
	})
	return err
}

// recordInbound is the per-message hook: emit a span event for tracing
// AND fold the message into the running Stats. The two jobs are
// independent and live in their own helpers below.
func (s *Stream) recordInbound(msg jsonMessage) {
	now := time.Now()
	if s.sessionSpan != nil {
		name := inboundEventName(msg.Type())
		s.sessionSpan.AddEvent(name,
			trace.WithAttributes(inboundAttrs(now.Sub(s.sessionStart), msg)...))
	}
	if ref := s.postCommitSpan.Load(); ref != nil {
		if name, attrs, ok := postCommitEventAttrs(now.Sub(ref.startedAt), msg); ok {
			ref.span.AddEvent(name, trace.WithAttributes(attrs...))
		}
	}
	s.statsMu.Lock()
	s.stats.observeInbound(now, msg)
	s.statsMu.Unlock()
}

// postCommitRef pairs the wait_final span with the commit timestamp so
// per-message events carry their offset since commit — the thing the
// reader actually wants to see on the timeline.
type postCommitRef struct {
	span      trace.Span
	startedAt time.Time
}

// SetPostCommitSpan attaches the wait_final span so recordInbound can
// add per-message events to it. No pairing clear: once wait_final ends,
// AddEvent calls on the ended span are no-ops, and the Stream is
// short-lived enough that holding the reference doesn't matter.
func (s *Stream) SetPostCommitSpan(span trace.Span, startedAt time.Time) {
	s.postCommitSpan.Store(&postCommitRef{span: span, startedAt: startedAt})
}

// postCommitEventAttrs maps a post-commit inbound message to a span event
// name + attributes. Returns ok=false for uninteresting message types
// (session.updated, speech_started after the fact, etc.) so the
// wait_final span timeline stays focused on what actually blocks it:
// deltas and the terminal completed/failed event.
func postCommitEventAttrs(sinceCommit time.Duration, msg jsonMessage) (string, []attribute.KeyValue, bool) {
	msgType := msg.Type()
	base := []attribute.KeyValue{
		attribute.Int64("since_commit_ms", sinceCommit.Milliseconds()),
	}
	switch msgType {
	case "conversation.item.input_audio_transcription.delta":
		attrs := base
		if d, ok := msg["delta"].(string); ok {
			attrs = append(attrs,
				attribute.Int("delta.text_len", len(d)),
				attribute.String("delta.text", truncate(d, 80)),
			)
		}
		return "realtime.delta", attrs, true
	case "conversation.item.input_audio_transcription.completed":
		attrs := base
		if t, ok := msg["transcript"].(string); ok {
			attrs = append(attrs,
				attribute.Int("transcript.text_len", len(t)),
				attribute.String("transcript.text", truncate(t, 80)),
			)
		}
		return "realtime.completed", attrs, true
	case "conversation.item.input_audio_transcription.failed":
		return "realtime.failed", base, true
	}
	return "", nil, false
}

// inboundEventName maps a WS inbound message type to the span event name
// used on the session span. Transcription-specific frames get distinct
// names so delta/completed/failed are individually visible on the Jaeger
// timeline; everything else stays under the generic realtime.inbound
// bucket so session.updated / buffer events don't clutter the view.
func inboundEventName(msgType string) string {
	switch msgType {
	case "conversation.item.input_audio_transcription.delta":
		return "realtime.transcription.delta"
	case "conversation.item.input_audio_transcription.completed":
		return "realtime.transcription.completed"
	case "conversation.item.input_audio_transcription.failed":
		return "realtime.transcription.failed"
	default:
		return "realtime.inbound"
	}
}

// inboundAttrs builds the OpenTelemetry attrs for a single inbound
// message — pure function of (elapsed, msg) so it's trivially testable
// and free of side effects.
func inboundAttrs(elapsed time.Duration, msg jsonMessage) []attribute.KeyValue {
	msgType := msg.Type()
	attrs := []attribute.KeyValue{
		attribute.String("message.type", msgType),
		attribute.String("elapsed", elapsed.Round(time.Millisecond).String()),
	}
	if id, ok := msg["item_id"].(string); ok && id != "" {
		attrs = append(attrs, attribute.String("item_id", id))
	}
	switch msgType {
	case "conversation.item.input_audio_transcription.delta":
		if d, ok := msg["delta"].(string); ok {
			attrs = append(attrs,
				attribute.Int("delta.text_len", len(d)),
				attribute.String("delta.text", truncate(d, 80)),
			)
		}
	case "conversation.item.input_audio_transcription.completed":
		if t, ok := msg["transcript"].(string); ok {
			attrs = append(attrs,
				attribute.Int("transcript.text_len", len(t)),
				attribute.String("transcript.text", truncate(t, 80)),
			)
		}
	case "input_audio_buffer.speech_started":
		if v, ok := numericField(msg, "audio_start_ms"); ok {
			attrs = append(attrs, attribute.Int64("audio_start_ms", v))
		}
	case "input_audio_buffer.speech_stopped":
		if v, ok := numericField(msg, "audio_end_ms"); ok {
			attrs = append(attrs, attribute.Int64("audio_end_ms", v))
		}
	}
	return attrs
}

// observeInbound folds one inbound message into the running stats.
// Caller holds the statsMu lock.
func (st *Stats) observeInbound(now time.Time, msg jsonMessage) {
	msgType := msg.Type()
	st.InboundCounts[msgType]++
	st.LastInboundType = msgType
	st.LastInboundAt = now

	switch msgType {
	case "input_audio_buffer.speech_started":
		st.SpeechStartedCount++
		if st.FirstSpeechStartedAt.IsZero() {
			st.FirstSpeechStartedAt = now
		}
		// New utterance underway → buffer has content again.
		st.InPostCommittedSilence = false
	case "input_audio_buffer.speech_stopped":
		st.SpeechStoppedCount++
		st.LastSpeechStoppedAt = now
	case "input_audio_buffer.committed":
		// Server-VAD-driven commit drained the buffer. Silent frames may
		// still be flowing from the client, but they won't cause another
		// utterance until VAD fires speech_started.
		st.InPostCommittedSilence = true
	case "conversation.item.input_audio_transcription.delta":
		if !st.CommitAt.IsZero() && now.After(st.CommitAt) {
			st.DeltaCountPostCommit++
			if st.FirstPostCommitType == "" {
				st.FirstPostCommitType = "delta"
				st.FirstPostCommitAt = now
			}
			if d, ok := msg["delta"].(string); ok && d != "" && st.FirstPostCommitDelta == "" {
				st.FirstPostCommitDelta = d
			}
		}
	case "conversation.item.input_audio_transcription.completed":
		if !st.CommitAt.IsZero() && st.FirstPostCommitType == "" {
			st.FirstPostCommitType = "completed"
			st.FirstPostCommitAt = now
		}
		if t, ok := msg["transcript"].(string); ok {
			st.CompletedText = t
		}
		st.CompletedAt = now
	}
}

// recordOutbound mirrors recordInbound for messages vocis sends to the
// backend. input_audio_buffer.append is intentionally skipped at the span
// level (too high-rate to be readable) but we still tally it in
// outboundCounts / appendBytes so the session-end summary captures it.
func (s *Stream) recordOutbound(msgType string) {
	if s.sessionSpan == nil {
		return
	}
	s.sessionSpan.AddEvent("realtime.outbound", trace.WithAttributes(
		attribute.String("message.type", msgType),
		attribute.String("elapsed", time.Since(s.sessionStart).Round(time.Millisecond).String()),
	))
	s.statsMu.Lock()
	s.stats.OutboundCounts[msgType]++
	s.statsMu.Unlock()
}

// PreCommitContext returns the inbound state right before commit, used by
// the commit span to record whether VAD already fired speech_stopped (so
// "fast vs slow path" is visible in a single attribute).
func (s *Stream) PreCommitContext() PreCommitContext {
	s.statsMu.Lock()
	defer s.statsMu.Unlock()
	now := time.Now()
	ctx := PreCommitContext{
		LastInboundType: s.stats.LastInboundType,
		AppendBytes:     s.stats.AppendBytes,
		// 16-bit PCM mono → 2 bytes per sample. Total samples = bytes/2.
		// Audio ms = samples * 1000 / sample_rate.
		AppendApproxMs: 0,
	}
	if s.targetRate > 0 {
		ctx.AppendApproxMs = (s.stats.AppendBytes / 2) * 1000 / int64(s.targetRate)
	}
	if !s.stats.LastInboundAt.IsZero() {
		ctx.MsSinceLastInbound = now.Sub(s.stats.LastInboundAt).Milliseconds()
	}
	if !s.stats.LastSpeechStoppedAt.IsZero() {
		ctx.SpeechStoppedSeenBefore = true
		ctx.MsSinceSpeechStopped = now.Sub(s.stats.LastSpeechStoppedAt).Milliseconds()
	}
	ctx.BufferDrainedByServerVAD = s.stats.InPostCommittedSilence
	return ctx
}

// PostCommitStats returns the post-commit timeline. Call after waitForFinal
// returns (success, timeout, or error). delta_eq_completed answers the
// "could we skip waiting for completed?" question directly.
func (s *Stream) PostCommitStats() PostCommitStats {
	s.statsMu.Lock()
	defer s.statsMu.Unlock()
	stats := PostCommitStats{
		FirstEventType:    s.stats.FirstPostCommitType,
		FirstDeltaText:    truncate(s.stats.FirstPostCommitDelta, 80),
		FirstDeltaTextLen: len(s.stats.FirstPostCommitDelta),
		CompletedText:     truncate(s.stats.CompletedText, 80),
		CompletedTextLen:  len(s.stats.CompletedText),
		DeltaCount:        s.stats.DeltaCountPostCommit,
	}
	if s.stats.FirstPostCommitDelta != "" && s.stats.CompletedText != "" {
		stats.DeltaEqCompleted = strings.TrimSpace(s.stats.FirstPostCommitDelta) == strings.TrimSpace(s.stats.CompletedText)
	}
	if !s.stats.CommitAt.IsZero() {
		if !s.stats.FirstPostCommitAt.IsZero() {
			stats.FirstEventMs = s.stats.FirstPostCommitAt.Sub(s.stats.CommitAt).Milliseconds()
		}
		if !s.stats.CompletedAt.IsZero() {
			stats.CompletedMs = s.stats.CompletedAt.Sub(s.stats.CommitAt).Milliseconds()
		}
	}
	return stats
}

// SetSessionSummaryAttrs writes the running counts/totals onto the session
// span. Called by Close() so traces show per-message-type counts without
// having to walk every span event.
func (s *Stream) setSessionSummaryAttrs() {
	if s.sessionSpan == nil {
		return
	}
	s.statsMu.Lock()
	defer s.statsMu.Unlock()
	attrs := []attribute.KeyValue{
		attribute.Int64("session.duration_ms", time.Since(s.sessionStart).Milliseconds()),
		attribute.Int("inbound.deltas_count", s.stats.InboundCounts["conversation.item.input_audio_transcription.delta"]),
		attribute.Int("inbound.completed_count", s.stats.InboundCounts["conversation.item.input_audio_transcription.completed"]),
		attribute.Int("inbound.speech_started_count", s.stats.SpeechStartedCount),
		attribute.Int("inbound.speech_stopped_count", s.stats.SpeechStoppedCount),
		attribute.Int("inbound.committed_count", s.stats.InboundCounts["input_audio_buffer.committed"]),
		attribute.Int("outbound.append_count", s.stats.OutboundCounts["input_audio_buffer.append"]),
		attribute.Int64("outbound.append_bytes", s.stats.AppendBytes),
	}
	if s.targetRate > 0 {
		attrs = append(attrs, attribute.Int64("outbound.append_total_audio_ms", (s.stats.AppendBytes/2)*1000/int64(s.targetRate)))
	}
	if !s.stats.FirstSpeechStartedAt.IsZero() {
		attrs = append(attrs, attribute.Int64("session.first_speech_started_at_ms", s.stats.FirstSpeechStartedAt.Sub(s.sessionStart).Milliseconds()))
	}
	s.sessionSpan.SetAttributes(attrs...)
}

// truncate returns at most max bytes of s with a trailing "…" if it was
// truncated. Span events should not carry full transcripts (could be
// arbitrarily long); 80 chars is enough to recognize the text at a glance.
func truncate(s string, max int) string {
	if len(s) <= max {
		return s
	}
	return s[:max] + "…"
}

// numericField pulls an integer-ish value from a JSON-decoded map. JSON
// numbers come in as float64; some Lemonade events also use ints.
func numericField(m jsonMessage, key string) (int64, bool) {
	switch v := m[key].(type) {
	case float64:
		return int64(v), true
	case int64:
		return v, true
	case int:
		return int64(v), true
	}
	return 0, false
}

func (s *Stream) waitReady(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case err := <-s.readyCh:
		return err
	case <-s.readDone:
		return errors.New("openai realtime stream closed before session became ready")
	}
}

func (s *Stream) sendJSON(ctx context.Context, payload any) error {
	deadline := time.Now().Add(s.writeTimeout)
	if dl, ok := ctx.Deadline(); ok && dl.Before(deadline) {
		deadline = dl
	}
	data, err := json.Marshal(payload)
	if err != nil {
		return err
	}
	dumpWSFrame("→", data)
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := s.conn.SetWriteDeadline(deadline); err != nil {
		return err
	}
	return s.conn.WriteMessage(websocket.TextMessage, data)
}

// ---------------------------------------------------------------------------
// DictationSession — high-level recording-to-text session
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
	// intends to feed through the session, when known upfront. Used by
	// waitForFinal to scale its post-commit timeout proportionally — a
	// batch commit of 20 min of pre-recorded audio needs ~minutes for
	// a local model to transcribe, so the fixed 15 s wait_final floor
	// would fire too soon. 0 = unknown (PTT serve path), waitForFinal
	// falls back to wall-clock trailingDuration scaling.
	ExpectedAudioMS int
}

type DictationSession struct {
	writeTimeout          time.Duration
	waitFinalFloorSeconds int
	expectedAudioMS       int
	tailSilenceMS         int
	cancel                context.CancelFunc
	callbacks             ConnectCallbacks
	// hallucinationFilters is the set of exact transcripts to drop,
	// keyed by lowercased+trimmed text for case-insensitive match.
	hallucinationFilters map[string]bool

	events   chan DictationEvent
	pumpDone chan error
	finals   chan finalResult

	// All state below is accessed across goroutines (caller, pump,
	// consumeStreamEvents). Atomics keep the call sites readable —
	// no mutex bookkeeping, just Load/Store/Add. Timestamps live as
	// UnixNano int64 so atomic.Int64 covers them.
	stream        atomic.Pointer[Stream]
	liveSegments  atomic.Bool
	hasTrailing   atomic.Bool
	segmentCount  atomic.Int32
	streamReadyAt atomic.Int64 // UnixNano; 0 = unset
	lastSegmentAt atomic.Int64 // UnixNano; 0 = unset
}

type finalResult struct {
	text string
	err  error
}

func (c *Client) StartDictation(ctx context.Context, opts DictationOpts) (*DictationSession, error) {
	if opts.SampleRate <= 0 {
		return nil, errors.New("recording.sample_rate must be greater than zero")
	}
	if opts.Channels <= 0 {
		return nil, errors.New("recording.channels must be greater than zero")
	}

	pumpCtx, cancel := context.WithCancel(ctx)
	session := &DictationSession{
		writeTimeout:          c.writeTimeout,
		waitFinalFloorSeconds: c.streaming.WaitFinalSeconds,
		expectedAudioMS:       opts.ExpectedAudioMS,
		tailSilenceMS:         c.streaming.TailSilenceMS,
		cancel:                cancel,
		callbacks:             opts.Callbacks,
		hallucinationFilters:  buildHallucinationSet(c.cfg.HallucinationFilters),
		events:                make(chan DictationEvent, 16),
		pumpDone:              make(chan error, 1),
		finals:                make(chan finalResult, 8),
	}
	session.liveSegments.Store(true)
	go session.run(pumpCtx, c, opts.SampleRate, opts.Channels, opts.Samples)
	return session, nil
}

func (s *DictationSession) Events() <-chan DictationEvent { return s.events }

// Finalize waits for the audio pump to finish, then collects any trailing
// transcript that arrived after the last live segment.
func (s *DictationSession) Finalize(ctx context.Context) (FinalizeResult, error) {
	s.liveSegments.Store(false)

	// Wait for the pump to exit naturally (samples channel closes).
	// Only cancel as a fallback if the finalization context expires.
	var pumpErr error
	select {
	case pumpErr = <-s.pumpDone:
	case <-ctx.Done():
		s.cancel()
		s.closeStream()
		return FinalizeResult{}, ctx.Err()
	}
	// Pump exited. Defer cancel to clean up consumeStreamEvents AFTER
	// we've collected trailing results.
	defer s.cancel()

	if pumpErr != nil {
		s.closeStream()
		return FinalizeResult{}, pumpErr
	}
	defer s.closeStream()

	stream := s.stream.Load()
	if stream == nil {
		return FinalizeResult{}, errors.New("realtime transcription stream was not established")
	}

	return s.collectTrailing(ctx, stream)
}

// collectTrailing drains any pending segment results, optionally commits
// remaining audio, and waits for the final transcript.
func (s *DictationSession) collectTrailing(ctx context.Context, stream *Stream) (FinalizeResult, error) {
	// Phase 1: drain segments that arrived between recording stop and now.
	_, drainSpan := telemetry.StartSpan(ctx, "vocis.transcribe.drain")
	text, err := s.drainPendingSegments(ctx, stream)
	telemetry.EndSpan(drainSpan, err)
	if err != nil {
		return FinalizeResult{}, err
	}

	// If all audio was already consumed by live segments, we're done.
	if !s.hasTrailing.Load() && strings.TrimSpace(stream.Partial()) == "" {
		return FinalizeResult{Text: text}, nil
	}

	// Server-VAD fast-path: if the last server event was
	// input_audio_buffer.committed and no new speech_started has fired,
	// the backend buffer is empty and a final commit would just return
	// "buffer too small". Short-circuit to avoid the round-trip, the log
	// warn, and the waitForFinal stall. Only safe when there's no
	// in-flight partial — a non-empty partial means a completed event is
	// still owed to us.
	pre := stream.PreCommitContext()
	if pre.BufferDrainedByServerVAD && strings.TrimSpace(stream.Partial()) == "" {
		return FinalizeResult{Text: text}, nil
	}

	// Phase 2: commit trailing audio and wait for the final transcript.
	// Pad with silence first so Whisper-family models can segment the
	// last word — without it, releasing the hotkey mid-word routinely
	// eats the trailing word from the transcript. Traced explicitly
	// because dumpWSFrame skips all outbound input_audio_buffer.append
	// frames to avoid audio-chunk spam, so the append itself is
	// invisible in the protocol log.
	if s.tailSilenceMS > 0 {
		sessionlog.Tracef("ws → input_audio_buffer.append (tail silence pad: %dms)", s.tailSilenceMS)
		if err := stream.AppendSilence(ctx, s.tailSilenceMS); err != nil {
			sessionlog.Warnf("tail silence pad failed: %v", err)
		}
	}
	_, commitSpan := telemetry.StartSpan(ctx, "vocis.transcribe.commit",
		attribute.Int64("commit.pending_audio_bytes", pre.AppendBytes),
		attribute.Int64("commit.pending_audio_ms", pre.AppendApproxMs),
		attribute.String("commit.last_inbound_type", pre.LastInboundType),
		attribute.Int64("commit.ms_since_last_inbound", pre.MsSinceLastInbound),
		attribute.Bool("commit.speech_stopped_seen_before_commit", pre.SpeechStoppedSeenBefore),
		attribute.Int64("commit.ms_since_speech_stopped", pre.MsSinceSpeechStopped),
		attribute.Int("commit.tail_silence_ms", s.tailSilenceMS),
	)
	err = stream.Commit(ctx)
	if err != nil && s.canSkipEmptyCommit(err, stream) {
		commitSpan.SetAttributes(attribute.Bool("commit.skipped", true))
		telemetry.EndSpan(commitSpan, nil)
		return FinalizeResult{Text: text}, nil
	}
	telemetry.EndSpan(commitSpan, err)
	if err != nil {
		return FinalizeResult{}, err
	}

	waitStartedAt := time.Now()
	_, waitSpan := telemetry.StartSpan(ctx, "vocis.transcribe.wait_final",
		attribute.String("trailing_duration", s.trailingDuration().Round(10*time.Millisecond).String()),
		attribute.Int("segment_count", int(s.segmentCount.Load())),
	)
	stream.SetPostCommitSpan(waitSpan, waitStartedAt)
	finalText, err := s.waitForFinal(ctx)
	addPostCommitAttrs(waitSpan, stream.PostCommitStats())
	if err != nil && (s.canSkipTrailingTimeout(err, stream) || s.canSkipEmptyCommit(err, stream)) {
		waitSpan.SetAttributes(attribute.Bool("trailing.skipped", true))
		telemetry.EndSpan(waitSpan, nil)
		return FinalizeResult{Text: text}, nil
	}
	telemetry.EndSpan(waitSpan, err)
	if err != nil {
		return FinalizeResult{}, err
	}

	return FinalizeResult{Text: appendSegmentText(text, finalText)}, nil
}

// addPostCommitAttrs attaches the post-commit timeline to the wait_final
// span. Pulled out of collectTrailing so the same logic can be reused on
// success, timeout, and error paths.
func addPostCommitAttrs(span trace.Span, stats PostCommitStats) {
	if span == nil {
		return
	}
	span.SetAttributes(
		attribute.String("wait_final.first_event_type", stats.FirstEventType),
		attribute.Int64("wait_final.first_event_ms", stats.FirstEventMs),
		attribute.Int("wait_final.first_delta_text_len", stats.FirstDeltaTextLen),
		attribute.String("wait_final.first_delta_text", stats.FirstDeltaText),
		attribute.Int("wait_final.completed_text_len", stats.CompletedTextLen),
		attribute.String("wait_final.completed_text", stats.CompletedText),
		attribute.Bool("wait_final.delta_eq_completed", stats.DeltaEqCompleted),
		attribute.Int("wait_final.delta_count", stats.DeltaCount),
		attribute.Int64("wait_final.completed_ms", stats.CompletedMs),
	)
}

// ---------------------------------------------------------------------------
// Pump — connects, buffers early audio, then streams to OpenAI
// ---------------------------------------------------------------------------

func (s *DictationSession) run(
	ctx context.Context,
	client *Client,
	sampleRate, channels int,
	samples <-chan []int16,
) {
	stream, buffered, err := s.connectAndBuffer(ctx, client, sampleRate, channels, samples)
	if err != nil {
		s.finishPump(err)
		return
	}

	s.setStream(stream)
	sessionlog.Infof("realtime transcription stream ready")
	go s.consumeStreamEvents(ctx)

	// Client-side VAD drives mid-session pause commits when configured.
	// Only active when manual_commit is on (server VAD is off) — the
	// config validator enforces this pairing.
	var vad *SileroVAD
	if client.streaming.ClientVAD && client.streaming.ManualCommit {
		if err := initSilero(client.streaming.OnnxruntimeLibrary); err != nil {
			sessionlog.Warnf("silero VAD init failed, disabling client VAD: %v", err)
		} else if sampleRate != sileroSampleRate {
			sessionlog.Warnf("silero VAD expects 16 kHz but sampleRate=%d; disabling client VAD", sampleRate)
		} else if v, err := NewSileroVAD(
			client.streaming.SilenceDurationMS,
			client.streaming.PrefixPaddingMS,
			client.streaming.MinUtteranceMS,
		); err != nil {
			sessionlog.Warnf("silero VAD construction failed, disabling client VAD: %v", err)
		} else {
			vad = v
			sessionlog.Infof(
				"client VAD (silero): silence=%dms prefix=%dms min_utterance=%dms",
				client.streaming.SilenceDurationMS,
				client.streaming.PrefixPaddingMS,
				client.streaming.MinUtteranceMS,
			)
		}
	}

	// Flush buffered audio that arrived while connecting.
	for _, chunk := range buffered {
		if err := stream.Append(ctx, chunk); err != nil {
			s.finishPump(err)
			return
		}
		s.maybePauseCommit(ctx, stream, vad, chunk)
		if vad == nil || vad.InSpeech() || vad.SpeechMs() > 0 {
			s.hasTrailing.Store(true)
		}
	}

	// Stream live audio until the samples channel closes or context cancels.
	s.streamAudio(ctx, stream, samples, vad)
}

// maybePauseCommit feeds a chunk into the client VAD (if enabled) and
// sends input_audio_buffer.commit when the VAD transitions to silence.
// Idempotent when vad is nil. A commit failure is logged but not fatal —
// the stream is still usable and Finalize will retry a final commit.
func (s *DictationSession) maybePauseCommit(
	ctx context.Context,
	stream *Stream,
	vad *SileroVAD,
	chunk []int16,
) {
	if vad == nil {
		return
	}
	if evt := vad.Feed(chunk); evt == VADSpeechStopped {
		if err := stream.Commit(ctx); err != nil {
			sessionlog.Warnf("client VAD pause commit failed: %v", err)
			return
		}
		vad.Reset()
		sessionlog.Debugf("client VAD pause commit sent")
	}
}

// connectAndBuffer starts the WebSocket connection in the background and
// buffers incoming audio samples until the connection is ready. Returns the
// connected stream and any buffered chunks.
func (s *DictationSession) connectAndBuffer(
	ctx context.Context,
	client *Client,
	sampleRate, channels int,
	samples <-chan []int16,
) (*Stream, [][]int16, error) {
	type result struct {
		stream *Stream
		err    error
	}

	var buffered [][]int16
	samplesOpen := true

	for attempt := 1; attempt <= maxConnectRetries; attempt++ {
		if s.callbacks.OnConnecting != nil {
			s.callbacks.OnConnecting(attempt, maxConnectRetries)
		}

		connectCh := make(chan result, 1)
		go func() {
			stream, err := client.StartStream(ctx, sampleRate, channels)
			connectCh <- result{stream, err}
		}()

		waiting := true
		for waiting {
			select {
			case <-ctx.Done():
				return nil, nil, ctx.Err()
			case r := <-connectCh:
				if r.err == nil {
					if s.callbacks.OnConnected != nil {
						s.callbacks.OnConnected()
					}
					return r.stream, buffered, nil
				}
				if attempt < maxConnectRetries {
					sessionlog.Warnf("connect attempt %d/%d failed: %v", attempt, maxConnectRetries, r.err)
				} else {
					return nil, nil, fmt.Errorf("start transcription stream: %w", r.err)
				}
				waiting = false
			case chunk, ok := <-samples:
				if !ok {
					samplesOpen = false
					samples = nil
					// Samples closed — wait for this connect attempt to finish.
					select {
					case <-ctx.Done():
						return nil, nil, ctx.Err()
					case r := <-connectCh:
						if r.err == nil {
							return r.stream, buffered, nil
						}
						if attempt < maxConnectRetries {
							sessionlog.Warnf("connect attempt %d/%d failed: %v", attempt, maxConnectRetries, r.err)
							waiting = false
						} else {
							return nil, nil, fmt.Errorf("start transcription stream: %w", r.err)
						}
					}
				} else if len(chunk) > 0 {
					buffered = append(buffered, chunk)
				}
			}
		}

		// Don't retry if recording already stopped — just fail.
		if !samplesOpen {
			return nil, nil, fmt.Errorf("start transcription stream: all %d attempts failed", maxConnectRetries)
		}
	}

	return nil, nil, errors.New("start transcription stream: exhausted retries")
}

// streamAudio reads from the samples channel and appends to the stream.
// If vad is non-nil, each chunk is also fed through the pause detector
// and a commit is sent on speech_stopped — turning a long hold into
// multiple transcribed segments without waiting for hotkey release.
func (s *DictationSession) streamAudio(
	ctx context.Context,
	stream *Stream,
	samples <-chan []int16,
	vad *SileroVAD,
) {
	var prevInSpeech bool
	var havePrev bool
	for {
		select {
		case <-ctx.Done():
			s.finishPump(nil)
			return
		case chunk, ok := <-samples:
			if !ok {
				s.finishPump(nil)
				return
			}
			if len(chunk) == 0 {
				continue
			}
			if err := stream.Append(ctx, chunk); err != nil {
				s.finishPump(err)
				return
			}
			s.maybePauseCommit(ctx, stream, vad, chunk)
			// Mark uncommitted audio as "trailing" only when we
			// believe it contains speech. During post-commit silence
			// the chunks still flow to the server, but flagging them
			// would defeat the release-time shortcut in
			// collectTrailing, forcing a redundant final commit and
			// wait for an (almost always empty) transcript.
			if vad == nil || vad.InSpeech() || vad.SpeechMs() > 0 {
				s.hasTrailing.Store(true)
			}

			// Log client VAD only on state transitions so the trace
			// shows when speech starts/stops rather than repeating
			// the same in_speech=true line every chunk.
			if vad != nil {
				snap := vad.Snapshot()
				if !havePrev || snap.InSpeech != prevInSpeech {
					sessionlog.Debugf(
						"client VAD: in_speech=%t last=%.4f max=%.4f min=%.4f silence_ms=%d speech_ms=%d",
						snap.InSpeech, snap.LastProb, snap.MaxProb, snap.MinProb, snap.SilenceMs, snap.SpeechMs,
					)
					prevInSpeech = snap.InSpeech
					havePrev = true
				}
			}
		}
	}
}

// ---------------------------------------------------------------------------
// Stream event handling
// ---------------------------------------------------------------------------

func (s *DictationSession) consumeStreamEvents(ctx context.Context) {
	stream := s.stream.Load()
	if stream == nil {
		return
	}
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-stream.Events():
			if !ok {
				return
			}
			if err := s.handleStreamEvent(event); err != nil {
				s.pushFinal("", err)
				return
			}
		}
	}
}

func (s *DictationSession) handleStreamEvent(event StreamEvent) error {
	switch event.Type {
	case StreamEventPartial:
		s.emitPartial(event.Text)
		return nil
	case StreamEventFinal:
		text := strings.TrimSpace(event.Text)
		if text == "" {
			return nil
		}
		if s.isHallucination(text) {
			sessionlog.Infof("dropped hallucinated final: %q", text)
			s.hasTrailing.Store(false)
			// If Finalize is already waiting on the finals channel
			// (liveSegments flipped to false), we must still push
			// something to unblock it — otherwise waitForFinal hangs
			// until timeout. Push an empty result so finalize returns
			// "no speech" cleanly.
			if !s.liveSegments.Load() {
				s.pushFinal("", nil)
			}
			return nil
		}
		s.hasTrailing.Store(false)
		s.lastSegmentAt.Store(time.Now().UnixNano())
		text = s.formatSegmentText(text)
		if s.liveSegments.Load() {
			s.emitEvent(DictationEvent{Type: DictationEventSegment, Text: text})
			return nil
		}
		s.pushFinal(text, nil)
		return nil
	case StreamEventError:
		if event.Err != nil {
			return event.Err
		}
		return errors.New("openai stream error")
	default:
		return nil
	}
}

// isHallucination reports whether text exactly matches (case-insensitive,
// already trimmed by caller) one of the configured stock phrases that
// Whisper and Gemma-FLM emit on silence or near-silent audio.
func (s *DictationSession) isHallucination(text string) bool {
	if len(s.hallucinationFilters) == 0 {
		return false
	}
	return s.hallucinationFilters[strings.ToLower(text)]
}

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
// Finalization helpers
// ---------------------------------------------------------------------------

// drainPendingSegments collects segment results that may have arrived between
// the recording stopping and Finalize being called.
func (s *DictationSession) drainPendingSegments(ctx context.Context, stream *Stream) (string, error) {
	// Skip the wait when nothing is in flight: no Whisper turn pending
	// AND no in-flight partial. Saves ~250ms on the common single-turn
	// dictation case where VAD never fired before commit.
	if !stream.HasPendingTranscription() && strings.TrimSpace(stream.Partial()) == "" {
		return "", nil
	}
	timer := time.NewTimer(250 * time.Millisecond)
	defer timer.Stop()

	var text string
	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case <-timer.C:
			return text, nil
		case result := <-s.finals:
			if result.err != nil {
				return "", result.err
			}
			if strings.TrimSpace(result.text) == "" {
				continue
			}
			text = appendSegmentText(text, result.text)
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(250 * time.Millisecond)
			if !s.hasTrailing.Load() && strings.TrimSpace(stream.Partial()) == "" {
				return text, nil
			}
		}
	}
}

// waitForFinal waits for the trailing transcript with a proportional timeout.
// Three signals feed the timeout budget:
//   - trailingDuration() / 5 — wall-clock since stream ready, useful for PTT
//     where the user has been speaking for a while.
//   - expectedAudioMS * 10 — when the caller knows the total audio it is
//     feeding (batch recall), scales the budget to accommodate a local
//     model that can run at 1-5x realtime. 10x headroom keeps even a
//     very slow backend from tripping the cap unnecessarily — user still
//     has Ctrl-C (propagated via ctx) as the real abort path.
//   - wait_final_seconds floor — minimum, for cold-start cases.
// Whichever signal produces the largest value wins.
func (s *DictationSession) waitForFinal(ctx context.Context) (string, error) {
	timeout := s.trailingDuration() / 5

	if s.expectedAudioMS > 0 {
		audioBased := time.Duration(s.expectedAudioMS) * time.Millisecond * 10
		if audioBased > timeout {
			timeout = audioBased
		}
	}

	floor := time.Duration(s.waitFinalFloorSeconds) * time.Second
	if floor <= 0 {
		floor = 3 * time.Second
	}
	if timeout < floor {
		timeout = floor
	}

	sessionlog.Debugf("wait_final: timeout=%s (trailing=%s, expected_audio=%dms, floor=%s)",
		timeout.Round(100*time.Millisecond),
		s.trailingDuration().Round(100*time.Millisecond),
		s.expectedAudioMS, floor)

	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	select {
	case <-waitCtx.Done():
		return "", waitCtx.Err()
	case result := <-s.finals:
		if result.err != nil {
			return "", result.err
		}
		return strings.TrimSpace(result.text), nil
	}
}

// canSkipEmptyCommit returns true when a commit-empty error is expected
// because all audio was consumed by live segments.
func (s *DictationSession) canSkipEmptyCommit(err error, stream *Stream) bool {
	return errors.Is(err, ErrInputAudioBufferCommitEmpty) &&
		int(s.segmentCount.Load()) > 0 &&
		strings.TrimSpace(stream.Partial()) == ""
}

// canSkipTrailingTimeout returns true when a trailing timeout is acceptable
// because we already have segment text and no partial is in flight.
func (s *DictationSession) canSkipTrailingTimeout(err error, stream *Stream) bool {
	return errors.Is(err, context.DeadlineExceeded) &&
		int(s.segmentCount.Load()) > 0 &&
		strings.TrimSpace(stream.Partial()) == ""
}

// ---------------------------------------------------------------------------
// Channel helpers
// ---------------------------------------------------------------------------

func (s *DictationSession) emitPartial(text string) {
	select {
	case s.events <- DictationEvent{Type: DictationEventPartial, Text: text}:
	default:
	}
}

func (s *DictationSession) emitEvent(event DictationEvent) {
	select {
	case s.events <- event:
	default:
	}
}

func (s *DictationSession) pushFinal(text string, err error) {
	select {
	case s.finals <- finalResult{text: text, err: err}:
	default:
	}
}

func (s *DictationSession) finishPump(err error) {
	select {
	case s.pumpDone <- err:
	default:
	}
}

// ---------------------------------------------------------------------------
// State helpers — wrap the multi-step atomic patterns. Single-op accesses
// (Load/Store on the atomic fields) happen inline at the call site.
// ---------------------------------------------------------------------------

// setStream stamps streamReadyAt and publishes the stream pointer.
// Pump goroutine only.
func (s *DictationSession) setStream(stream *Stream) {
	s.streamReadyAt.Store(time.Now().UnixNano())
	s.stream.Store(stream)
}

// closeStream atomically swaps the stream pointer to nil and closes it.
// Safe to call concurrently / multiple times.
func (s *DictationSession) closeStream() {
	if stream := s.stream.Swap(nil); stream != nil {
		_ = stream.Close()
	}
}

// trailingDuration returns time since the last segment arrived, falling
// back to the stream-ready timestamp if no segment has come yet. Reads
// two atomics independently — they're updated by different goroutines
// and we don't need joint consistency, just "use whichever is fresher".
func (s *DictationSession) trailingDuration() time.Duration {
	ref := s.lastSegmentAt.Load()
	if ref == 0 {
		ref = s.streamReadyAt.Load()
	}
	if ref == 0 {
		return 0
	}
	return time.Since(time.Unix(0, ref))
}

// formatSegmentText increments the segment counter and adds a leading
// space when the new segment needs separation from the running text.
// Atomic.Add returns the new count, so first-segment is `n == 1`.
func (s *DictationSession) formatSegmentText(text string) string {
	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	if s.segmentCount.Add(1) == 1 {
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

// ---------------------------------------------------------------------------
// Text utilities
// ---------------------------------------------------------------------------

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

// dumpWSFrame traces a single WebSocket frame to the session log.
//
// Outbound `input_audio_buffer.append` frames are skipped (one every
// ~50ms, kilobytes of base64 each — drowns the interesting protocol
// traffic). Counts/bytes are still captured in span attrs. Skip is
// direction-gated so no inbound event can ever be filtered out
// accidentally by a substring collision: the invariant we want is
// "every frame the server sends us is visible in the trace log."
//
// Always on so the raw protocol transcript is available after the fact
// to diagnose backend-specific event shape mismatches (e.g. a backend
// that ships transcription deltas under a different field name than the
// OpenAI-realtime spec).
func dumpWSFrame(direction string, data []byte) {
	if direction == "→" && strings.Contains(string(data), `"type":"input_audio_buffer.append"`) {
		return
	}
	sessionlog.Tracef("ws %s %s", direction, data)
}

// ---------------------------------------------------------------------------
// Stream read loop
// ---------------------------------------------------------------------------

func (s *Stream) readLoop() {
	defer close(s.readDone)
	defer close(s.events)

	for {
		_, data, err := s.conn.ReadMessage()
		if err != nil {
			s.markReady(err)
			s.emit(StreamEvent{Type: StreamEventError, Err: err})
			return
		}
		dumpWSFrame("←", data)
		var raw jsonMessage
		if err := json.Unmarshal(data, &raw); err != nil {
			s.markReady(err)
			s.emit(StreamEvent{Type: StreamEventError, Err: err})
			return
		}

		msgType := raw.Type()
		s.recordInbound(raw)
		switch msgType {
		case "session.created", "session.updated":
			// Log the full payload for session.updated so we can see
			// whether our turn_detection:null took effect — Lemonade
			// echoes the effective session config back here, and a
			// mismatch vs what we sent is the smoking gun when server
			// VAD fires despite manual_commit mode.
			if msgType == "session.updated" {
				sessionlog.Debugf("realtime: session.updated payload=%s", string(data))
			} else {
				sessionlog.Debugf("realtime: %s", msgType)
			}
			s.markReady(nil)
		case "input_audio_buffer.speech_started",
			"input_audio_buffer.speech_stopped",
			"input_audio_buffer.committed",
			"input_audio_buffer.cleared":
			sessionlog.Debugf("realtime: %s", msgType)
		case "conversation.item.input_audio_transcription.delta":
			var event transcriptionDeltaEvent
			if err := raw.decode(&event); err != nil {
				sessionlog.Warnf("realtime: delta decode failed: %v payload=%s", err, truncate(string(data), 200))
			} else if event.Delta == "" {
				// Backend emitted a delta frame but the text is empty under
				// the spec-standard `delta` key. Most likely the backend
				// uses a non-standard field name — log the full payload so
				// we can tell what to map.
				sessionlog.Warnf("realtime: delta event has empty text under 'delta' key — backend field mismatch? payload=%s", truncate(string(data), 400))
			} else {
				partial := s.appendPartial(event.ItemID, event.Delta)
				s.emitPartial(StreamEvent{Type: StreamEventPartial, Text: partial})
			}
		case "conversation.item.input_audio_transcription.completed":
			var event transcriptionCompletedEvent
			if err := raw.decode(&event); err != nil {
				s.emit(StreamEvent{Type: StreamEventError, Err: err})
				return
			}
			final, clearPartial := s.completePartial(event.ItemID, event.Transcript)
			if clearPartial {
				s.emitPartial(StreamEvent{Type: StreamEventPartial, Text: ""})
			}
			s.emit(StreamEvent{Type: StreamEventFinal, Text: final})
		case "conversation.item.input_audio_transcription.failed":
			var event transcriptionFailedEvent
			if err := raw.decode(&event); err != nil {
				s.emit(StreamEvent{Type: StreamEventError, Err: err})
				return
			}
			s.emit(StreamEvent{
				Type: StreamEventError,
				Err:  formatRealtimeError(event.Error.Code, event.Error.Message),
			})
			return
		case "error":
			var event realtimeErrorEvent
			if err := raw.decode(&event); err != nil {
				s.emit(StreamEvent{Type: StreamEventError, Err: err})
				return
			}
			err := formatRealtimeError(event.Error.Code, event.Error.Message)
			s.markReady(err)
			s.emit(StreamEvent{Type: StreamEventError, Err: err})
			return
		default:
			// Surface anything the backend emits that we don't handle. Catches
			// backend-specific event names (e.g. Lemonade variants) so they
			// show up in the session log instead of being silently dropped.
			sessionlog.Debugf("realtime: unhandled event type=%q", msgType)
		}
	}
}

func (s *Stream) markReady(err error) {
	s.readyOnce.Do(func() {
		s.readyCh <- err
	})
}

func (s *Stream) emit(event StreamEvent) {
	select {
	case s.events <- event:
	default:
		sessionlog.Warnf("stream event dropped type=%s (channel full)", event.Type)
	}
}

func (s *Stream) emitPartial(event StreamEvent) {
	select {
	case s.events <- event:
	default:
	}
}

func (s *Stream) appendPartial(itemID, delta string) string {
	s.partialMu.Lock()
	defer s.partialMu.Unlock()

	merge := s.mergeDelta
	if merge == nil {
		merge = mergeIncrementalDelta
	}

	if itemID == "" {
		s.partial = merge(s.partial, delta)
		return s.partial
	}
	if s.partials == nil {
		s.partials = make(map[string]string)
	}
	s.partials[itemID] = merge(s.partials[itemID], delta)
	s.activeID = itemID
	s.partial = s.partials[itemID]
	return s.partial
}

func (s *Stream) completePartial(itemID, transcript string) (string, bool) {
	s.partialMu.Lock()
	defer s.partialMu.Unlock()

	tracked := s.partial
	clearPartial := true
	if itemID != "" {
		if s.partials != nil {
			if value, ok := s.partials[itemID]; ok {
				tracked = value
				delete(s.partials, itemID)
			}
		}
		clearPartial = s.activeID == itemID || s.activeID == ""
		if clearPartial {
			s.activeID = ""
			s.partial = ""
		}
	} else {
		s.activeID = ""
		s.partial = ""
	}

	return reconcileCompletedTranscript(tracked, transcript), clearPartial
}

func reconcileCompletedTranscript(partial, transcript string) string {
	partial = strings.TrimSpace(partial)
	transcript = strings.TrimSpace(transcript)

	switch {
	case partial == "":
		return transcript
	case transcript == "":
		return partial
	}

	if normalizedHasSuffix(partial, transcript) && len([]rune(partial)) > len([]rune(transcript)) {
		return partial
	}
	return transcript
}

func normalizedHasSuffix(full, suffix string) bool {
	full = normalizeText(full)
	suffix = normalizeText(suffix)
	if full == "" || suffix == "" {
		return false
	}
	return strings.HasSuffix(full, suffix)
}

func normalizeText(text string) string {
	return strings.Join(strings.Fields(strings.ToLower(text)), " ")
}

// ---------------------------------------------------------------------------
// OpenAI protocol helpers
// ---------------------------------------------------------------------------

type realtimeEvent struct {
	Type string `json:"type"`
}

type realtimeReadyEvent struct {
	Type string `json:"type"`
}

type transcriptionDeltaEvent struct {
	Type   string `json:"type"`
	ItemID string `json:"item_id"`
	Delta  string `json:"delta"`
}

type transcriptionCompletedEvent struct {
	Type       string `json:"type"`
	ItemID     string `json:"item_id"`
	Transcript string `json:"transcript"`
}

type transcriptionFailedEvent struct {
	Type  string `json:"type"`
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
		Param   string `json:"param"`
		Type    string `json:"type"`
	} `json:"error"`
}

type realtimeErrorEvent struct {
	Type  string `json:"type"`
	Error struct {
		Code    string `json:"code"`
		Message string `json:"message"`
		EventID string `json:"event_id"`
		Type    string `json:"type"`
	} `json:"error"`
}

type jsonMessage map[string]any

func (m jsonMessage) Type() string {
	value, _ := m["type"].(string)
	return value
}

func (m jsonMessage) decode(dst any) error {
	data, err := json.Marshal(m)
	if err != nil {
		return err
	}
	return json.Unmarshal(data, dst)
}

// ---------------------------------------------------------------------------
// PCM encoder — resamples and downmixes mono PCM16 to a target sample rate.
// ---------------------------------------------------------------------------

type pcmEncoder struct {
	inputRate  int64
	outputRate int64
	channels   int
	accum      int64
}

func newPCMEncoder(inputSampleRate, outputSampleRate, channels int) *pcmEncoder {
	return &pcmEncoder{
		inputRate:  int64(inputSampleRate),
		outputRate: int64(outputSampleRate),
		channels:   channels,
	}
}

func (e *pcmEncoder) Encode(samples []int16) []byte {
	if e == nil || e.inputRate <= 0 || e.outputRate <= 0 || e.channels <= 0 {
		return nil
	}

	frames := len(samples) / e.channels
	if frames == 0 {
		return nil
	}

	estimate := int((int64(frames)*e.outputRate)/e.inputRate + 2)
	out := make([]byte, 0, estimate*2)

	for frame := 0; frame < frames; frame++ {
		sample := mixToMono(samples[frame*e.channels : frame*e.channels+e.channels])
		e.accum += e.outputRate
		for e.accum >= e.inputRate {
			out = binary.LittleEndian.AppendUint16(out, uint16(sample))
			e.accum -= e.inputRate
		}
	}

	return out
}

func mixToMono(frame []int16) int16 {
	if len(frame) == 0 {
		return 0
	}
	if len(frame) == 1 {
		return frame[0]
	}
	var total int
	for _, sample := range frame {
		total += int(sample)
	}
	return int16(total / len(frame))
}

// ---------------------------------------------------------------------------
// Misc helpers
// ---------------------------------------------------------------------------

func formatDialError(err error, resp *http.Response) error {
	if resp == nil {
		return err
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	message := strings.TrimSpace(string(body))
	requestID := strings.TrimSpace(resp.Header.Get("x-request-id"))
	if message == "" {
		message = http.StatusText(resp.StatusCode)
	}
	return fmt.Errorf("openai realtime connect: status %d%s: %s",
		resp.StatusCode,
		requestIDSuffix(requestID),
		message,
	)
}

func formatRealtimeError(code, message string) error {
	message = strings.TrimSpace(message)
	if message == "" {
		message = "realtime transcription failed"
	}
	code = strings.TrimSpace(code)
	if code == "" {
		return errors.New(message)
	}
	if code == "input_audio_buffer_commit_empty" {
		return fmt.Errorf("%w: %s", ErrInputAudioBufferCommitEmpty, message)
	}
	return fmt.Errorf("%s (%s)", message, code)
}

func organization(cfg config.TranscriptionConfig) string {
	if value := strings.TrimSpace(cfg.Organization); value != "" {
		return value
	}
	if value := strings.TrimSpace(os.Getenv("OPENAI_ORGANIZATION")); value != "" {
		return value
	}
	return strings.TrimSpace(os.Getenv("OPENAI_ORG_ID"))
}

func project(cfg config.TranscriptionConfig) string {
	if value := strings.TrimSpace(cfg.Project); value != "" {
		return value
	}
	if value := strings.TrimSpace(os.Getenv("OPENAI_PROJECT")); value != "" {
		return value
	}
	return strings.TrimSpace(os.Getenv("OPENAI_PROJECT_ID"))
}

func requestIDSuffix(requestID string) string {
	if requestID == "" {
		return ""
	}
	return fmt.Sprintf(" (request id %s)", requestID)
}

func requestID(err *openaisdk.Error) string {
	if err == nil || err.Response == nil {
		return ""
	}
	return strings.TrimSpace(err.Response.Header.Get("x-request-id"))
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
