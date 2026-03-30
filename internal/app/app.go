package app

import (
	"context"
	"errors"
	"fmt"
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
	overlay    *overlay.Overlay
	recorder   *recorder.Recorder
	injector   *injector.Injector
	transcribe *openai.Client
	store      *securestore.Store

	mu           sync.Mutex
	recording    *recordingState
	transcribing bool
	lastToggle   time.Time
	sequence     uint64
}

type recordingState struct {
	id        uint64
	startedAt time.Time
	session   *recorder.Session
	stream    *openai.Stream
	pumpDone  chan error
	target    injector.Target
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
	sessionlog.Infof("starting vtt session")
	recorder.CleanupStale()

	apiKey, err := a.store.APIKey()
	if err != nil {
		return err
	}

	a.recorder = recorder.New()
	a.injector = injector.New(a.cfg.Insertion)
	a.transcribe = openai.New(apiKey, a.cfg.OpenAI)

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

	stream, err := a.transcribe.StartStream(ctx, a.cfg.Recording.SampleRate, a.cfg.Recording.Channels)
	if err != nil {
		sessionlog.Errorf("start transcription stream: %v", err)
		a.overlay.ShowError(err)
		return
	}

	session, err := a.recorder.Start(ctx, a.cfg.Recording)
	if err != nil {
		_ = stream.Close()
		sessionlog.Errorf("start recording: %v", err)
		a.overlay.ShowError(err)
		return
	}

	a.sequence++
	state := &recordingState{
		id:        a.sequence,
		startedAt: time.Now(),
		session:   session,
		stream:    stream,
		pumpDone:  make(chan error, 1),
		target:    target,
	}
	a.recording = state
	a.overlay.ShowListening(target.WindowClass)
	sessionlog.Infof("recording started: %d Hz, %d channel(s), realtime transcription armed",
		state.session.SampleRate(), state.session.Channels())
	go a.pumpAudio(ctx, state)
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
	defer state.stream.Close()

	stopCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()

	if err := state.session.Stop(stopCtx); err != nil {
		sessionlog.Errorf("stop recording: %v", err)
		a.overlay.ShowError(err)
		return
	}
	if err := <-state.pumpDone; err != nil {
		sessionlog.Errorf("stream audio: %v", err)
		a.overlay.ShowError(err)
		return
	}
	sessionlog.Infof("audio captured successfully: %d bytes streamed over %s",
		state.session.BytesCaptured(), state.session.Duration().Round(10*time.Millisecond))

	transcribeCtx, transcribeCancel := context.WithTimeout(
		ctx,
		time.Duration(a.cfg.OpenAI.RequestLimit)*time.Second,
	)
	defer transcribeCancel()

	text, err := state.stream.Commit(transcribeCtx)
	if err != nil {
		sessionlog.Errorf("transcribe audio: %v", err)
		a.overlay.ShowError(err)
		return
	}
	sessionlog.Infof("transcription complete: %d characters", len(text))

	if text == "" {
		sessionlog.Warnf("transcription was empty")
		a.overlay.ShowError(errors.New("transcription came back empty"))
		return
	}

	if err := a.injector.Insert(ctx, state.target, text); err != nil {
		sessionlog.Errorf("insert transcript: %v", err)
		a.overlay.ShowError(err)
		return
	}
	sessionlog.Infof("transcript inserted into window=%s", state.target.WindowID)

	a.overlay.ShowSuccess(text)
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
		_ = state.stream.Close()
	}

	return nil
}

func (a *App) pumpAudio(ctx context.Context, state *recordingState) {
	defer func() {
		select {
		case state.pumpDone <- nil:
		default:
		}
	}()

	for {
		select {
		case <-ctx.Done():
			select {
			case state.pumpDone <- ctx.Err():
			default:
			}
			return
		case chunk, ok := <-state.session.Samples():
			if !ok {
				return
			}
			if len(chunk) == 0 {
				continue
			}
			if err := state.stream.Append(ctx, chunk); err != nil {
				select {
				case state.pumpDone <- err:
				default:
				}
				return
			}
		}
	}
}
