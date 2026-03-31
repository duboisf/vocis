package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"vtt/internal/config"
	"vtt/internal/hotkeys"
	"vtt/internal/injector"
	"vtt/internal/openai"
	"vtt/internal/overlay"
	"vtt/internal/recorder"
	"vtt/internal/securestore"
	"vtt/internal/sessionlog"
)

type App struct {
	cfg        config.Config
	overlay    overlayUI
	recorder   *recorder.Recorder
	injector   injectorClient
	transcribe *openai.Client
	store      *securestore.Store

	mu                         sync.Mutex
	recording                  *recordingState
	transcribing               bool
	dismissCompletionOverlay   bool
	lastToggle                 time.Time
	lastLiveInsert             time.Time
	sequence                   uint64
}

type recordingState struct {
	id        uint64
	startedAt time.Time
	session   *recorder.Session
	stream    *openai.Stream
	pumpDone  chan error
	results   chan streamResult
	cancel    context.CancelFunc
	target    injector.Target

	mu                  sync.Mutex
	liveSegmentDelivery bool
	deliveredSegments   int
	pendingFinalCommit  bool
}

type streamResult struct {
	text string
	err  error
}

type overlayUI interface {
	ShowHint(text string)
	ShowListening(windowClass string)
	SetListeningText(windowClass, text string)
	AnimateChunk(text string)
	ShowTranscribing()
	ShowSuccess(text string)
	ShowError(err error)
	SetLevel(level float64)
	Hide()
	Close()
}

type injectorClient interface {
	CaptureTarget(ctx context.Context) (injector.Target, error)
	Insert(ctx context.Context, target injector.Target, text string) error
	InsertLive(ctx context.Context, target injector.Target, text string) error
}

const minToggleInterval = 250 * time.Millisecond
const segmentDrainGrace = 250 * time.Millisecond
const syntheticReleaseGuard = 180 * time.Millisecond

func New(cfg config.Config) *App {
	return &App{
		cfg:   cfg,
		store: securestore.New(),
	}
}

func (a *App) Run(ctx context.Context) error {
	if err := a.cfg.Validate(); err != nil {
		return err
	}
	sessionlog.Infof("starting vtt session")
	recorder.CleanupStale()

	apiKey, err := a.store.APIKey()
	if err != nil {
		return err
	}

	a.recorder = recorder.New()
	a.injector = injector.New(a.cfg.Insertion)
	a.transcribe = openai.New(apiKey, a.cfg.OpenAI, a.cfg.Streaming)

	a.overlay, err = overlay.New(a.cfg.Overlay)
	if err != nil {
		return err
	}
	defer a.overlay.Close()

	hk, err := a.registerHotkeyWithFallback()
	if err != nil {
		return err
	}
	defer hk.Close()

	a.overlay.ShowHint(a.hotkeyHint(hk.Shortcut()))

	for {
		select {
		case <-ctx.Done():
			sessionlog.Infof("received shutdown signal")
			return a.shutdown()
		case <-hk.Down():
			a.handleDown(ctx)
		case <-hk.Up():
			a.handleUp(ctx)
		}
	}
}

func (a *App) handleDown(ctx context.Context) {
	if a.dismissInFlightOverlay() {
		return
	}
	if a.cfg.HotkeyMode == "toggle" {
		a.handleToggle(ctx)
		return
	}
	a.handleStart(ctx)
}

func (a *App) handleUp(ctx context.Context) {
	if a.cfg.HotkeyMode != "hold" {
		return
	}
	if a.shouldIgnoreSyntheticUp() {
		return
	}
	a.handleStop(ctx)
}

func (a *App) handleToggle(ctx context.Context) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if time.Since(a.lastToggle) < minToggleInterval {
		return
	}
	a.lastToggle = time.Now()

	if a.transcribing {
		return
	}

	if a.recording == nil {
		a.startRecordingLocked(ctx)
		return
	}

	a.stopRecordingLocked(ctx)
}

func (a *App) handleStart(ctx context.Context) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.transcribing || a.recording != nil {
		return
	}

	a.startRecordingLocked(ctx)
}

func (a *App) handleStop(ctx context.Context) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.transcribing || a.recording == nil {
		return
	}

	a.stopRecordingLocked(ctx)
}

func (a *App) startRecordingLocked(ctx context.Context) {
	a.dismissCompletionOverlay = false
	a.overlay.ShowListening("")

	target, err := a.injector.CaptureTarget(ctx)
	if err != nil {
		sessionlog.Errorf("capture target: %v", err)
		a.overlay.ShowError(err)
		return
	}
	if a.cfg.LogWindowTitle {
		sessionlog.Infof("starting recording for window=%s class=%s title=%q",
			target.WindowID, target.WindowClass, target.WindowName)
	} else {
		sessionlog.Infof("starting recording for window=%s class=%s",
			target.WindowID, target.WindowClass)
	}

	session, err := a.recorder.Start(ctx, a.cfg.Recording)
	if err != nil {
		sessionlog.Errorf("start recording: %v", err)
		a.overlay.ShowError(err)
		return
	}

	recordCtx, cancel := context.WithCancel(ctx)

	a.sequence++
	state := &recordingState{
		id:                  a.sequence,
		startedAt:           time.Now(),
		session:             session,
		pumpDone:            make(chan error, 1),
		results:             make(chan streamResult, 8),
		cancel:              cancel,
		target:              target,
		liveSegmentDelivery: a.cfg.Streaming.Mode == "segment",
	}
	a.recording = state
	a.overlay.ShowListening(target.WindowClass)
	sessionlog.Infof("recording started: %d Hz, %d channel(s), connecting realtime transcription",
		state.session.SampleRate(), state.session.Channels())
	go a.pumpAudio(recordCtx, state)
	go a.monitorRecordingLevel(ctx, state.id, state.session)

	if a.cfg.Recording.MaxDurationSeconds > 0 {
		go a.forceStopAfter(ctx, state.id, time.Duration(a.cfg.Recording.MaxDurationSeconds)*time.Second)
	}
}

func (a *App) stopRecordingLocked(ctx context.Context) {
	state := a.recording
	a.recording = nil
	state.setLiveSegmentDelivery(false)
	a.transcribing = true
	a.overlay.ShowTranscribing()
	sessionlog.Infof("stopping recording and finalizing transcription after %s",
		time.Since(state.startedAt).Round(10*time.Millisecond))

	go a.finishRecording(ctx, state)
}

func (a *App) registerHotkeyWithFallback() (*hotkeys.Registration, error) {
	candidates := []string{
		a.cfg.Hotkey,
		"ctrl+alt+space",
		"f8",
		"f9",
		"shift+f8",
	}

	var lastErr error
	seen := make(map[string]struct{}, len(candidates))
	for _, candidate := range candidates {
		if _, ok := seen[candidate]; ok {
			continue
		}
		seen[candidate] = struct{}{}

		hk, err := hotkeys.Register(candidate)
		if err == nil {
			if candidate != a.cfg.Hotkey {
				sessionlog.Warnf("hotkey %s unavailable, using %s", a.cfg.Hotkey, candidate)
			}
			return hk, nil
		}
		lastErr = err
	}

	if lastErr == nil {
		lastErr = errors.New("no hotkey candidates available")
	}
	return nil, lastErr
}

func (a *App) forceStopAfter(ctx context.Context, id uint64, maxDuration time.Duration) {
	timer := time.NewTimer(maxDuration)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return
	case <-timer.C:
	}

	a.mu.Lock()
	if a.recording == nil || a.recording.id != id || a.transcribing {
		a.mu.Unlock()
		return
	}
	state := a.recording
	a.recording = nil
	state.setLiveSegmentDelivery(false)
	a.transcribing = true
	a.mu.Unlock()

	a.overlay.ShowTranscribing()
	sessionlog.Warnf("auto-stopping recording after timeout")
	go a.finishRecording(ctx, state)
}

func (a *App) finishRecording(ctx context.Context, state *recordingState) {
	defer func() {
		a.mu.Lock()
		a.transcribing = false
		a.mu.Unlock()
	}()
	defer state.cancel()

	stopCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	if err := state.session.Stop(stopCtx); err != nil {
		if errors.Is(err, recorder.ErrRecordingTooShort) {
			sessionlog.Infof("discarding short recording after %s",
				state.session.Duration().Round(10*time.Millisecond))
			state.cancel()
			<-state.pumpDone
			if state.stream != nil {
				_ = state.stream.Close()
			}
			a.hideCompletionOverlay()
			return
		}
		sessionlog.Errorf("stop recording: %v", err)
		a.showCompletionError(err)
		state.cancel()
		<-state.pumpDone
		if state.stream != nil {
			_ = state.stream.Close()
		}
		return
	}
	if err := <-state.pumpDone; err != nil {
		sessionlog.Errorf("stream audio: %v", err)
		a.showCompletionError(err)
		if state.stream != nil {
			_ = state.stream.Close()
		}
		return
	}
	defer state.stream.Close()
	sessionlog.Infof("audio captured successfully: %d bytes streamed over %s",
		state.session.BytesCaptured(), state.session.Duration().Round(10*time.Millisecond))

	if err := a.drainStreamResults(ctx, state); err != nil {
		sessionlog.Errorf("stream transcription: %v", err)
		a.showCompletionError(err)
		return
	}

	if a.cfg.Streaming.Mode == "segment" {
		if err := a.waitForOptionalStreamResults(ctx, state, segmentDrainGrace); err != nil {
			sessionlog.Errorf("stream transcription: %v", err)
			a.showCompletionError(err)
			return
		}
		if !state.needsFinalCommit() && strings.TrimSpace(state.stream.Partial()) == "" {
			sessionlog.Infof("no trailing audio left to finalize after segmented insert")
			a.hideCompletionOverlay()
			return
		}
	}

	transcribeCtx, transcribeCancel := context.WithTimeout(
		ctx,
		time.Duration(a.cfg.OpenAI.RequestLimit)*time.Second,
	)
	defer transcribeCancel()

	if err := state.stream.Commit(transcribeCtx); err != nil {
		if a.shouldIgnoreEmptySegmentCommit(state, err) {
			sessionlog.Infof("no trailing audio left to finalize after segmented insert")
			a.hideCompletionOverlay()
			return
		}
		sessionlog.Errorf("transcribe audio: %v", err)
		a.showCompletionError(err)
		return
	}
	text, err := a.waitForStreamResult(transcribeCtx, state)
	if err != nil {
		if a.shouldIgnoreEmptySegmentCommit(state, err) {
			sessionlog.Infof("no trailing audio left to finalize after segmented insert")
			a.hideCompletionOverlay()
			return
		}
		sessionlog.Errorf("transcribe audio: %v", err)
		a.showCompletionError(err)
		return
	}
	sessionlog.Infof("transcription complete: %d characters", len(text))

	if text == "" {
		if a.cfg.Streaming.Mode == "segment" {
			a.hideCompletionOverlay()
			return
		}
		sessionlog.Warnf("transcription was empty")
		a.showCompletionError(errors.New("transcription came back empty"))
		return
	}

	if err := a.injector.Insert(ctx, state.target, text); err != nil {
		sessionlog.Errorf("insert transcript: %v", err)
		a.showCompletionError(err)
		return
	}
	sessionlog.Infof("transcript inserted into window=%s", state.target.WindowID)

	if a.cfg.Streaming.Mode == "segment" {
		a.hideCompletionOverlay()
		return
	}
	a.showCompletionSuccess(text)
}

func (a *App) monitorRecordingLevel(ctx context.Context, id uint64, session *recorder.Session) {
	ticker := time.NewTicker(65 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}

		a.mu.Lock()
		active := a.recording != nil && a.recording.id == id && !a.transcribing
		a.mu.Unlock()
		if !active {
			a.overlay.SetLevel(0)
			return
		}

		a.overlay.SetLevel(session.Level())
	}
}

func (a *App) hotkeyHint(shortcut string) string {
	if a.cfg.HotkeyMode == "toggle" {
		return fmt.Sprintf("Press %s to start and press again to stop", shortcut)
	}
	return fmt.Sprintf("Hold %s to record, then release to transcribe", shortcut)
}

func (a *App) dismissInFlightOverlay() bool {
	a.mu.Lock()
	defer a.mu.Unlock()

	if !a.transcribing {
		return false
	}

	a.dismissCompletionOverlay = true
	a.overlay.Hide()
	return true
}

func (a *App) showCompletionSuccess(text string) {
	if a.completionOverlayDismissed() {
		a.overlay.Hide()
		return
	}
	a.overlay.ShowSuccess(text)
}

func (a *App) showCompletionError(err error) {
	if a.completionOverlayDismissed() {
		a.overlay.Hide()
		return
	}
	a.overlay.ShowError(err)
}

func (a *App) hideCompletionOverlay() {
	a.overlay.Hide()
}

func (a *App) completionOverlayDismissed() bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return a.dismissCompletionOverlay
}

func (a *App) shouldIgnoreEmptySegmentCommit(state *recordingState, err error) bool {
	if err == nil || a.cfg.Streaming.Mode != "segment" {
		return false
	}
	if !errors.Is(err, openai.ErrInputAudioBufferCommitEmpty) {
		return false
	}
	if state.deliveredSegmentCount() == 0 {
		return false
	}
	return strings.TrimSpace(state.stream.Partial()) == ""
}

func (a *App) shutdown() error {
	a.mu.Lock()
	state := a.recording
	a.recording = nil
	a.mu.Unlock()

	if state != nil {
		stopCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		if err := state.session.Stop(stopCtx); err != nil {
			sessionlog.Warnf("shutdown: %v", err)
		}
		if state.stream != nil {
			_ = state.stream.Close()
		}
	}

	return nil
}

func (a *App) drainStreamResults(ctx context.Context, state *recordingState) error {
	for {
		select {
		case result := <-state.results:
			if result.err != nil {
				return result.err
			}
			if strings.TrimSpace(result.text) == "" {
				continue
			}
			if err := a.injector.Insert(ctx, state.target, result.text); err != nil {
				return err
			}
			sessionlog.Infof("stream segment inserted into window=%s", state.target.WindowID)
		default:
			return nil
		}
	}
}

func (a *App) waitForStreamResult(ctx context.Context, state *recordingState) (string, error) {
	for {
		select {
		case <-ctx.Done():
			return "", ctx.Err()
		case result := <-state.results:
			if result.err != nil {
				return "", result.err
			}
			return strings.TrimSpace(result.text), nil
		}
	}
}

func (a *App) waitForOptionalStreamResults(
	ctx context.Context,
	state *recordingState,
	grace time.Duration,
) error {
	timer := time.NewTimer(grace)
	defer timer.Stop()

	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return nil
		case result := <-state.results:
			if result.err != nil {
				return result.err
			}
			if strings.TrimSpace(result.text) == "" {
				continue
			}
			if err := a.injector.Insert(ctx, state.target, result.text); err != nil {
				return err
			}
			sessionlog.Infof("stream segment inserted into window=%s", state.target.WindowID)
			if !timer.Stop() {
				select {
				case <-timer.C:
				default:
				}
			}
			timer.Reset(grace)
		}
	}
}

func (a *App) pumpAudio(ctx context.Context, state *recordingState) {
	type startStreamResult struct {
		stream *openai.Stream
		err    error
	}

	resultCh := make(chan startStreamResult, 1)
	go func() {
		stream, err := a.transcribe.StartStream(
			ctx,
			a.cfg.Recording.SampleRate,
			a.cfg.Recording.Channels,
		)
		resultCh <- startStreamResult{stream: stream, err: err}
	}()

	var pending [][]int16
	samples := state.session.Samples()
	samplesOpen := true

	for {
		if state.stream == nil {
			select {
			case <-ctx.Done():
				a.finishPump(state, ctx.Err())
				return
			case result := <-resultCh:
				if result.err != nil {
					a.finishPump(state, fmt.Errorf("start transcription stream: %w", result.err))
					return
				}
				state.stream = result.stream
				sessionlog.Infof("realtime transcription stream ready")
				go a.consumeStreamEvents(ctx, state)
				for _, chunk := range pending {
					if err := state.stream.Append(ctx, chunk); err != nil {
						a.finishPump(state, err)
						return
					}
					state.markAudioAppended()
				}
				pending = nil
				if !samplesOpen {
					a.finishPump(state, nil)
					return
				}
			case chunk, ok := <-samples:
				if !ok {
					samplesOpen = false
					samples = nil
					continue
				}
				if len(chunk) == 0 {
					continue
				}
				pending = append(pending, chunk)
			}
			continue
		}

		select {
		case <-ctx.Done():
			a.finishPump(state, ctx.Err())
			return
		case chunk, ok := <-samples:
			if !ok {
				a.finishPump(state, nil)
				return
			}
			if len(chunk) == 0 {
				continue
			}
			if err := state.stream.Append(ctx, chunk); err != nil {
				a.finishPump(state, err)
				return
			}
			state.markAudioAppended()
		}
	}
}

func (a *App) finishPump(state *recordingState, err error) {
	select {
	case state.pumpDone <- err:
	default:
	}
}

func (a *App) consumeStreamEvents(ctx context.Context, state *recordingState) {
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-state.stream.Events():
			if !ok {
				return
			}
			if err := a.handleStreamEvent(ctx, state, event); err != nil {
				a.pushStreamResult(state, "", err)
				return
			}
		}
	}
}

func (a *App) handleStreamEvent(
	ctx context.Context,
	state *recordingState,
	event openai.StreamEvent,
) error {
	switch event.Type {
	case openai.StreamEventPartial:
		if a.cfg.Streaming.ShowPartialOverlay {
			a.overlay.SetListeningText(state.target.WindowClass, event.Text)
		}
		return nil
	case openai.StreamEventFinal:
		text := strings.TrimSpace(event.Text)
		if text == "" {
			return nil
		}
		text = state.formatSegmentText(text)
		state.clearPendingFinalCommit()
		if a.cfg.Streaming.Mode == "segment" && state.liveSegmentsEnabled() {
			if err := a.injector.InsertLive(ctx, state.target, text); err != nil {
				return err
			}
			state.noteDeliveredSegment()
			a.markLiveInsert()
			a.overlay.AnimateChunk(text)
			if a.cfg.Streaming.ShowPartialOverlay {
				a.overlay.SetListeningText(state.target.WindowClass, text)
			}
			sessionlog.Infof("stream segment inserted into window=%s", state.target.WindowID)
			return nil
		}
		a.pushStreamResult(state, text, nil)
		return nil
	case openai.StreamEventError:
		if event.Err != nil {
			return event.Err
		}
		return errors.New("openai stream error")
	default:
		return nil
	}
}

func (a *App) pushStreamResult(state *recordingState, text string, err error) {
	select {
	case state.results <- streamResult{text: text, err: err}:
	default:
	}
}

func (s *recordingState) liveSegmentsEnabled() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.liveSegmentDelivery
}

func (s *recordingState) setLiveSegmentDelivery(enabled bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.liveSegmentDelivery = enabled
}

func (s *recordingState) noteDeliveredSegment() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.deliveredSegments++
}

func (s *recordingState) deliveredSegmentCount() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.deliveredSegments
}

func (s *recordingState) markAudioAppended() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pendingFinalCommit = true
}

func (s *recordingState) clearPendingFinalCommit() {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.pendingFinalCommit = false
}

func (s *recordingState) needsFinalCommit() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.pendingFinalCommit
}

func (s *recordingState) formatSegmentText(text string) string {
	s.mu.Lock()
	defer s.mu.Unlock()

	text = strings.TrimSpace(text)
	if text == "" {
		return ""
	}
	if s.deliveredSegments == 0 {
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

func (a *App) markLiveInsert() {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.lastLiveInsert = time.Now()
}

func (a *App) shouldIgnoreSyntheticUp() bool {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.cfg.HotkeyMode != "hold" || a.cfg.Streaming.Mode != "segment" {
		return false
	}
	if a.recording == nil {
		return false
	}
	return time.Since(a.lastLiveInsert) < syntheticReleaseGuard
}

func startsWithPunctuation(text string) bool {
	if text == "" {
		return false
	}
	switch text[0] {
	case '.', ',', ';', ':', '!', '?', ')', ']', '}':
		return true
	default:
		return false
	}
}
