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

	mu                       sync.Mutex
	recording                *recordingState
	transcribing             bool
	dismissCompletionOverlay bool
	lastToggle               time.Time
	lastLiveInsert           time.Time
	sequence                 uint64
}

type recordingState struct {
	id        uint64
	startedAt time.Time
	session   *recorder.Session
	dictation *openai.DictationSession
	cancel    context.CancelFunc
	target    injector.Target
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
		id:        a.sequence,
		startedAt: time.Now(),
		session:   session,
		cancel:    cancel,
		target:    target,
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
		sessionlog.Errorf("start dictation: %v", err)
		a.overlay.ShowError(err)
		return
	}
	state.dictation = dictation
	a.recording = state
	a.overlay.ShowListening(target.WindowClass)
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
			a.hideCompletionOverlay()
			return
		}
		sessionlog.Errorf("stop recording: %v", err)
		a.showCompletionError(err)
		state.cancel()
		return
	}
	sessionlog.Infof("audio captured successfully: %d bytes streamed over %s",
		state.session.BytesCaptured(), state.session.Duration().Round(10*time.Millisecond))

	transcribeCtx, transcribeCancel := context.WithTimeout(
		ctx,
		time.Duration(a.cfg.OpenAI.RequestLimit)*time.Second,
	)
	defer transcribeCancel()

	result, err := state.dictation.Finalize(transcribeCtx)
	if err != nil {
		sessionlog.Errorf("transcribe audio: %v", err)
		a.showCompletionError(err)
		return
	}
	text := strings.TrimSpace(result.Text)
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
			a.overlay.SetListeningText(state.target.WindowClass, event.Text)
		}
		return nil
	case openai.DictationEventSegment:
		text := strings.TrimSpace(event.Text)
		if text == "" {
			return nil
		}
		if err := a.injector.InsertLive(ctx, state.target, event.Text); err != nil {
			return err
		}
		a.markLiveInsert()
		a.overlay.AnimateChunk(event.Text)
		if a.cfg.Streaming.ShowPartialOverlay {
			a.overlay.SetListeningText(state.target.WindowClass, event.Text)
		}
		sessionlog.Infof("stream segment inserted into window=%s", state.target.WindowID)
		return nil
	default:
		return nil
	}
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
