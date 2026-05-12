package x11

import (
	"fmt"
	"image"
	"image/color"
	"io"
	"strings"
	"sync"
	"time"

	"github.com/BurntSushi/xgb"
	"github.com/BurntSushi/xgb/xinerama"
	"github.com/BurntSushi/xgb/xproto"
	"github.com/BurntSushi/xgbutil"
	"github.com/BurntSushi/xgbutil/ewmh"
	"github.com/BurntSushi/xgbutil/xgraphics"
	"github.com/BurntSushi/xgbutil/xwindow"

	"vocis/internal/config"
	"vocis/internal/sessionlog"
	"vocis/internal/ui"
)

type Overlay struct {
	mu       sync.Mutex
	x        *xgbutil.XUtil
	win      *xwindow.Window
	renderer *ui.OverlayRenderer
	visible  bool
	state    viewState
	level    float64
	hide     *time.Timer

	animToken    uint64
	animating    bool
	liveBody     string
	wavePhase    float64
	partialToken uint64
	height       int
	targetHeight int
	resizeToken  uint64

	baseX      int
	baseY      int
	fadeToken  uint64
	fadeAlpha  float64
	fadeOffset int
	slidingIn  bool

	crossFadeT     float64
	crossPrevFrame *image.RGBA

	countdownReset  chan countdownPhase
	countdownExtend chan countdownPhase
	completedPhases []string // pre-formatted: "Wrapping up — done (2.3s)"

	escapeCh      chan struct{}
	escapeGrabbed bool
	escapeKeycode xproto.Keycode
	escapeConn    *xgb.Conn
}

type countdownPhase struct {
	label string
}

type viewState struct {
	title         string
	titleSuffix   string
	submitHint    bool
	subtitle      string
	body          string
	accent        color.RGBA
	reactiveWave  bool
	idleWave      bool
	heartbeatWave bool
}

func NewOverlay() (*Overlay, error) {
	// Suppress xgb's internal logger — it logs "Invalid event/error type: <nil>"
	// when connections are closed, which is expected and not useful.
	xgb.Logger.SetOutput(io.Discard)

	xu, err := xgbutil.NewConn()
	if err != nil {
		return nil, err
	}

	win, err := xwindow.Generate(xu)
	if err != nil {
		return nil, err
	}

	x, y := position(xu)
	mask := xproto.CwBackPixel | xproto.CwBorderPixel | xproto.CwOverrideRedirect
	win.Create(xu.RootWin(), x, y, ui.OverlayWidth, ui.OverlayHeight, int(mask), 0x101623, 0, 1)
	win.Map()
	win.Unmap()
	_ = ewmh.WmWindowOpacitySet(xu, win.Id, ui.OverlayOpacity)
	win.Stack(xproto.StackModeAbove)

	renderer := ui.NewOverlayRenderer()

	return &Overlay{
		x:         xu,
		win:       win,
		renderer:  renderer,
		height:    ui.OverlayHeight,
		baseX:     x,
		baseY:     y,
		fadeAlpha: 1,
		state: viewState{
			title:    ui.OverlayReadyTitle,
			subtitle: ui.OverlayReadySubtitle,
			body:     "",
			accent:   color.RGBA{R: 96, G: 165, B: 250, A: 255},
		},
	}, nil
}

func (o *Overlay) ShowHint(text string) {
	o.show(viewState{
		title:    ui.OverlayReadyTitle,
		subtitle: ui.OverlayReadySubtitle,
		body:     text,
		accent:   color.RGBA{R: 96, G: 165, B: 250, A: 255},
		idleWave: true,
	}, true)
}

func (o *Overlay) ShowListening(windowClass, hotkeyMode string) {
	body := ui.ListeningBody("")
	o.show(viewState{
		title:        ui.OverlayListeningTitle,
		titleSuffix:  " " + ui.OverlayListeningSuffix,
		subtitle:     ui.OverlayListeningConnecting,
		body:         body,
		accent:       color.RGBA{R: 34, G: 197, B: 94, A: 255},
		reactiveWave: true,
	}, false)
}

func (o *Overlay) SetConnected(windowClass string) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if !o.visible || o.state.title != ui.OverlayListeningTitle {
		return
	}
	o.state.subtitle = config.ExpandTemplate(ui.OverlayListeningConnected, map[string]string{
		"window": windowClass,
	})
	o.drawLocked()
}

func (o *Overlay) SetConnecting(attempt, max int) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if !o.visible || o.state.title != ui.OverlayListeningTitle {
		return
	}
	if attempt > 1 {
		o.state.subtitle = config.ExpandTemplate(ui.OverlayListeningReconnecting, map[string]string{
			"attempt": fmt.Sprintf("%d", attempt),
			"max":     fmt.Sprintf("%d", max),
		})
	} else {
		o.state.subtitle = ui.OverlayListeningConnecting
	}
	o.drawLocked()
}

// SetLoadingModel updates the Listening-view subtitle to indicate the
// transcription model is being force-loaded on the backend. Intended
// for the session-start preflight on Lemonade; no-op if the overlay
// isn't currently in the Listening state.
func (o *Overlay) SetLoadingModel(modelName string) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if !o.visible || o.state.title != ui.OverlayListeningTitle {
		return
	}
	o.state.subtitle = config.ExpandTemplate(ui.OverlayListeningLoadingModel, map[string]string{
		"model": modelName,
	})
	o.drawLocked()
}

func (o *Overlay) SetSubmitMode(enabled bool) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if !o.visible || o.state.title != ui.OverlayListeningTitle {
		return
	}
	o.state.titleSuffix = " " + ui.OverlayListeningSuffix
	o.state.submitHint = enabled
	o.drawLocked()
}

func (o *Overlay) SetListeningText(windowClass, text string) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if !o.visible || o.state.title != ui.OverlayListeningTitle {
		return
	}

	subtitle := config.ExpandTemplate(ui.OverlayListeningConnected, map[string]string{"window": windowClass})
	targetText := ui.NormalizeListeningText(text)
	body := ui.ListeningBody(targetText)
	o.liveBody = body
	currentText := ui.DisplayedListeningText(o.state.body)
	if o.state.subtitle == subtitle && currentText == targetText {
		return
	}

	o.state.subtitle = subtitle
	if o.animating {
		return
	}
	if ui.ShouldAnimatePartial(currentText, targetText) {
		o.partialToken++
		token := o.partialToken
		go o.animateListeningText(token, currentText, targetText)
		return
	}
	o.state.body = body
	o.drawLocked()
}

func (o *Overlay) AnimateChunk(text string) {
	text = strings.TrimSpace(strings.ReplaceAll(text, "\n", " "))
	if text == "" {
		return
	}

	o.mu.Lock()
	if !o.visible || o.state.title != ui.OverlayListeningTitle {
		o.mu.Unlock()
		return
	}

	o.animToken++
	token := o.animToken
	o.animating = true
	o.state.body = ""
	o.drawLocked()
	o.mu.Unlock()

	go o.animateChunk(token, ui.Shorten(text, o.renderer.BodyTextLimit()))
}

func (o *Overlay) ShowFinishing(body, shortcut string) {
	var suffix string
	if shortcut != "" {
		suffix = " " + config.ExpandTemplate(ui.OverlayFinishingCancelHint, map[string]string{
			"shortcut": shortcut,
		})
	}

	o.show(viewState{
		title:         ui.OverlayFinishingTitle,
		titleSuffix:   suffix,
		subtitle:      formatElapsed(ui.OverlayFinishingWrappingUp, 0),
		body:          body,
		accent:        color.RGBA{R: 96, G: 165, B: 250, A: 255},
		heartbeatWave: true,
	}, false)

	o.mu.Lock()
	o.completedPhases = nil
	o.countdownReset = make(chan countdownPhase, 1)
	o.countdownExtend = make(chan countdownPhase, 1)
	o.mu.Unlock()

	go o.animateElapsed(countdownPhase{label: ui.OverlayFinishingWrappingUp})
}

func (o *Overlay) SetFinishingPhase(label string) {
	o.mu.Lock()
	ch := o.countdownReset
	o.mu.Unlock()

	if ch != nil {
		select {
		case ch <- countdownPhase{label: label}:
		default:
		}
	}
}

// ExtendFinishingPhase transitions the current phase to a second sub-phase
// shown inline (e.g. "Wait · Stream... (10.0s)") without completing it.
func (o *Overlay) ExtendFinishingPhase(label string) {
	o.mu.Lock()
	ch := o.countdownExtend
	o.mu.Unlock()

	if ch != nil {
		select {
		case ch <- countdownPhase{label: label}:
		default:
		}
	}
}

func (o *Overlay) SetFinishingText(body string) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if !o.visible || o.state.title != ui.OverlayFinishingTitle {
		return
	}
	o.state.body = body
	o.drawLocked()
}

func (o *Overlay) buildSubtitle(activeLine string) string {
	var lines []string
	lines = append(lines, o.completedPhases...)
	lines = append(lines, activeLine)
	return strings.Join(lines, "\n")
}

func formatElapsed(label string, elapsed time.Duration) string {
	return fmt.Sprintf("%s... (%.1fs)", label, elapsed.Seconds())
}

func formatTwoPhaseElapsed(doneLabel, activeLabel string, elapsed time.Duration) string {
	return fmt.Sprintf("%s · %s... (%.1fs)", doneLabel, activeLabel, elapsed.Seconds())
}

// phaseDoneLine formats a completed phase with its final elapsed duration,
// e.g. "Wrapping up — done (2.3s)". Pushed onto completedPhases so the user
// can see how long each stage of finishing took.
func (o *Overlay) phaseDoneLine(label string, elapsed time.Duration) string {
	return fmt.Sprintf("%s — %s (%.1fs)", label, ui.OverlayFinishingPhaseDone, elapsed.Seconds())
}

func (o *Overlay) animateElapsed(phase countdownPhase) {
	start := time.Now()
	label := phase.label
	doneLabel := "" // set when extended; makes the line two-phase
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	o.mu.Lock()
	resetCh := o.countdownReset
	extendCh := o.countdownExtend
	o.mu.Unlock()

	activeElapsed := func(elapsed time.Duration) string {
		if doneLabel != "" {
			return formatTwoPhaseElapsed(doneLabel, label, elapsed)
		}
		return formatElapsed(label, elapsed)
	}

	for {
		select {
		case newPhase := <-resetCh:
			o.mu.Lock()
			completedLabel := label
			if doneLabel != "" {
				completedLabel = doneLabel + " · " + label
			}
			o.completedPhases = append(o.completedPhases, o.phaseDoneLine(completedLabel, time.Since(start)))
			label = newPhase.label
			doneLabel = ""
			start = time.Now()
			o.state.subtitle = o.buildSubtitle(formatElapsed(label, 0))
			o.drawLocked()
			o.mu.Unlock()
		case ext := <-extendCh:
			o.mu.Lock()
			doneLabel = label
			label = ext.label
			start = time.Now()
			o.state.subtitle = o.buildSubtitle(activeElapsed(0))
			o.drawLocked()
			o.mu.Unlock()
		case <-ticker.C:
			o.mu.Lock()
			if !o.visible || o.state.title != ui.OverlayFinishingTitle {
				o.mu.Unlock()
				return
			}
			o.state.subtitle = o.buildSubtitle(activeElapsed(time.Since(start)))
			o.drawLocked()
			o.mu.Unlock()
		}
	}
}

func (o *Overlay) ShowSuccess(text string) {
	o.show(viewState{
		title:    ui.OverlaySuccessTitle,
		subtitle: ui.OverlaySuccessSubtitle,
		body:     ui.Shorten(strings.ReplaceAll(text, "\n", " "), o.renderer.BodyTextLimit()),
		accent:   color.RGBA{R: 56, G: 189, B: 248, A: 255},
	}, true)
}

func (o *Overlay) ShowWarning(text string) {
	o.show(viewState{
		title:  ui.OverlayWarningTitle,
		body:   text,
		accent: color.RGBA{R: 251, G: 191, B: 36, A: 255},
	}, true)
}

func (o *Overlay) ShowError(err error) {
	o.show(viewState{
		title:    ui.OverlayErrorTitle,
		subtitle: ui.Shorten(err.Error(), o.renderer.SubtitleTextLimit()),
		accent:   color.RGBA{R: 248, G: 113, B: 113, A: 255},
	}, true)
}

func (o *Overlay) GrabEscape() <-chan struct{} {
	o.mu.Lock()
	defer o.mu.Unlock()

	if o.escapeGrabbed {
		return o.escapeCh
	}

	// Use a separate X connection so WaitForEvent doesn't block the overlay.
	conn, err := xgb.NewConn()
	if err != nil {
		sessionlog.Warnf("failed to open X connection for Escape grab: %v", err)
		o.escapeCh = make(chan struct{}, 1)
		return o.escapeCh
	}

	setup := xproto.Setup(conn)
	root := setup.DefaultScreen(conn).Root
	mapping, err := xproto.GetKeyboardMapping(conn,
		setup.MinKeycode,
		byte(setup.MaxKeycode-setup.MinKeycode+1),
	).Reply()
	if err != nil {
		sessionlog.Warnf("failed to get keyboard mapping: %v", err)
		conn.Close()
		o.escapeCh = make(chan struct{}, 1)
		return o.escapeCh
	}
	cols := int(mapping.KeysymsPerKeycode)
	var escapeKeycode xproto.Keycode
	for i := 0; i < len(mapping.Keysyms)/cols; i++ {
		if mapping.Keysyms[i*cols] == 0xff1b { // XK_Escape
			escapeKeycode = xproto.Keycode(int(setup.MinKeycode) + i)
			break
		}
	}
	if escapeKeycode == 0 {
		sessionlog.Warnf("could not find Escape keycode")
		conn.Close()
		o.escapeCh = make(chan struct{}, 1)
		return o.escapeCh
	}

	err = xproto.GrabKeyChecked(conn, true, root,
		xproto.ModMaskAny, escapeKeycode,
		xproto.GrabModeAsync, xproto.GrabModeAsync,
	).Check()
	if err != nil {
		sessionlog.Warnf("failed to grab Escape: %v", err)
		conn.Close()
		o.escapeCh = make(chan struct{}, 1)
		return o.escapeCh
	}

	o.escapeCh = make(chan struct{}, 1)
	o.escapeKeycode = escapeKeycode
	o.escapeConn = conn
	o.escapeGrabbed = true
	go o.escapeEventLoop(conn)
	return o.escapeCh
}

func (o *Overlay) escapeEventLoop(conn *xgb.Conn) {
	for {
		ev, err := conn.WaitForEvent()
		if ev == nil {
			return
		}
		if err != nil {
			return
		}
		if _, ok := ev.(xproto.KeyPressEvent); ok {
			select {
			case o.escapeCh <- struct{}{}:
			default:
			}
		}
	}
}

func (o *Overlay) UngrabEscape() {
	o.mu.Lock()
	defer o.mu.Unlock()

	if !o.escapeGrabbed {
		return
	}
	_ = xproto.UngrabKeyChecked(o.escapeConn, o.escapeKeycode,
		xproto.Setup(o.escapeConn).DefaultScreen(o.escapeConn).Root,
		xproto.ModMaskAny,
	).Check()
	o.escapeConn.Close()
	o.escapeConn = nil
	o.escapeGrabbed = false
}

func (o *Overlay) Close() {
	o.mu.Lock()
	defer o.mu.Unlock()

	o.hideImmediate()
	if o.win != nil {
		o.win.Destroy()
	}
}

func (o *Overlay) Hide() {
	o.fadeOut()
}

func (o *Overlay) hideImmediate() {
	o.animToken++
	o.animating = false
	o.partialToken++
	o.fadeToken++
	o.slidingIn = false
	if o.hide != nil {
		o.hide.Stop()
		o.hide = nil
	}
	o.visible = false
	o.fadeAlpha = 0
	o.fadeOffset = fadeSlideDistance
	if o.win != nil {
		o.win.Unmap()
	}
}

func (o *Overlay) show(state viewState, autoHide bool) {
	o.mu.Lock()

	o.fadeToken++
	if o.hide != nil {
		o.hide.Stop()
		o.hide = nil
	}

	if o.visible {
		if o.slidingIn {
			// Still sliding in — just update state, let the slide continue
			o.fadeToken--
			o.applyStateLocked(state)
			o.drawLocked()
			if autoHide {
				duration := time.Duration(ui.OverlayAutoHideMillis) * time.Millisecond
				o.hide = time.AfterFunc(duration, func() {
					o.fadeOut()
				})
			}
			o.mu.Unlock()
			return
		}
		o.fadeAlpha = ui.OverlayOpacity
		o.fadeOffset = 0
		_ = ewmh.WmWindowOpacitySet(o.x, o.win.Id, ui.OverlayOpacity)
		o.win.Move(o.baseX, o.baseY)
		token := o.fadeToken
		o.mu.Unlock()
		go o.animateCrossFade(token, state, autoHide)
		return
	}

	o.repositionLocked()
	o.applyStateLocked(state)
	o.fadeAlpha = 0
	o.fadeOffset = fadeSlideDistance
	o.win.Move(o.baseX, o.baseY-fadeSlideDistance)
	_ = ewmh.WmWindowOpacitySet(o.x, o.win.Id, 0)
	o.drawLocked()

	o.visible = true
	o.slidingIn = true
	o.win.Map()
	o.win.Stack(xproto.StackModeAbove)
	fadeToken := o.fadeToken

	if autoHide {
		duration := time.Duration(ui.OverlayAutoHideMillis) * time.Millisecond
		o.hide = time.AfterFunc(duration, func() {
			o.fadeOut()
		})
	}
	if state.idleWave {
		go o.animateIdleWave(o.animToken)
	}
	if state.heartbeatWave {
		go o.animateHeartbeat(o.animToken)
	}
	o.mu.Unlock()

	go o.animateFadeIn(fadeToken)
}

func (o *Overlay) neededHeight(state viewState) int {
	return o.renderer.NeededHeight(state.body)
}

// frameLocked packages the overlay's current state for the renderer.
func (o *Overlay) frameLocked() ui.Frame {
	return ui.Frame{
		State: ui.State{
			Title:         o.state.title,
			TitleSuffix:   o.state.titleSuffix,
			SubmitHint:    o.state.submitHint,
			Subtitle:      o.state.subtitle,
			Body:          o.state.body,
			Accent:        o.state.accent,
			ReactiveWave:  o.state.reactiveWave,
			IdleWave:      o.state.idleWave,
			HeartbeatWave: o.state.heartbeatWave,
		},
		Level:      o.level,
		WavePhase:  o.wavePhase,
		Height:     o.height,
		CrossFadeT: o.crossFadeT,
		CrossPrev:  o.crossPrevFrame,
	}
}

func (o *Overlay) applyStateLocked(state viewState) {
	o.animToken++
	o.animating = false
	o.partialToken++
	o.state = state
	o.level = 0
	o.wavePhase = 0

	needed := o.neededHeight(state)
	o.height = needed
	o.targetHeight = needed
	o.resizeToken++
	o.win.Resize(ui.OverlayWidth, needed)
	if state.title == ui.OverlayListeningTitle {
		o.liveBody = state.body
	} else {
		o.liveBody = ""
	}
}

func (o *Overlay) animateCrossFade(token uint64, state viewState, autoHide bool) {
	steps := int(crossFadeDuration / fadeInterval)
	ticker := time.NewTicker(fadeInterval)
	defer ticker.Stop()

	// Capture previous frame and apply new state
	o.mu.Lock()
	if token != o.fadeToken {
		o.mu.Unlock()
		return
	}
	o.crossPrevFrame = o.captureFrameLocked()
	o.crossFadeT = 0
	o.applyStateLocked(state)

	if o.hide != nil {
		o.hide.Stop()
		o.hide = nil
	}
	if autoHide {
		duration := time.Duration(ui.OverlayAutoHideMillis) * time.Millisecond
		o.hide = time.AfterFunc(duration, func() {
			o.fadeOut()
		})
	}
	animToken := o.animToken
	o.drawLocked()
	o.mu.Unlock()

	// Animate blend from old to new
	for i := 1; i <= steps; i++ {
		<-ticker.C
		o.mu.Lock()
		if token != o.fadeToken {
			o.crossPrevFrame = nil
			o.mu.Unlock()
			return
		}
		o.crossFadeT = float64(i) / float64(steps)
		o.drawLocked()
		o.mu.Unlock()
	}

	// Clean up
	o.mu.Lock()
	if token == o.fadeToken {
		o.crossPrevFrame = nil
		o.crossFadeT = 1
		if state.idleWave {
			go o.animateIdleWave(animToken)
		}
		if state.heartbeatWave {
			go o.animateHeartbeat(animToken)
		}
	}
	o.mu.Unlock()
}

func (o *Overlay) SetLevel(level float64) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if !o.visible || !o.state.reactiveWave {
		return
	}

	if level < 0 {
		level = 0
	}
	if level > 1 {
		level = 1
	}
	o.level = level
	o.drawLocked()
}

const (
	fadeSlideDistance = 28
	fadeDuration      = 320 * time.Millisecond
	crossFadeDuration = 80 * time.Millisecond
	fadeInterval      = 12 * time.Millisecond
)

func (o *Overlay) drawLocked() {
	needed := o.renderer.NeededHeight(o.state.body)
	if needed != o.targetHeight {
		o.targetHeight = needed
		if o.height != needed {
			o.resizeToken++
			go o.animateResize(o.resizeToken)
		}
	}

	img := o.renderer.Render(o.frameLocked())

	ximg := xgraphics.NewConvert(o.x, img)
	ximg.XSurfaceSet(o.win.Id)
	ximg.XDraw()
	ximg.XPaint(o.win.Id)
	ximg.Destroy()
}

func (o *Overlay) animateChunk(token uint64, text string) {
	runes := []rune(text)
	for i := range runes {
		time.Sleep(16 * time.Millisecond)

		o.mu.Lock()
		if token != o.animToken || !o.visible || o.state.title != ui.OverlayListeningTitle {
			o.mu.Unlock()
			return
		}
		o.state.body = string(runes[:i+1])
		o.drawLocked()
		o.mu.Unlock()
	}

	time.Sleep(260 * time.Millisecond)

	o.mu.Lock()
	defer o.mu.Unlock()

	if token != o.animToken || !o.visible || o.state.title != ui.OverlayListeningTitle {
		return
	}
	o.animating = false
	o.state.body = o.liveBody
	o.drawLocked()
}

func (o *Overlay) animateListeningText(token uint64, current, target string) {
	targetRunes := []rune(target)
	currentLen := len([]rune(current))
	if currentLen > len(targetRunes) {
		currentLen = 0
	}

	for currentLen < len(targetRunes) {
		next := ui.NextWordBoundary(targetRunes, currentLen)
		time.Sleep(28 * time.Millisecond)

		o.mu.Lock()
		if token != o.partialToken || o.animating || !o.visible || o.state.title != ui.OverlayListeningTitle {
			o.mu.Unlock()
			return
		}
		o.state.body = ui.ListeningBody(string(targetRunes[:next]))
		o.drawLocked()
		o.mu.Unlock()

		currentLen = next
	}
}

func (o *Overlay) captureFrameLocked() *image.RGBA {
	f := o.frameLocked()
	f.CrossPrev = nil
	f.CrossFadeT = 0
	return o.renderer.Render(f)
}

const resizeStep = 4

func (o *Overlay) animateResize(token uint64) {
	ticker := time.NewTicker(12 * time.Millisecond)
	defer ticker.Stop()

	for range ticker.C {
		o.mu.Lock()
		if token != o.resizeToken || !o.visible {
			o.mu.Unlock()
			return
		}
		target := o.targetHeight
		if o.height == target {
			o.mu.Unlock()
			return
		}
		if o.height < target {
			o.height += resizeStep
			if o.height > target {
				o.height = target
			}
		} else {
			o.height -= resizeStep
			if o.height < target {
				o.height = target
			}
		}
		o.win.Resize(ui.OverlayWidth, o.height)
		o.drawLocked()
		o.mu.Unlock()
	}
}

func (o *Overlay) animateIdleWave(token uint64) {
	ticker := time.NewTicker(45 * time.Millisecond)
	defer ticker.Stop()

	for range ticker.C {
		o.mu.Lock()
		if token != o.animToken || !o.visible || !o.state.idleWave {
			o.mu.Unlock()
			return
		}
		o.wavePhase += 0.28
		o.drawLocked()
		o.mu.Unlock()
	}
}

func (o *Overlay) animateHeartbeat(token uint64) {
	ticker := time.NewTicker(30 * time.Millisecond)
	defer ticker.Stop()

	for range ticker.C {
		o.mu.Lock()
		if token != o.animToken || !o.visible || !o.state.heartbeatWave {
			o.mu.Unlock()
			return
		}
		o.wavePhase += 0.12
		o.drawLocked()
		o.mu.Unlock()
	}
}

func (o *Overlay) fadeOut() {
	o.mu.Lock()
	if !o.visible {
		o.mu.Unlock()
		return
	}
	o.animToken++
	o.animating = false
	o.partialToken++
	if o.hide != nil {
		o.hide.Stop()
		o.hide = nil
	}
	o.fadeToken++
	token := o.fadeToken
	o.mu.Unlock()

	o.animateFadeOut(token)
}

func (o *Overlay) animateFadeIn(token uint64) {
	steps := int(fadeDuration / fadeInterval)
	ticker := time.NewTicker(fadeInterval)
	defer ticker.Stop()

	for i := 1; i <= steps; i++ {
		<-ticker.C
		o.mu.Lock()
		if token != o.fadeToken || !o.visible {
			o.mu.Unlock()
			return
		}
		linear := float64(i) / float64(steps)
		eased := ui.EaseOutCubic(linear)
		o.fadeAlpha = linear * ui.OverlayOpacity
		o.fadeOffset = int(float64(fadeSlideDistance) * (1 - eased))
		_ = ewmh.WmWindowOpacitySet(o.x, o.win.Id, o.fadeAlpha)
		o.win.Move(o.baseX, o.baseY-o.fadeOffset)
		o.mu.Unlock()
	}

	o.mu.Lock()
	o.fadeAlpha = ui.OverlayOpacity
	o.fadeOffset = 0
	o.slidingIn = false
	_ = ewmh.WmWindowOpacitySet(o.x, o.win.Id, ui.OverlayOpacity)
	o.win.Move(o.baseX, o.baseY)
	o.mu.Unlock()
}

func (o *Overlay) animateFadeOut(token uint64) {
	steps := int(fadeDuration / fadeInterval)
	ticker := time.NewTicker(fadeInterval)
	defer ticker.Stop()

	for i := 1; i <= steps; i++ {
		<-ticker.C
		o.mu.Lock()
		if token != o.fadeToken {
			o.mu.Unlock()
			return
		}
		linear := float64(i) / float64(steps)
		eased := ui.EaseInCubic(linear)
		o.fadeAlpha = ui.OverlayOpacity * (1 - linear)
		o.fadeOffset = int(float64(fadeSlideDistance) * eased)
		_ = ewmh.WmWindowOpacitySet(o.x, o.win.Id, o.fadeAlpha)
		o.win.Move(o.baseX, o.baseY-o.fadeOffset)
		o.mu.Unlock()
	}

	o.mu.Lock()
	o.visible = false
	o.fadeAlpha = 0
	o.fadeOffset = fadeSlideDistance
	if o.win != nil {
		o.win.Unmap()
	}
	o.mu.Unlock()
}

func (o *Overlay) repositionLocked() {
	x, y := position(o.x)
	o.baseX = x
	o.baseY = y
}

func position(xu *xgbutil.XUtil) (int, int) {
	monX, monY, monW, _ := activeMonitor(xu)
	x := monX + monW/2 - ui.OverlayWidth/2
	if x < 0 {
		x = 0
	}
	y := monY + ui.OverlayMarginTop
	if y < 0 {
		y = 0
	}
	return x, y
}

func activeMonitor(xu *xgbutil.XUtil) (x, y, w, h int) {
	screen := xu.Screen()
	fallbackW := int(screen.WidthInPixels)
	fallbackH := int(screen.HeightInPixels)

	conn := xu.Conn()
	if err := xinerama.Init(conn); err != nil {
		return 0, 0, fallbackW, fallbackH
	}
	reply, err := xinerama.QueryScreens(conn).Reply()
	if err != nil || len(reply.ScreenInfo) == 0 {
		return 0, 0, fallbackW, fallbackH
	}

	ptrReply, err := xproto.QueryPointer(conn, xu.RootWin()).Reply()
	if err != nil {
		info := reply.ScreenInfo[0]
		return int(info.XOrg), int(info.YOrg), int(info.Width), int(info.Height)
	}
	px, py := int(ptrReply.RootX), int(ptrReply.RootY)

	for _, info := range reply.ScreenInfo {
		ix, iy := int(info.XOrg), int(info.YOrg)
		iw, ih := int(info.Width), int(info.Height)
		if px >= ix && px < ix+iw && py >= iy && py < iy+ih {
			return ix, iy, iw, ih
		}
	}

	info := reply.ScreenInfo[0]
	return int(info.XOrg), int(info.YOrg), int(info.Width), int(info.Height)
}
