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
	"vocis/internal/transcribe"
	"vocis/internal/platform"
	"vocis/internal/recorder"
	"vocis/internal/securestore"
	"vocis/internal/sessionlog"
	"vocis/internal/telemetry"
)

type App struct {
	cfg            config.Config
	overlay        OverlayUI
	recorder       *recorder.Recorder
	injector       InjectorClient
	transcribe     *transcribe.Client
	store          *securestore.Store
	apiKey         string
	ducker         AudioDucker
	registerHotkey HotkeyRegistrar
	hotkeyBackend  string

	mu                       sync.Mutex
	recording                *recordingState
	transcribing             bool
	transcribeCancel         context.CancelFunc
	sessionCancel            context.CancelFunc
	dismissCompletionOverlay bool
	lastToggle               time.Time
	sequence                 uint64
	shortcut                 string
}

type recordingState struct {
	id        uint64
	startedAt time.Time
	session   *recorder.Session
	dictation *transcribe.DictationSession
	cancel    context.CancelFunc
	target      platform.Target
	liveText    string
	displayText string // committed segments (canonical text), one per line
	// currentPartial is the in-flight (still-streaming) turn. The overlay
	// renders displayText + currentPartial; on the next Partial it
	// replaces, on the next Segment it gets cleared and the canonical
	// text lands in displayText instead.
	currentPartial string
	submitMode     bool
	span           trace.Span
	spanCtx        context.Context
	activeSpan     trace.Span
}

type OverlayUI interface {
	ShowHint(text string)
	ShowListening(windowClass, hotkeyMode string)
	SetConnected(windowClass string)
	SetConnecting(attempt, max int)
	SetLoadingModel(modelName string)
	SetSubmitMode(enabled bool)
	SetListeningText(windowClass, text string)
	AnimateChunk(text string)
	ShowFinishing(body, shortcut string)
	SetFinishingPhase(label string)
	ExtendFinishingPhase(label string)
	SetFinishingText(body string)
	ShowSuccess(text string)
	ShowError(err error)
	ShowWarning(subtitle string)
	GrabEscape() <-chan struct{}
	UngrabEscape()
	SetLevel(level float64)
	Hide()
	Close()
}

type InjectorClient interface {
	CaptureTarget(ctx context.Context) (platform.Target, error)
	Insert(ctx context.Context, target platform.Target, text string) error
	InsertLive(ctx context.Context, target platform.Target, text string) error
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
		store:          securestore.New(),
	}
}

func (a *App) Run(ctx context.Context) error {
	if err := a.cfg.Validate(); err != nil {
		return err
	}
	sessionlog.Infof("starting vocis session")
	recorder.CleanupStale()

	if a.cfg.Transcription.Backend == config.BackendLemonade {
		// Lemonade runs locally with no auth.
		a.apiKey = ""
	} else {
		apiKey, err := a.store.APIKey()
		if err != nil {
			return err
		}
		a.apiKey = apiKey
	}

	a.recorder = recorder.New()
	a.transcribe = transcribe.New(a.apiKey, a.cfg.Transcription, a.cfg.Streaming)

	// For Lemonade, proactively check that both configured models are
	// resident and force-load anything that isn't. Avoids a 5-10s load
	// stall on the user's first dictation of the session (which would
	// otherwise manifest as "no transcript ever arrives" because the
	// load runs on the WS path while audio is already flowing).
	// Fire-and-forget — a failed warm never makes things worse.
	go transcribe.EnsureLemonadeModelsLoaded(ctx, a.cfg, a.transcribe)

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

func (a *App) handleStop(ctx context.Context) {
	a.mu.Lock()
	defer a.mu.Unlock()

	if a.transcribing || a.recording == nil {
		return
	}

	a.stopRecordingLocked(ctx)
}

func (a *App) reloadConfig() {
	cfg, path, err := config.Load()
	if err != nil {
		sessionlog.Warnf("config reload failed, keeping current: %v", err)
		return
	}
	a.cfg.Transcription = cfg.Transcription
	a.cfg.Recording = cfg.Recording
	a.cfg.Streaming = cfg.Streaming
	a.cfg.PostProcess = cfg.PostProcess
	a.cfg.LogWindowTitle = cfg.LogWindowTitle
	a.transcribe = transcribe.New(a.apiKey, a.cfg.Transcription, a.cfg.Streaming)
	sessionlog.Infof("config reloaded: %s", path)
}

func (a *App) startRecordingLocked(ctx context.Context) {
	a.reloadConfig()
	a.ducker.Duck()
	a.dismissCompletionOverlay = false
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
	// on the Lemonade backend before we open the WS for live audio. No-op
	// for OpenAI. Runs inline so the overlay clearly says "Loading X..."
	// while the user waits — but the mic is hot the whole time, so any
	// speech during the load is preserved as preroll.
	preflightStart := time.Now()
	if err := transcribe.EnsureTranscribeModelLoaded(spanCtx, a.cfg.Transcription, func(model string) {
		a.overlay.SetLoadingModel(model)
		recordingSpan.AddEvent("lemonade.model.loading",
			trace.WithAttributes(attribute.String("model", model)),
		)
	}); err != nil {
		_ = stopDrain()
		_ = session.Stop(ctx)
		session.Cleanup()
		a.ducker.Restore()
		telemetry.EndSpan(recordingSpan, err)
		sessionlog.Errorf("transcription model preflight: %v", err)
		a.overlay.ShowError(fmt.Errorf("model load failed: %w", err))
		return
	}
	preflightElapsed := time.Since(preflightStart)
	prerolled := stopDrain()
	if len(prerolled.chunks) > 0 {
		prerollMS := prerolled.samples * 1000 / a.cfg.Recording.SampleRate
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
	a.sessionCancel = cancel

	a.sequence++
	state := &recordingState{
		id:         a.sequence,
		startedAt:  time.Now(),
		session:    session,
		cancel:     cancel,
		target:     target,
		span:       recordingSpan,
		spanCtx:    spanCtx,
		submitMode: a.cfg.Insertion.AutoSubmit,
	}
	dictation, err := a.transcribe.StartDictation(recordCtx, transcribe.DictationOpts{
		SampleRate: a.cfg.Recording.SampleRate,
		Channels:   a.cfg.Recording.Channels,
		Samples:    wrappedSamples,
		Callbacks: transcribe.ConnectCallbacks{
			OnConnecting: func(attempt, max int) {
				a.overlay.SetConnecting(attempt, max)
				recordingSpan.AddEvent("overlay.connecting",
					trace.WithAttributes(
						attribute.Int("attempt", attempt),
						attribute.Int("max", max),
					),
				)
			},
			OnConnected: func() {
				a.overlay.SetConnected(target.WindowClass)
				recordingSpan.AddEvent("overlay.connected")
			},
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
	go a.consumeDictationEvents(recordCtx, state)
	go a.monitorRecordingLevel(ctx, state.id, state.session)

	// Pre-warm the post-processing model in the background while the user
	// is still talking. On Lemonade with max_models.llm=1, this triggers
	// the model swap eagerly so the real PP request after Finalize doesn't
	// pay the 5s+ load cost. No-op if PP is disabled.
	if a.cfg.PostProcess.Enabled && a.cfg.PostProcess.Model != "" {
		go a.transcribe.WarmPostProcess(ctx, a.cfg.PostProcess.Model)
	}

	if a.cfg.Recording.MaxDurationSeconds > 0 {
		go a.forceStopAfter(ctx, state.id, time.Duration(a.cfg.Recording.MaxDurationSeconds)*time.Second)
	}
}

func (a *App) stopRecordingLocked(ctx context.Context) {
	state := a.recording
	a.recording = nil
	a.transcribing = true
	a.overlay.ShowFinishing(state.displayText, a.shortcut)
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
	state.span.AddEvent("overlay.finishing",
		trace.WithAttributes(attribute.Bool("auto_stop", true)),
	)
	sessionlog.Warnf("auto-stopping recording after timeout")
	go a.finishRecording(ctx, state)
}

func (a *App) finishRecording(ctx context.Context, state *recordingState) {
	escapeCh := a.overlay.GrabEscape()
	defer a.overlay.UngrabEscape()
	defer func() {
		a.mu.Lock()
		a.transcribing = false
		a.mu.Unlock()
	}()
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
			a.hideCompletionOverlay()
			return
		}
		dictationErr = err
		sessionlog.Errorf("stop recording: %v", err)
		a.showCompletionError(err)
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
	transcribeCtx, transcribeCancel := context.WithCancel(spanCtx)
	defer transcribeCancel()

	a.mu.Lock()
	a.transcribeCancel = transcribeCancel
	a.mu.Unlock()
	defer func() {
		a.mu.Lock()
		a.transcribeCancel = nil
		a.mu.Unlock()
	}()

	finalizeStart := time.Now()
	transcribeCtx, transcribeSpan := telemetry.StartSpan(transcribeCtx, "vocis.transcribe.finalize")
	result, err := state.dictation.Finalize(transcribeCtx)
	finalizeDuration := time.Since(finalizeStart).Round(10 * time.Millisecond)
	telemetry.EndSpan(transcribeSpan, err)
	if err != nil {
		dictationErr = err
		if a.completionOverlayDismissed() {
			sessionlog.Infof("transcription cancelled by user elapsed=%s error=%v", finalizeDuration, err)
			return
		}
		sessionlog.Errorf("transcribe failed elapsed=%s error=%v", finalizeDuration, err)
		a.showCompletionError(err)
		return
	}
	sessionlog.Infof("finalization completed elapsed=%s", finalizeDuration)
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
	sessionlog.Infof("transcription complete chars=%d live=%d trailing=%d",
		len(text), len(state.liveText), len(trailing))

	if text == "" {
		sessionlog.Warnf("transcription was empty")
		a.showCompletionError(errors.New("transcription came back empty"))
		return
	}

	displayText := state.displayText
	if trailing != "" {
		if displayText != "" {
			displayText += "\n"
		}
		displayText += trailing
	}
	a.overlay.SetFinishingText(displayText)

	postProcessSkipped := false
	if a.cfg.PostProcess.Enabled {
		a.overlay.SetFinishingPhase(a.cfg.Overlay.Finishing.PPWait)
		state.span.AddEvent("overlay.phase.wait")
		ppSpanCtx, ppSpan := telemetry.StartSpan(spanCtx, "vocis.postprocess",
			attribute.Int("input.length", len(text)),
			attribute.String("model", a.cfg.PostProcess.Model),
		)

		ppCtx, ppCancel := context.WithCancel(ppSpanCtx)
		resultCh := make(chan transcribe.PostProcessResult, 1)
		go func() {
			resultCh <- a.transcribe.PostProcess(ppCtx, a.cfg.PostProcess, text, func() {
				a.overlay.ExtendFinishingPhase(a.cfg.Overlay.Finishing.PPStream)
			})
		}()

		var result transcribe.PostProcessResult
		select {
		case result = <-resultCh:
		case <-escapeCh:
			ppCancel()
			ppSpan.AddEvent("postprocess.cancelled_by_user")
			sessionlog.Infof("post-processing skipped by user (Escape)")
			result = transcribe.PostProcessResult{Text: text, Skipped: true}
		}
		ppCancel()

		ppSpan.SetAttributes(
			attribute.Int("output.length", len(result.Text)),
			attribute.Bool("skipped", result.Skipped),
		)
		telemetry.EndSpan(ppSpan, nil)
		text = result.Text
		postProcessSkipped = result.Skipped
	}

	insertCtx, insertSpan := telemetry.StartSpan(spanCtx, "vocis.inject",
		attribute.String("target.window_id", state.target.WindowID),
		attribute.String("target.window_class", state.target.WindowClass),
		attribute.String("target.kitty_window_id", state.target.KittyWindowID),
		attribute.Int("text.length", len(text)),
	)
	err = a.injector.Insert(insertCtx, state.target, text)
	telemetry.EndSpan(insertSpan, err)
	if err != nil {
		if errors.Is(err, platform.ErrTargetGone) {
			// Soft path: the original kitty tab/pane is gone but the
			// transcript is safely on the clipboard. Don't taint the
			// dictation span as failed — the transcription itself
			// succeeded; only delivery degraded.
			state.span.AddEvent("overlay.warning",
				trace.WithAttributes(attribute.String("reason", "target_gone")),
			)
			sessionlog.Warnf("target window gone — transcript on clipboard (%d chars)", len(text))
			a.overlay.ShowWarning(a.cfg.Overlay.Warning.TargetGone)
			return
		}
		dictationErr = err
		sessionlog.Errorf("insert transcript: %v", err)
		a.showCompletionError(err)
		return
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
	state.span.SetAttributes(attribute.Bool("submit_mode", state.submitMode))
	if postProcessSkipped {
		state.span.AddEvent("overlay.warning", trace.WithAttributes(attribute.String("reason", "postprocess_skipped")))
		a.overlay.ShowWarning(a.cfg.Overlay.Warning.PostprocessSkipped)
	} else {
		state.span.AddEvent("overlay.success")
		a.showCompletionSuccess(text)
	}
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
		return fmt.Sprintf("Press %s to start, press again to stop", shortcut)
	}
	return fmt.Sprintf("Hold %s, release to transcribe", shortcut)
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
	if a.sessionCancel != nil {
		a.sessionCancel()
	}
	a.transcribing = false
	a.overlay.ShowWarning(a.cfg.Overlay.Warning.Cancelled)
	sessionlog.Infof("transcription cancelled by user")
	// Note: span is ended by the finishRecording defer, which will see the cancelled context.
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
	if isNoSpeechError(err) {
		a.overlay.ShowWarning(a.cfg.Overlay.Warning.NoSpeech)
		return
	}
	a.overlay.ShowError(userFacingError(err))
}

// addOverlayEvent is a convenience for adding overlay state events to a span
// that may be nil (e.g. before the dictation span is created).
func addOverlayEvent(span trace.Span, name string, attrs ...attribute.KeyValue) {
	if span == nil {
		return
	}
	if len(attrs) > 0 {
		span.AddEvent(name, trace.WithAttributes(attrs...))
	} else {
		span.AddEvent(name)
	}
}

func isNoSpeechError(err error) bool {
	return errors.Is(err, transcribe.ErrInputAudioBufferCommitEmpty) ||
		strings.Contains(err.Error(), "transcription came back empty")
}


func userFacingError(err error) error {
	msg := err.Error()
	switch {
	case errors.Is(err, context.DeadlineExceeded):
		return errors.New("Timed out waiting for transcription")
	case strings.Contains(msg, "i/o timeout"):
		return errors.New("Could not connect to OpenAI (network timeout)")
	case strings.Contains(msg, "stream was not established"):
		return errors.New("Could not connect to OpenAI")
	default:
		return err
	}
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
	event transcribe.DictationEvent,
) error {
	switch event.Type {
	case transcribe.DictationEventPartial:
		if !a.cfg.Streaming.ShowPartialOverlay {
			return nil
		}
		// Live-subtitle mode: the partial replaces the previous in-flight
		// partial. Rendered preview = committed segments + current partial.
		state.currentPartial = strings.TrimSpace(event.Text)
		a.overlay.SetListeningText(state.target.WindowClass, renderPreview(state.displayText, state.currentPartial))
		return nil

	case transcribe.DictationEventSegment:
		text := strings.TrimSpace(event.Text)
		if text == "" {
			return nil
		}
		// The canonical turn replaces whatever partial was being shown.
		state.liveText += event.Text
		if state.displayText != "" {
			state.displayText += "\n"
		}
		state.displayText += text
		state.currentPartial = ""
		a.overlay.SetListeningText(state.target.WindowClass, state.displayText)
		sessionlog.Infof("stream segment accumulated: %d chars total", len(state.liveText))
		return nil

	default:
		return nil
	}
}

// renderPreview joins committed text with the in-flight partial. Partial
// goes on its own line because once it completes it becomes a new line —
// the visual position shouldn't jump between the two states.
func renderPreview(committed, partial string) string {
	if partial == "" {
		return committed
	}
	if committed == "" {
		return partial
	}
	return committed + "\n" + partial
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
