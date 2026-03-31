package app

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	"go.opentelemetry.io/otel/attribute"
	"go.opentelemetry.io/otel/trace"

	"vocis/internal/config"
	"vocis/internal/hotkeys"
	"vocis/internal/injector"
	"vocis/internal/openai"
	"vocis/internal/overlay"
	"vocis/internal/recorder"
	"vocis/internal/securestore"
	"vocis/internal/sessionlog"
	"vocis/internal/telemetry"
)

type App struct {
	cfg        config.Config
	overlay    overlayUI
	recorder   *recorder.Recorder
	injector   injectorClient
	transcribe *openai.Client
	store      *securestore.Store

	mu                       sync.Mutex
	recording                *recordingState
	transcribing             bool
	transcribeCancel         context.CancelFunc
	dismissCompletionOverlay bool
	lastToggle               time.Time
	sequence                 uint64
	shortcut                 string
}

type recordingState struct {
	id        uint64
	startedAt time.Time
	session   *recorder.Session
	dictation *openai.DictationSession
	cancel    context.CancelFunc
	target      injector.Target
	liveText    string
	displayText string
	span        trace.Span
	spanCtx     context.Context
}

type overlayUI interface {
	ShowHint(text string)
	ShowListening(windowClass, hotkeyMode string)
	SetListeningText(windowClass, text string)
	AnimateChunk(text string)
	ShowFinishing(body, shortcut string)
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
	sessionlog.Infof("starting vocis session")
	recorder.CleanupStale()

	apiKey, err := a.store.APIKey()
	if err != nil {
		return err
	}

	a.recorder = recorder.New()
	a.injector = injector.New(a.cfg.Insertion, a.cfg.Hotkey)
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

	a.shortcut = hk.Shortcut()
	a.overlay.ShowHint(a.hotkeyHint(a.shortcut))

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
	a.overlay.ShowListening("", a.cfg.HotkeyMode)

	spanCtx, recordingSpan := telemetry.StartSpan(ctx, "vocis.dictation")

	session, err := a.recorder.Start(spanCtx, a.cfg.Recording)
	if err != nil {
		telemetry.EndSpan(recordingSpan, err)
		sessionlog.Errorf("start recording: %v", err)
		a.overlay.ShowError(err)
		return
	}

	target, err := a.injector.CaptureTarget(spanCtx)
	if err != nil {
		telemetry.EndSpan(recordingSpan, err)
		sessionlog.Errorf("capture target: %v", err)
		_ = session.Stop(ctx)
		a.overlay.ShowError(err)
		return
	}
	recordingSpan.SetAttributes(
		attribute.String("target.window_id", target.WindowID),
		attribute.String("target.window_class", target.WindowClass),
	)
	if a.cfg.LogWindowTitle {
		sessionlog.Infof("starting recording for window=%s class=%s title=%q",
			target.WindowID, target.WindowClass, target.WindowName)
	} else {
		sessionlog.Infof("starting recording for window=%s class=%s",
			target.WindowID, target.WindowClass)
	}

	recordCtx, cancel := context.WithCancel(spanCtx)

	a.sequence++
	state := &recordingState{
		id:        a.sequence,
		startedAt: time.Now(),
		session:   session,
		cancel:    cancel,
		target:    target,
		span:      recordingSpan,
		spanCtx:   spanCtx,
	}
	dictation, err := a.transcribe.StartDictation(
		recordCtx,
		a.cfg.Recording.SampleRate,
		a.cfg.Recording.Channels,
		session.Samples(),
	)
	if err != nil {
		cancel()
		_ = session.Stop(context.Background())
		telemetry.EndSpan(recordingSpan, err)
		sessionlog.Errorf("start dictation: %v", err)
		a.overlay.ShowError(err)
		return
	}
	state.dictation = dictation
	a.recording = state
	a.overlay.ShowListening(target.WindowClass, a.cfg.HotkeyMode)
	sessionlog.Infof("recording started: %d Hz, %d channel(s), connecting realtime transcription",
		state.session.SampleRate(), state.session.Channels())
	go a.consumeDictationEvents(recordCtx, state)
	go a.monitorRecordingLevel(ctx, state.id, state.session)

	if a.cfg.Recording.MaxDurationSeconds > 0 {
		go a.forceStopAfter(ctx, state.id, time.Duration(a.cfg.Recording.MaxDurationSeconds)*time.Second)
	}
}

func (a *App) stopRecordingLocked(ctx context.Context) {
	state := a.recording
	a.recording = nil
	a.transcribing = true
	a.overlay.ShowFinishing(state.displayText, a.shortcut)
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
	a.transcribing = true
	a.mu.Unlock()

	a.overlay.ShowFinishing(state.displayText, a.shortcut)
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
	defer telemetry.EndSpan(state.span, nil)

	spanCtx := state.spanCtx

	stopCtx, cancel := context.WithTimeout(spanCtx, 10*time.Second)
	defer cancel()

	if err := state.session.Stop(stopCtx); err != nil {
		if errors.Is(err, recorder.ErrRecordingTooShort) {
			state.span.SetAttributes(attribute.Bool("recording.discarded", true))
			sessionlog.Infof("discarding short recording after %s",
				state.session.Duration().Round(10*time.Millisecond))
			state.cancel()
			a.hideCompletionOverlay()
			return
		}
		state.span.RecordError(err)
		sessionlog.Errorf("stop recording: %v", err)
		a.showCompletionError(err)
		state.cancel()
		return
	}
	state.span.SetAttributes(
		attribute.Int64("recording.bytes", state.session.BytesCaptured()),
		attribute.String("recording.duration", state.session.Duration().Round(10*time.Millisecond).String()),
	)
	sessionlog.Infof("audio captured successfully: %d bytes streamed over %s",
		state.session.BytesCaptured(), state.session.Duration().Round(10*time.Millisecond))

	transcribeCtx, transcribeCancel := context.WithTimeout(
		spanCtx,
		time.Duration(a.cfg.OpenAI.RequestLimit)*time.Second,
	)
	defer transcribeCancel()

	a.mu.Lock()
	a.transcribeCancel = transcribeCancel
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		a.transcribeCancel = nil
		a.mu.Unlock()
	}()

	transcribeCtx, transcribeSpan := telemetry.StartSpan(transcribeCtx, "vocis.transcribe.finalize")
	result, err := state.dictation.Finalize(transcribeCtx)
	telemetry.EndSpan(transcribeSpan, err)
	if err != nil {
		if a.completionOverlayDismissed() {
			sessionlog.Infof("transcription cancelled by user")
			return
		}
		sessionlog.Errorf("transcribe audio: %v", err)
		a.showCompletionError(err)
		return
	}
	trailing := strings.TrimSpace(result.Text)

	text := state.liveText
	if trailing != "" {
		if text == "" {
			text = trailing
		} else {
			text = text + " " + trailing
		}
	}
	text = strings.TrimSpace(text)
	state.span.SetAttributes(
		attribute.Int("transcription.total_chars", len(text)),
		attribute.Int("transcription.live_chars", len(state.liveText)),
		attribute.Int("transcription.trailing_chars", len(trailing)),
	)
	sessionlog.Infof("transcription complete: %d characters (%d live + %d trailing)",
		len(text), len(state.liveText), len(trailing))

	if text == "" {
		sessionlog.Warnf("transcription was empty")
		a.showCompletionError(errors.New("transcription came back empty"))
		return
	}

	insertCtx, insertSpan := telemetry.StartSpan(spanCtx, "vocis.inject",
		attribute.String("target.window_id", state.target.WindowID),
		attribute.String("target.window_class", state.target.WindowClass),
		attribute.Int("text.length", len(text)),
	)
	err = a.injector.Insert(insertCtx, state.target, text)
	telemetry.EndSpan(insertSpan, err)
	if err != nil {
		sessionlog.Errorf("insert transcript: %v", err)
		a.showCompletionError(err)
		return
	}
	sessionlog.Infof("transcript inserted into window=%s", state.target.WindowID)
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
	if a.transcribeCancel != nil {
		a.transcribeCancel()
	}
	a.overlay.Hide()
	sessionlog.Infof("transcription cancelled by user")
	return true
}

func (a *App) showCompletionSuccess(text string) {
	a.overlay.Hide()
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
		_, _ = state.dictation.Finalize(stopCtx)
	}

	return nil
}

func (a *App) consumeDictationEvents(ctx context.Context, state *recordingState) {
	for {
		select {
		case <-ctx.Done():
			return
		case event, ok := <-state.dictation.Events():
			if !ok {
				return
			}
			if err := a.handleDictationEvent(ctx, state, event); err != nil {
				sessionlog.Errorf("live dictation event: %v", err)
				a.showCompletionError(err)
				return
			}
		}
	}
}

func (a *App) handleDictationEvent(
	ctx context.Context,
	state *recordingState,
	event openai.DictationEvent,
) error {
	switch event.Type {
	case openai.DictationEventPartial:
		if a.cfg.Streaming.ShowPartialOverlay {
			display := state.displayText
			if partial := strings.TrimSpace(event.Text); partial != "" {
				if display != "" {
					display += " "
				}
				display += partial
			}
			if display != "" {
				a.overlay.SetListeningText(state.target.WindowClass, display)
			}
		}
		return nil
	case openai.DictationEventSegment:
		text := strings.TrimSpace(event.Text)
		if text == "" {
			return nil
		}
		state.liveText += event.Text
		if state.displayText != "" {
			state.displayText += "\n"
		}
		state.displayText += text
		a.overlay.SetListeningText(state.target.WindowClass, state.displayText)
		sessionlog.Infof("stream segment accumulated: %d chars total", len(state.liveText))
		return nil
	default:
		return nil
	}
}
