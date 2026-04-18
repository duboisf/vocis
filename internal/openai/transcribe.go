package openai

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
	cfg          config.OpenAIConfig
	streaming    config.StreamingConfig
	client       openaisdk.Client
	chatStreamer chatCompletionStreamer
	transport    Transport
	writeTimeout time.Duration
}

func New(apiKey string, cfg config.OpenAIConfig, streaming config.StreamingConfig) *Client {
	timeout := time.Duration(cfg.RequestLimit) * time.Second

	baseURL := strings.TrimRight(cfg.BaseURL, "/")
	if baseURL == "" {
		baseURL = defaultBaseURL
	}
	if cfg.Backend != config.BackendLemonade && baseURL != defaultBaseURL {
		sessionlog.Warnf("openai base_url is non-default: %s", baseURL)
	}

	opts := []option.RequestOption{
		option.WithAPIKey(apiKey),
		option.WithBaseURL(baseURL),
		option.WithRequestTimeout(timeout),
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

	// statsMu protects all stats fields below. They feed:
	//   - the session span's closing attributes (counts/totals)
	//   - PreCommitContext() which the commit span queries
	//   - PostCommitStats() which the wait_final span queries
	statsMu              sync.Mutex
	inboundCounts        map[string]int
	outboundCounts       map[string]int
	appendBytes          int64
	lastInboundType      string
	lastInboundAt        time.Time
	firstSpeechStartedAt time.Time
	lastSpeechStoppedAt  time.Time
	speechStartedCount   int
	speechStoppedCount   int
	commitAt             time.Time
	firstPostCommitType  string
	firstPostCommitAt    time.Time
	firstPostCommitDelta string
	deltaCountPostCommit int
	completedText        string
	completedAt          time.Time
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
		conn:           conn,
		encoder:        newPCMEncoder(sampleRate, c.transport.SampleRate(), channels),
		writeTimeout:   c.writeTimeout,
		sessionSpan:    sessionSpan,
		sessionStart:   time.Now(),
		targetRate:     c.transport.SampleRate(),
		readyCh:        make(chan error, 1),
		events:         make(chan StreamEvent, 16),
		readDone:       make(chan struct{}),
		inboundCounts:  make(map[string]int),
		outboundCounts: make(map[string]int),
	}
	go stream.readLoop()

	if err := stream.sendJSON(connectCtx, c.transport.SessionUpdate()); err != nil {
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
	s.appendBytes += int64(len(payload))
	s.outboundCounts["input_audio_buffer.append"]++
	s.statsMu.Unlock()
	return nil
}

func (s *Stream) Commit(ctx context.Context) error {
	err := s.sendJSON(ctx, map[string]any{"type": "input_audio_buffer.commit"})
	if err == nil {
		s.statsMu.Lock()
		s.commitAt = time.Now()
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
			s.sessionSpan = nil
		}
	})
	return err
}

// recordInbound attaches a span event for a realtime message received from
// the backend AND updates the in-stream stats that PreCommitContext /
// PostCommitStats expose to commit / wait_final spans.
//
// Span event attrs:
//   - message.type
//   - elapsed (since session start)
//   - item_id (when present — lets you correlate which transcription turn)
//   - delta.text / delta.text_len (for delta events; text truncated to 80 chars)
//   - transcript.text / transcript.text_len (for completed events)
//   - audio_start_ms / audio_end_ms (for speech_started / speech_stopped — Lemonade's VAD timestamps)
func (s *Stream) recordInbound(msg jsonMessage) {
	now := time.Now()
	msgType := msg.Type()

	attrs := []attribute.KeyValue{
		attribute.String("message.type", msgType),
		attribute.String("elapsed", now.Sub(s.sessionStart).Round(time.Millisecond).String()),
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

	if s.sessionSpan != nil {
		s.sessionSpan.AddEvent("realtime.inbound", trace.WithAttributes(attrs...))
	}

	s.statsMu.Lock()
	s.inboundCounts[msgType]++
	s.lastInboundType = msgType
	s.lastInboundAt = now
	switch msgType {
	case "input_audio_buffer.speech_started":
		s.speechStartedCount++
		if s.firstSpeechStartedAt.IsZero() {
			s.firstSpeechStartedAt = now
		}
	case "input_audio_buffer.speech_stopped":
		s.speechStoppedCount++
		s.lastSpeechStoppedAt = now
	case "conversation.item.input_audio_transcription.delta":
		if !s.commitAt.IsZero() && now.After(s.commitAt) {
			s.deltaCountPostCommit++
			if s.firstPostCommitType == "" {
				s.firstPostCommitType = "delta"
				s.firstPostCommitAt = now
			}
			if d, ok := msg["delta"].(string); ok && d != "" && s.firstPostCommitDelta == "" {
				s.firstPostCommitDelta = d
			}
		}
	case "conversation.item.input_audio_transcription.completed":
		if !s.commitAt.IsZero() && s.firstPostCommitType == "" {
			s.firstPostCommitType = "completed"
			s.firstPostCommitAt = now
		}
		if t, ok := msg["transcript"].(string); ok {
			s.completedText = t
		}
		s.completedAt = now
	}
	s.statsMu.Unlock()
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
	s.outboundCounts[msgType]++
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
		LastInboundType: s.lastInboundType,
		AppendBytes:     s.appendBytes,
		// 16-bit PCM mono → 2 bytes per sample. Total samples = bytes/2.
		// Audio ms = samples * 1000 / sample_rate.
		AppendApproxMs: 0,
	}
	if s.targetRate > 0 {
		ctx.AppendApproxMs = (s.appendBytes / 2) * 1000 / int64(s.targetRate)
	}
	if !s.lastInboundAt.IsZero() {
		ctx.MsSinceLastInbound = now.Sub(s.lastInboundAt).Milliseconds()
	}
	if !s.lastSpeechStoppedAt.IsZero() {
		ctx.SpeechStoppedSeenBefore = true
		ctx.MsSinceSpeechStopped = now.Sub(s.lastSpeechStoppedAt).Milliseconds()
	}
	return ctx
}

// PostCommitStats returns the post-commit timeline. Call after waitForFinal
// returns (success, timeout, or error). delta_eq_completed answers the
// "could we skip waiting for completed?" question directly.
func (s *Stream) PostCommitStats() PostCommitStats {
	s.statsMu.Lock()
	defer s.statsMu.Unlock()
	stats := PostCommitStats{
		FirstEventType:    s.firstPostCommitType,
		FirstDeltaText:    truncate(s.firstPostCommitDelta, 80),
		FirstDeltaTextLen: len(s.firstPostCommitDelta),
		CompletedText:     truncate(s.completedText, 80),
		CompletedTextLen:  len(s.completedText),
		DeltaCount:        s.deltaCountPostCommit,
	}
	if s.firstPostCommitDelta != "" && s.completedText != "" {
		stats.DeltaEqCompleted = strings.TrimSpace(s.firstPostCommitDelta) == strings.TrimSpace(s.completedText)
	}
	if !s.commitAt.IsZero() {
		if !s.firstPostCommitAt.IsZero() {
			stats.FirstEventMs = s.firstPostCommitAt.Sub(s.commitAt).Milliseconds()
		}
		if !s.completedAt.IsZero() {
			stats.CompletedMs = s.completedAt.Sub(s.commitAt).Milliseconds()
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
		attribute.Int("inbound.deltas_count", s.inboundCounts["conversation.item.input_audio_transcription.delta"]),
		attribute.Int("inbound.completed_count", s.inboundCounts["conversation.item.input_audio_transcription.completed"]),
		attribute.Int("inbound.speech_started_count", s.speechStartedCount),
		attribute.Int("inbound.speech_stopped_count", s.speechStoppedCount),
		attribute.Int("inbound.committed_count", s.inboundCounts["input_audio_buffer.committed"]),
		attribute.Int("outbound.append_count", s.outboundCounts["input_audio_buffer.append"]),
		attribute.Int64("outbound.append_bytes", s.appendBytes),
	}
	if s.targetRate > 0 {
		attrs = append(attrs, attribute.Int64("outbound.append_total_audio_ms", (s.appendBytes/2)*1000/int64(s.targetRate)))
	}
	if !s.firstSpeechStartedAt.IsZero() {
		attrs = append(attrs, attribute.Int64("session.first_speech_started_at_ms", s.firstSpeechStartedAt.Sub(s.sessionStart).Milliseconds()))
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

type DictationSession struct {
	writeTimeout          time.Duration
	waitFinalFloorSeconds int
	cancel                context.CancelFunc
	callbacks             ConnectCallbacks

	events   chan DictationEvent
	pumpDone chan error
	finals   chan finalResult

	mu            sync.Mutex
	stream        *Stream
	liveSegments  bool
	hasTrailing   bool
	segmentCount  int
	streamReadyAt time.Time
	lastSegmentAt time.Time
}

type finalResult struct {
	text string
	err  error
}

func (c *Client) StartDictation(
	ctx context.Context,
	sampleRate, channels int,
	samples <-chan []int16,
	callbacks ConnectCallbacks,
) (*DictationSession, error) {
	if sampleRate <= 0 {
		return nil, errors.New("recording.sample_rate must be greater than zero")
	}
	if channels <= 0 {
		return nil, errors.New("recording.channels must be greater than zero")
	}

	pumpCtx, cancel := context.WithCancel(ctx)
	session := &DictationSession{
		writeTimeout:          c.writeTimeout,
		waitFinalFloorSeconds: c.streaming.WaitFinalSeconds,
		cancel:                cancel,
		callbacks:             callbacks,
		events:                make(chan DictationEvent, 16),
		pumpDone:              make(chan error, 1),
		finals:                make(chan finalResult, 8),
		liveSegments:          true,
	}
	go session.run(pumpCtx, c, sampleRate, channels, samples)
	return session, nil
}

func (s *DictationSession) Events() <-chan DictationEvent { return s.events }

// Finalize waits for the audio pump to finish, then collects any trailing
// transcript that arrived after the last live segment.
func (s *DictationSession) Finalize(ctx context.Context) (FinalizeResult, error) {
	s.setLiveSegments(false)

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

	stream := s.currentStream()
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
	if !s.hasTrailingAudio() && strings.TrimSpace(stream.Partial()) == "" {
		return FinalizeResult{Text: text}, nil
	}

	// Phase 2: commit trailing audio and wait for the final transcript.
	pre := stream.PreCommitContext()
	_, commitSpan := telemetry.StartSpan(ctx, "vocis.transcribe.commit",
		attribute.Int64("commit.pending_audio_bytes", pre.AppendBytes),
		attribute.Int64("commit.pending_audio_ms", pre.AppendApproxMs),
		attribute.String("commit.last_inbound_type", pre.LastInboundType),
		attribute.Int64("commit.ms_since_last_inbound", pre.MsSinceLastInbound),
		attribute.Bool("commit.speech_stopped_seen_before_commit", pre.SpeechStoppedSeenBefore),
		attribute.Int64("commit.ms_since_speech_stopped", pre.MsSinceSpeechStopped),
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

	_, waitSpan := telemetry.StartSpan(ctx, "vocis.transcribe.wait_final",
		attribute.String("trailing_duration", s.trailingDuration().Round(10*time.Millisecond).String()),
		attribute.Int("segment_count", s.segmentCountValue()),
	)
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

	// Flush buffered audio that arrived while connecting.
	for _, chunk := range buffered {
		if err := stream.Append(ctx, chunk); err != nil {
			s.finishPump(err)
			return
		}
		s.markAudioAppended()
	}

	// Stream live audio until the samples channel closes or context cancels.
	s.streamAudio(ctx, stream, samples)
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
func (s *DictationSession) streamAudio(ctx context.Context, stream *Stream, samples <-chan []int16) {
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
			s.markAudioAppended()
		}
	}
}

// ---------------------------------------------------------------------------
// Stream event handling
// ---------------------------------------------------------------------------

func (s *DictationSession) consumeStreamEvents(ctx context.Context) {
	stream := s.currentStream()
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
		s.clearTrailingFlag()
		s.markSegmentReceived()
		text = s.formatSegmentText(text)
		if s.liveSegmentsEnabled() {
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

// ---------------------------------------------------------------------------
// Finalization helpers
// ---------------------------------------------------------------------------

// drainPendingSegments collects segment results that may have arrived between
// the recording stopping and Finalize being called.
func (s *DictationSession) drainPendingSegments(ctx context.Context, stream *Stream) (string, error) {
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
			if !s.hasTrailingAudio() && strings.TrimSpace(stream.Partial()) == "" {
				return text, nil
			}
		}
	}
}

// waitForFinal waits for the trailing transcript with a proportional timeout.
// The floor is streaming.wait_final_seconds (default 3); raise for local
// backends where Whisper model load + CPU inference can run 5-15s on the
// first request.
func (s *DictationSession) waitForFinal(ctx context.Context) (string, error) {
	timeout := s.trailingDuration() / 5
	floor := time.Duration(s.waitFinalFloorSeconds) * time.Second
	if floor <= 0 {
		floor = 3 * time.Second
	}
	if timeout < floor {
		timeout = floor
	}
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
		s.segmentCountValue() > 0 &&
		strings.TrimSpace(stream.Partial()) == ""
}

// canSkipTrailingTimeout returns true when a trailing timeout is acceptable
// because we already have segment text and no partial is in flight.
func (s *DictationSession) canSkipTrailingTimeout(err error, stream *Stream) bool {
	return errors.Is(err, context.DeadlineExceeded) &&
		s.segmentCountValue() > 0 &&
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
// State accessors (mutex-protected)
// ---------------------------------------------------------------------------

func (s *DictationSession) currentStream() *Stream {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.stream
}

func (s *DictationSession) setStream(stream *Stream) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.stream = stream
	s.streamReadyAt = time.Now()
}

func (s *DictationSession) closeStream() {
	s.mu.Lock()
	stream := s.stream
	s.stream = nil
	s.mu.Unlock()
	if stream != nil {
		_ = stream.Close()
	}
}

func (s *DictationSession) liveSegmentsEnabled() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.liveSegments
}

func (s *DictationSession) setLiveSegments(enabled bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.liveSegments = enabled
}

func (s *DictationSession) markAudioAppended() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hasTrailing = true
}

func (s *DictationSession) clearTrailingFlag() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.hasTrailing = false
}

func (s *DictationSession) hasTrailingAudio() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.hasTrailing
}

func (s *DictationSession) segmentCountValue() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.segmentCount
}

func (s *DictationSession) markSegmentReceived() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lastSegmentAt = time.Now()
}

func (s *DictationSession) trailingDuration() time.Duration {
	s.mu.Lock()
	defer s.mu.Unlock()
	ref := s.lastSegmentAt
	if ref.IsZero() {
		ref = s.streamReadyAt
	}
	if ref.IsZero() {
		return 0
	}
	return time.Since(ref)
}

func (s *DictationSession) formatSegmentText(text string) string {
	s.mu.Lock()
	defer s.mu.Unlock()

	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	if s.segmentCount == 0 {
		s.segmentCount++
		return text
	}
	s.segmentCount++
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

// dumpWSFrame writes a single WebSocket frame to stderr when
// VOCIS_WS_DUMP=1. Skips `input_audio_buffer.append` entirely (one frame
// per ~50ms, kilobytes of base64 each — drowns the interesting protocol
// traffic). Audio append counts/bytes are still captured in span attrs.
//
// Sized at one stderr line per frame so it can be `tee`'d for protocol
// debugging without parsing.
func dumpWSFrame(direction string, data []byte) {
	if os.Getenv("VOCIS_WS_DUMP") != "1" {
		return
	}
	if strings.Contains(string(data), `"type":"input_audio_buffer.append"`) {
		return
	}
	ts := time.Now().Format("15:04:05.000")
	fmt.Fprintf(os.Stderr, "[ws %s %s] %s\n", ts, direction, data)
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
			sessionlog.Debugf("realtime: %s", msgType)
			s.markReady(nil)
		case "input_audio_buffer.speech_started",
			"input_audio_buffer.speech_stopped",
			"input_audio_buffer.committed",
			"input_audio_buffer.cleared":
			sessionlog.Debugf("realtime: %s", msgType)
		case "conversation.item.input_audio_transcription.delta":
			var event transcriptionDeltaEvent
			if err := raw.decode(&event); err == nil && event.Delta != "" {
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

	if itemID == "" {
		s.partial += delta
		return s.partial
	}
	if s.partials == nil {
		s.partials = make(map[string]string)
	}
	s.partials[itemID] += delta
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

func organization(cfg config.OpenAIConfig) string {
	if value := strings.TrimSpace(cfg.Organization); value != "" {
		return value
	}
	if value := strings.TrimSpace(os.Getenv("OPENAI_ORGANIZATION")); value != "" {
		return value
	}
	return strings.TrimSpace(os.Getenv("OPENAI_ORG_ID"))
}

func project(cfg config.OpenAIConfig) string {
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
