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

	"vocis/internal/audiocapture"
	"vocis/internal/config"
	"vocis/internal/platform"
	"vocis/internal/recorder"
	"vocis/internal/sessionlog"
	"vocis/internal/telemetry"
	"vocis/internal/transcribe"
	"vocis/internal/ui"
)

type App struct {
	cfg            config.Config
	overlay        OverlayUI
	recorder       *recorder.Recorder
	injector       InjectorClient
	transcribe     *transcribe.Client
	ducker         AudioDucker
	registerHotkey HotkeyRegistrar
	hotkeyBackend  string

	mu sync.Mutex
	// recording is set while the mic is open (hotkey held); finishing
	// is set from release until the transcript is delivered or fails.
	// At most one dictation exists at a time, so both never point at
	// different states simultaneously.
	recording  *recordingState
	finishing  *recordingState
	lastToggle time.Time
	shortcut   string
}

type recordingState struct {
	startedAt time.Time
	session   *recorder.Session
	dictation transcribe.Dictation
	// ctx spans the whole dictation (recording + finalize); cancel
	// aborts every goroutine attached to it.
	ctx    context.Context
	cancel context.CancelFunc
	// dismissed is set (under App.mu) when the user cancels during
	// finishing, so late results skip the overlay instead of
	// overwriting the "Cancelled" warning.
	dismissed  bool
	target     platform.Target
	submitMode bool
	span       trace.Span
	spanCtx    context.Context
	activeSpan trace.Span
}

type OverlayUI interface {
	ShowHint(text string)
	ShowListening(windowClass, hotkeyMode string)
	SetConnected(windowClass string)
	SetLoadingModel(modelName string)
	SetSubmitMode(enabled bool)
	SetListeningText(windowClass, text string)
	ShowFinishing(body, shortcut string)
	SetFinishingText(body string)
	ShowError(err error)
	ShowWarning(subtitle string)
	SetLevel(level float64)
	Hide()
	Close()
}

type InjectorClient interface {
	CaptureTarget(ctx context.Context) (platform.Target, error)
	Insert(ctx context.Context, target platform.Target, text string) error
	PressEnter(ctx context.Context, target platform.Target) error
}

type AudioDucker interface {
	Duck()
	Restore()
}

// HotkeySource provides key events from a registered global hotkey.
type HotkeySource interface {
	Down() <-chan struct{}
	Up() <-chan struct{}
	Tap() <-chan struct{}
	Shortcut() string
	Close() error
}

// HotkeyRegistrar creates a HotkeySource for the given shortcut string.
type HotkeyRegistrar func(shortcut string) (HotkeySource, error)

const minToggleInterval = 250 * time.Millisecond

// Deps holds the platform-specific dependencies injected into the App.
type Deps struct {
	Overlay        OverlayUI
	Injector       InjectorClient
	Ducker         AudioDucker
	RegisterHotkey HotkeyRegistrar
	// HotkeyBackend is a short label ("x11", "gnome-extension") recorded on
	// each session's root trace span so Jaeger queries can filter by backend.
	HotkeyBackend string
}

func New(cfg config.Config, deps Deps) *App {
	return &App{
		cfg:            cfg,
		overlay:        deps.Overlay,
		injector:       deps.Injector,
		ducker:         deps.Ducker,
		registerHotkey: deps.RegisterHotkey,
		hotkeyBackend:  deps.HotkeyBackend,
	}
}

func (a *App) Run(ctx context.Context) error {
	if err := a.cfg.Validate(); err != nil {
		return err
	}
	sessionlog.Infof("starting vocis session")

	a.recorder = recorder.New()
	a.transcribe = transcribe.New(a.cfg.Transcription)

	// Audio capture GC sweeps the per-session WAV dir on a fixed
	// cadence. The writer itself is opened lazily per dictation by the
	// chat-audio session (which is also where the config lives — under
	// transcription.audio_capture). GC ties to the app's long-lived
	// context so it stops cleanly on shutdown.
	audiocapture.StartGC(ctx, a.cfg.Transcription.AudioCapture)

	// Defer overlay close before the Lemonade preflight so that path
	// can use ShowError below — without this, a failed preflight would
	// return before the defer was registered and the overlay would
	// never get cleaned up.
	defer a.overlay.Close()

	// For Lemonade, proactively check that both configured models are
	// resident and force-load anything that isn't. Avoids a 5-10s load
	// stall on the user's first dictation of the session (which would
	// otherwise manifest as "no transcript ever arrives" because the
	// load runs on the WS path while audio is already flowing).
	//
	// The reachability check itself runs synchronously: if Lemonade
	// isn't running there's nothing for vocis to do — the first
	// dictation would silently fail at the WS connect — so we surface
	// the error at startup, both via the overlay (so users running
	// vocis under a service manager actually see it) and via the
	// returned error (so it lands in stderr/journal). The actual
	// model-warm requests still fire and forget in the background.
	if err := transcribe.EnsureLemonadeModelsLoaded(ctx, a.cfg, a.transcribe); err != nil {
		sessionlog.Errorf("lemonade preflight: %v", err)
		a.overlay.ShowError(err)
		// Hold long enough for the overlay to actually be visible.
		// Reusing AutoHideMillis keeps this consistent with how long
		// every other transient overlay stays up, and avoids adding a
		// new tunable just for the preflight path. Bail early on ctx
		// cancellation so Ctrl-C still exits promptly.
		select {
		case <-time.After(time.Duration(ui.OverlayAutoHideMillis) * time.Millisecond):
		case <-ctx.Done():
		}
		return err
	}

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
		case <-hk.Tap():
			a.handleTap()
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

	a.mu.Lock()
	defer a.mu.Unlock()
	if a.finishing != nil || a.recording != nil {
		return
	}
	a.startRecordingLocked(ctx)
}

func (a *App) handleUp(ctx context.Context) {
	if a.cfg.HotkeyMode != "hold" {
		return
	}

	a.mu.Lock()
	defer a.mu.Unlock()
	if a.recording == nil {
		return
	}
	a.stopRecordingLocked(ctx)
}

func (a *App) handleToggle(ctx context.Context) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if time.Since(a.lastToggle) < minToggleInterval {
		return
	}
	a.lastToggle = time.Now()

	if a.finishing != nil {
		return
	}

	if a.recording == nil {
		a.startRecordingLocked(ctx)
		return
	}

	a.stopRecordingLocked(ctx)
}

func (a *App) handleTap() {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.recording == nil {
		return
	}
	a.toggleSubmitMode()
}

func (a *App) toggleSubmitMode() {
	state := a.recording
	if state == nil {
		return
	}
	state.submitMode = !state.submitMode
	if state.submitMode {
		sessionlog.Infof("submit mode enabled")
		a.overlay.SetSubmitMode(true)
	} else {
		sessionlog.Infof("submit mode disabled")
		a.overlay.SetSubmitMode(false)
	}
	state.span.AddEvent("overlay.submit_mode",
		trace.WithAttributes(attribute.Bool("enabled", state.submitMode)),
	)
}

func (a *App) reloadConfig() {
	cfg, path, err := config.Load()
	if err != nil {
		sessionlog.Warnf("config reload failed, keeping current: %v", err)
		return
	}
	a.cfg.Transcription = cfg.Transcription
	a.cfg.Recording = cfg.Recording
	a.cfg.LogWindowTitle = cfg.LogWindowTitle
	a.transcribe = transcribe.New(a.cfg.Transcription)
	sessionlog.Infof("config reloaded: %s", path)
}

func (a *App) startRecordingLocked(ctx context.Context) {
	a.reloadConfig()
	a.ducker.Duck()
	a.overlay.ShowListening("", a.cfg.HotkeyMode)

	spanCtx, recordingSpan := telemetry.StartSpan(ctx, "vocis.dictation",
		attribute.String("hotkey.backend", a.hotkeyBackend),
	)

	// Open the mic FIRST, before the Lemonade model preflight. On a cold
	// load preflight runs 5-10 s; if we held off on recorder.Start until
	// after, every word the user spoke during that window was silently
	// lost — they pressed the hotkey, started talking, and the first
	// few seconds never reached the transcription pipeline. The drain
	// goroutine below collects those samples into a preroll buffer that
	// we replay into StartDictation once preflight completes.
	session, err := a.recorder.Start(spanCtx, a.cfg.Recording)
	if err != nil {
		a.ducker.Restore()
		telemetry.EndSpan(recordingSpan, err)
		sessionlog.Errorf("start recording: %v", err)
		a.overlay.ShowError(err)
		return
	}

	// Drain the mic into a preroll buffer while preflight runs. The
	// recorder's samples channel buffers 16 chunks (~2 s at 16 kHz);
	// without active draining it would block the recorder's writer
	// goroutine on a cold preflight.
	stopDrain := drainPreroll(session.Samples())

	// Preflight: make sure the configured transcription model is resident
	// on the Lemonade backend before we open the WS for live audio. Runs
	// inline so the overlay clearly says "Loading X..." while the user
	// waits — but the mic is hot the whole time, so any speech during
	// the load is preserved as preroll.
	preflightStart := time.Now()
	if err := transcribe.EnsureTranscribeModelLoaded(spanCtx, a.cfg.Transcription, func(model string) {
		a.overlay.SetLoadingModel(model)
		recordingSpan.AddEvent("lemonade.model.loading",
			trace.WithAttributes(attribute.String("model", model)),
		)
	}); err != nil {
		_ = stopDrain()
		_ = session.Stop(ctx)
		a.ducker.Restore()
		telemetry.EndSpan(recordingSpan, err)
		sessionlog.Errorf("transcription model preflight: %v", err)
		a.overlay.ShowError(fmt.Errorf("model load failed: %w", err))
		return
	}
	preflightElapsed := time.Since(preflightStart)
	prerolled := stopDrain()
	if len(prerolled.chunks) > 0 {
		prerollMS := prerolled.samples * 1000 / recorder.SampleRate
		sessionlog.Infof("preroll: captured %d chunks (%d samples, %dms) during model preflight (%s)",
			len(prerolled.chunks), prerolled.samples, prerollMS, preflightElapsed.Round(10*time.Millisecond))
		recordingSpan.AddEvent("preroll.captured",
			trace.WithAttributes(
				attribute.Int("preroll.chunks", len(prerolled.chunks)),
				attribute.Int("preroll.samples", prerolled.samples),
				attribute.Int("preroll.duration_ms", prerollMS),
				attribute.String("preflight.elapsed", preflightElapsed.Round(time.Millisecond).String()),
			),
		)
	}

	// Wrap session.Samples(): emit the preroll chunks first, then forward
	// live samples. StartDictation reads from this wrapped channel and
	// can't tell the difference from a single contiguous mic stream.
	wrappedSamples := wrapSamplesWithPreroll(prerolled.chunks, session.Samples())

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
		attribute.String("hotkey_mode", a.cfg.HotkeyMode),
	)
	if a.cfg.LogWindowTitle {
		sessionlog.Infof("starting recording for window=%s class=%s title=%q",
			target.WindowID, target.WindowClass, target.WindowName)
	} else {
		sessionlog.Infof("starting recording for window=%s class=%s",
			target.WindowID, target.WindowClass)
	}

	recordCtx, cancel := context.WithCancel(spanCtx)
	state := &recordingState{
		startedAt:  time.Now(),
		session:    session,
		ctx:        recordCtx,
		cancel:     cancel,
		target:     target,
		span:       recordingSpan,
		spanCtx:    spanCtx,
		submitMode: a.cfg.Insertion.AutoSubmit,
	}
	dictation, err := a.transcribe.StartDictation(recordCtx, transcribe.DictationOpts{
		SampleRate: recorder.SampleRate,
		Channels:   recorder.Channels,
		Samples:    wrappedSamples,
		OnConnected: func() {
			a.overlay.SetConnected(target.WindowClass)
			recordingSpan.AddEvent("overlay.connected")
		},
	})
	if err != nil {
		cancel()
		_ = session.Stop(context.Background())
		telemetry.EndSpan(recordingSpan, err)
		sessionlog.Errorf("start dictation: %v", err)
		a.overlay.ShowError(err)
		return
	}
	state.dictation = dictation
	_, activeSpan := telemetry.StartSpan(spanCtx, "vocis.recording.active")
	state.activeSpan = activeSpan
	a.recording = state
	a.overlay.ShowListening(target.WindowClass, a.cfg.HotkeyMode)
	if state.submitMode {
		a.overlay.SetSubmitMode(true)
	}
	sessionlog.Infof("recording started: %d Hz, %d channel(s), connecting realtime transcription",
		state.session.SampleRate(), state.session.Channels())
	go a.consumeDictationEvents(state)
	go a.monitorRecordingLevel(state)

	if recorder.DefaultMaxDurationSeconds > 0 {
		go a.forceStopAfter(ctx, state, time.Duration(recorder.DefaultMaxDurationSeconds)*time.Second)
	}
}

func (a *App) stopRecordingLocked(ctx context.Context) {
	state := a.recording
	a.recording = nil
	a.finishing = state
	a.overlay.ShowFinishing("", a.shortcut)
	state.span.AddEvent("overlay.finishing")
	sessionlog.Infof("stopping recording duration=%s",
		time.Since(state.startedAt).Round(10*time.Millisecond))

	go a.finishRecording(ctx, state)
}

func (a *App) registerHotkeyWithFallback() (HotkeySource, error) {
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

		hk, err := a.registerHotkey(candidate)
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

func (a *App) forceStopAfter(ctx context.Context, state *recordingState, maxDuration time.Duration) {
	timer := time.NewTimer(maxDuration)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return
	case <-timer.C:
	}

	a.mu.Lock()
	if a.recording != state {
		a.mu.Unlock()
		return
	}
	a.recording = nil
	a.finishing = state
	a.mu.Unlock()

	a.overlay.ShowFinishing("", a.shortcut)
	state.span.AddEvent("overlay.finishing",
		trace.WithAttributes(attribute.Bool("auto_stop", true)),
	)
	sessionlog.Warnf("auto-stopping recording after timeout")
	go a.finishRecording(ctx, state)
}

func (a *App) finishRecording(ctx context.Context, state *recordingState) {
	// Safety net for error returns. The success path clears finishing
	// earlier via markDelivered() so the overlay fade-out doesn't
	// extend the dismissable window past the paste.
	defer a.markDelivered(state)
	var dictationErr error
	defer state.cancel()
	defer func() { telemetry.EndSpan(state.span, dictationErr) }()

	spanCtx := state.spanCtx

	if state.activeSpan != nil {
		telemetry.EndSpan(state.activeSpan, nil)
	}

	stopCtx, cancel := context.WithTimeout(spanCtx, 2*time.Second)
	defer cancel()

	if err := state.session.Stop(stopCtx); err != nil {
		a.ducker.Restore()
		if errors.Is(err, recorder.ErrRecordingTooShort) {
			state.span.SetAttributes(attribute.Bool("recording.discarded", true))
			sessionlog.Infof("discarding short recording duration=%s",
				state.session.Duration().Round(10*time.Millisecond))
			state.cancel()
			a.overlay.Hide()
			return
		}
		dictationErr = err
		sessionlog.Errorf("stop recording: %v", err)
		a.showCompletionError(state, err)
		state.cancel()
		return
	}
	a.ducker.Restore()

	state.span.SetAttributes(
		attribute.Int64("recording.bytes", state.session.BytesCaptured()),
		attribute.String("recording.duration", state.session.Duration().Round(10*time.Millisecond).String()),
	)
	sessionlog.Infof("audio captured bytes=%d duration=%s",
		state.session.BytesCaptured(), state.session.Duration().Round(10*time.Millisecond))

	sessionlog.Infof("finalizing recording=%s (no timeout — elapsed counter replaces deadline)",
		state.session.Duration().Round(10*time.Millisecond))

	finalizeStart := time.Now()
	transcribeCtx, transcribeSpan := telemetry.StartSpan(state.ctx, "vocis.transcribe.finalize")
	result, err := state.dictation.Finalize(transcribeCtx)
	finalizeDuration := time.Since(finalizeStart).Round(10 * time.Millisecond)
	telemetry.EndSpan(transcribeSpan, err)
	if err != nil {
		dictationErr = err
		if a.dismissed(state) {
			sessionlog.Infof("transcription cancelled by user elapsed=%s error=%v", finalizeDuration, err)
			return
		}
		sessionlog.Errorf("transcribe failed elapsed=%s error=%v", finalizeDuration, err)
		a.showCompletionError(state, err)
		return
	}
	text := strings.TrimSpace(result.Text)
	state.span.SetAttributes(attribute.Int("transcription.total_chars", len(text)))
	sessionlog.Infof("finalization completed elapsed=%s chars=%d", finalizeDuration, len(text))

	if text == "" {
		sessionlog.Warnf("transcription was empty")
		a.showCompletionError(state, errors.New("transcription came back empty"))
		return
	}
	a.overlay.SetFinishingText(text)

	if err := a.deliverTranscript(spanCtx, state, text); err != nil {
		dictationErr = err
	}
}

// deliverTranscript inserts the assembled transcript into the captured
// target, presses Enter on submit-mode, and surfaces the final overlay
// state (success, warning, or error). Returns a non-nil error only on
// hard insert failures — ErrTargetGone is soft because the transcript
// is on the clipboard, the transcription itself succeeded, and tainting
// the dictation span as failed would be misleading.
func (a *App) deliverTranscript(spanCtx context.Context, state *recordingState, text string) error {
	insertCtx, insertSpan := telemetry.StartSpan(spanCtx, "vocis.inject",
		attribute.String("target.window_id", state.target.WindowID),
		attribute.String("target.window_class", state.target.WindowClass),
		attribute.String("target.kitty_window_id", state.target.KittyWindowID),
		attribute.Int("text.length", len(text)),
	)
	err := a.injector.Insert(insertCtx, state.target, text)
	telemetry.EndSpan(insertSpan, err)
	if err != nil {
		if errors.Is(err, platform.ErrTargetGone) {
			state.span.AddEvent("overlay.warning",
				trace.WithAttributes(attribute.String("reason", "target_gone")),
			)
			sessionlog.Warnf("target window gone — transcript on clipboard (%d chars)", len(text))
			a.markDelivered(state)
			a.overlay.ShowWarning(ui.OverlayWarningTargetGone)
			return nil
		}
		sessionlog.Errorf("insert transcript: %v", err)
		a.showCompletionError(state, err)
		return err
	}

	if state.target.KittyWindowID != "" {
		sessionlog.Infof("transcript inserted into kitty window id=%s (OS window=%s) submit=%v",
			state.target.KittyWindowID, state.target.WindowID, state.submitMode)
	} else {
		sessionlog.Infof("transcript inserted into window=%s submit=%v",
			state.target.WindowID, state.submitMode)
	}
	if state.submitMode {
		if state.target.KittyWindowID != "" {
			sessionlog.Infof("submit mode: pressing Enter on kitty window id=%s", state.target.KittyWindowID)
		} else {
			sessionlog.Infof("submit mode: pressing Enter on window=%s", state.target.WindowID)
		}
		if err := a.injector.PressEnter(insertCtx, state.target); err != nil {
			sessionlog.Warnf("press enter failed: %v", err)
		}
	}
	// Paste (and submit Enter, if any) is done — the user has the
	// result. Clear finishing NOW so a quick follow-up hotkey press
	// starts a new dictation instead of being eaten by
	// dismissInFlightOverlay during the ~320ms overlay fade-out below.
	a.markDelivered(state)
	state.span.SetAttributes(attribute.Bool("submit_mode", state.submitMode))
	state.span.AddEvent("overlay.success")
	a.overlay.Hide()
	return nil
}

// markDelivered ends the dismissable phase of a dictation: a follow-up
// hotkey press starts a new dictation instead of cancelling this one.
// Idempotent, and a no-op if a different state is already finishing.
func (a *App) markDelivered(state *recordingState) {
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.finishing == state {
		a.finishing = nil
		sessionlog.Debugf("dictation finished: finishing state cleared (overlay cleanup may still be running)")
	}
}

func (a *App) monitorRecordingLevel(state *recordingState) {
	ticker := time.NewTicker(65 * time.Millisecond)
	defer ticker.Stop()

	for {
		select {
		case <-state.ctx.Done():
			return
		case <-ticker.C:
		}

		a.mu.Lock()
		active := a.recording == state
		a.mu.Unlock()
		if !active {
			a.overlay.SetLevel(0)
			return
		}

		a.overlay.SetLevel(state.session.Level())
	}
}

func (a *App) hotkeyHint(shortcut string) string {
	if a.cfg.HotkeyMode == "toggle" {
		return fmt.Sprintf("Press %s to start, press again to stop", shortcut)
	}
	return fmt.Sprintf("Hold %s, release to transcribe", shortcut)
}

// dismissInFlightOverlay cancels a dictation that is still finishing.
// Returns false when nothing is finishing, so the caller starts a new
// recording instead.
func (a *App) dismissInFlightOverlay() bool {
	a.mu.Lock()
	defer a.mu.Unlock()

	state := a.finishing
	if state == nil {
		return false
	}
	state.dismissed = true
	state.cancel()
	a.finishing = nil
	a.overlay.ShowWarning(ui.OverlayWarningCancelled)
	sessionlog.Infof("transcription cancelled by user")
	// Note: span is ended by the finishRecording defer, which will see the cancelled context.
	return true
}

func (a *App) showCompletionError(state *recordingState, err error) {
	if a.dismissed(state) {
		a.overlay.Hide()
		return
	}
	if isNoSpeechError(err) {
		a.overlay.ShowWarning(ui.OverlayWarningNoSpeech)
		return
	}
	a.overlay.ShowError(userFacingError(err))
}

func isNoSpeechError(err error) bool {
	return strings.Contains(err.Error(), "transcription came back empty")
}

func userFacingError(err error) error {
	msg := err.Error()
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return errors.New("Timed out waiting for transcription")
	case strings.Contains(msg, "i/o timeout"):
		return errors.New("Could not connect to Lemonade (network timeout)")
	default:
		return err
	}
}

func (a *App) dismissed(state *recordingState) bool {
	a.mu.Lock()
	defer a.mu.Unlock()
	return state.dismissed
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

// consumeDictationEvents drives the overlay from the session's event
// stream. Display only: the authoritative transcript comes back from
// Finalize, so a dropped or late event never loses text.
func (a *App) consumeDictationEvents(state *recordingState) {
	for {
		select {
		case <-state.ctx.Done():
			return
		case event, ok := <-state.dictation.Events():
			if !ok {
				return
			}
			a.handleDictationEvent(state, event)
		}
	}
}

// handleDictationEvent renders a streaming partial. The POST only
// happens at release, so partials arrive during the Finishing phase;
// both setters are called and each short-circuits when its view is
// not the active one.
func (a *App) handleDictationEvent(state *recordingState, event transcribe.DictationEvent) {
	if event.Type != transcribe.DictationEventPartial {
		return
	}
	text := strings.TrimSpace(event.Text)
	a.overlay.SetListeningText(state.target.WindowClass, text)
	a.overlay.SetFinishingText(text)
}

// prerollSnapshot is what drainPreroll returns when stopped: the chunks
// captured during the model preflight plus the total sample count.
type prerollSnapshot struct {
	chunks  [][]int16
	samples int
}

// drainPreroll spawns a goroutine that copies samples from `src` into a
// local buffer until stop() is called. stop() returns the captured
// snapshot — chunks in arrival order, plus total sample count. Safe to
// call stop() exactly once; subsequent calls return an empty snapshot.
//
// Used during the Lemonade model preflight: the mic is already open
// (so the user's first words after pressing the hotkey aren't lost),
// but StartDictation isn't ready yet. The recorder's samples channel
// buffers ~2 s at 16 kHz; on a 5-10 s cold preflight that channel
// would back up and block the recorder's writer. Active draining
// keeps the writer alive and preserves the audio for replay.
func drainPreroll(src <-chan []int16) func() prerollSnapshot {
	type result struct {
		chunks  [][]int16
		samples int
	}
	stopCh := make(chan struct{})
	resultCh := make(chan result, 1)

	go func() {
		var snap result
		for {
			select {
			case <-stopCh:
				resultCh <- snap
				return
			case chunk, ok := <-src:
				if !ok {
					resultCh <- snap
					return
				}
				snap.chunks = append(snap.chunks, chunk)
				snap.samples += len(chunk)
			}
		}
	}()

	var once sync.Once
	return func() prerollSnapshot {
		var snap prerollSnapshot
		once.Do(func() {
			close(stopCh)
			r := <-resultCh
			snap = prerollSnapshot{chunks: r.chunks, samples: r.samples}
		})
		return snap
	}
}

// wrapSamplesWithPreroll returns a channel that emits every chunk in
// `preroll` (in order) and then forwards every chunk read from `live`
// until live closes. Lets StartDictation consume preroll + live as a
// single contiguous stream without the dictation pipeline needing to
// know preroll exists.
func wrapSamplesWithPreroll(preroll [][]int16, live <-chan []int16) <-chan []int16 {
	// 32 chunks of headroom matches the recorder's natural pacing
	// (~2 s at 16 kHz, 2048 samples per chunk). Big enough that the
	// dictation consumer's pace is the bottleneck, not the wrapper.
	out := make(chan []int16, 32)
	go func() {
		defer close(out)
		for _, chunk := range preroll {
			out <- chunk
		}
		for chunk := range live {
			out <- chunk
		}
	}()
	return out
}
