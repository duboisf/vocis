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
}

func (c *Client) StartStream(ctx context.Context, sampleRate, channels int) (*Stream, error) {
	if sampleRate <= 0 {
		return nil, errors.New("recording.sample_rate must be greater than zero")
	}
	if channels <= 0 {
		return nil, errors.New("recording.channels must be greater than zero")
	}

	connectCtx, connectCancel := context.WithTimeout(ctx, connectTimeout)
	defer connectCancel()

	ctx, connectSpan := telemetry.StartSpan(connectCtx, "vocis.transcribe.connect",
		attribute.String("transcribe.backend", string(c.backendName())),
		attribute.String("transcribe.model", c.cfg.Model),
		attribute.Int("audio.sample_rate", sampleRate),
		attribute.Int("audio.channels", channels),
	)

	conn, err := c.transport.Dial(ctx)
	if err != nil {
		telemetry.EndSpan(connectSpan, err)
		return nil, err
	}

	stream := &Stream{
		conn:         conn,
		encoder:      newPCMEncoder(sampleRate, c.transport.SampleRate(), channels),
		writeTimeout: c.writeTimeout,
		readyCh:      make(chan error, 1),
		events:       make(chan StreamEvent, 16),
		readDone:     make(chan struct{}),
	}
	go stream.readLoop()

	if err := stream.sendJSON(ctx, c.transport.SessionUpdate()); err != nil {
		stream.Close()
		telemetry.EndSpan(connectSpan, err)
		return nil, err
	}

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
	return s.sendJSON(ctx, map[string]any{
		"type":  "input_audio_buffer.append",
		"audio": base64.StdEncoding.EncodeToString(payload),
	})
}

func (s *Stream) Commit(ctx context.Context) error {
	return s.sendJSON(ctx, map[string]any{"type": "input_audio_buffer.commit"})
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
	})
	return err
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
	s.writeMu.Lock()
	defer s.writeMu.Unlock()
	if err := s.conn.SetWriteDeadline(deadline); err != nil {
		return err
	}
	return s.conn.WriteJSON(payload)
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
	writeTimeout time.Duration
	cancel       context.CancelFunc
	callbacks    ConnectCallbacks

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
		writeTimeout: c.writeTimeout,
		cancel:       cancel,
		callbacks:    callbacks,
		events:       make(chan DictationEvent, 16),
		pumpDone:     make(chan error, 1),
		finals:       make(chan finalResult, 8),
		liveSegments: true,
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
	_, commitSpan := telemetry.StartSpan(ctx, "vocis.transcribe.commit")
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
func (s *DictationSession) waitForFinal(ctx context.Context) (string, error) {
	timeout := s.trailingDuration() / 5
	if timeout < 3*time.Second {
		timeout = 3 * time.Second
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

// ---------------------------------------------------------------------------
// Stream read loop
// ---------------------------------------------------------------------------

func (s *Stream) readLoop() {
	defer close(s.readDone)
	defer close(s.events)

	for {
		var raw jsonMessage
		if err := s.conn.ReadJSON(&raw); err != nil {
			s.markReady(err)
			s.emit(StreamEvent{Type: StreamEventError, Err: err})
			return
		}

		switch raw.Type() {
		case "session.created", "session.updated":
			s.markReady(nil)
		case "input_audio_buffer.speech_started",
			"input_audio_buffer.speech_stopped",
			"input_audio_buffer.committed",
			"input_audio_buffer.cleared":
			sessionlog.Debugf("realtime: %s", raw.Type())
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
