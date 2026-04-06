package x11

import (
	"fmt"
	"image"
	"io"
	"image/color"
	"image/draw"
	"math"
	"os"
	"os/exec"
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
	"golang.org/x/image/font"
	"golang.org/x/image/font/basicfont"
	"golang.org/x/image/font/opentype"

	"vocis/internal/config"
	"vocis/internal/sessionlog"
	"vocis/internal/ui"
)

type Overlay struct {
	cfg config.OverlayConfig

	mu      sync.Mutex
	x       *xgbutil.XUtil
	win     *xwindow.Window
	visible bool
	state   viewState
	level   float64
	hide    *time.Timer

	animToken    uint64
	animating    bool
	liveBody     string
	wavePhase    float64
	partialToken uint64
	height       int
	targetHeight int
	resizeToken  uint64
	face         font.Face
	smallFace      font.Face
	glyphWidth     int
	smallGlyphWidth int

	baseX      int
	baseY      int
	fadeToken  uint64
	fadeAlpha  float64
	fadeOffset int
	slidingIn  bool

	crossFadeT     float64
	crossPrevFrame *image.RGBA

	countdownReset  chan countdownPhase
	completedPhases []string

	escapeCh      chan struct{}
	escapeGrabbed bool
	escapeKeycode xproto.Keycode
	escapeConn    *xgb.Conn
	escapeDone    chan struct{}
}

type countdownPhase struct {
	label   string
	timeout time.Duration
}

type viewState struct {
	title        string
	titleSuffix  string
	submitHint   bool
	subtitle     string
	body         string
	accent       color.RGBA
	reactiveWave   bool
	idleWave       bool
	heartbeatWave  bool
}

func NewOverlay(cfg config.OverlayConfig) (*Overlay, error) {
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

	x, y := position(xu, cfg)
	mask := xproto.CwBackPixel | xproto.CwBorderPixel | xproto.CwOverrideRedirect
	win.Create(xu.RootWin(), x, y, cfg.Width, cfg.Height, int(mask), 0x101623, 0, 1)
	win.Map()
	win.Unmap()
	_ = ewmh.WmWindowOpacitySet(xu, win.Id, cfg.Opacity)
	win.Stack(xproto.StackModeAbove)

	fontSize := cfg.FontSize
	if fontSize <= 0 {
		fontSize = 13
	}
	face, glyphW := loadFont(cfg.Font, fontSize)
	smallFace, smallGlyphW := loadFont(cfg.Font, fontSize-2)

	return &Overlay{
		cfg:        cfg,
		x:          xu,
		win:        win,
		height:     cfg.Height,
		face:       face,
		smallFace:       smallFace,
		smallGlyphWidth: smallGlyphW,
		glyphWidth: glyphW,
		baseX:      x,
		baseY:      y,
		fadeAlpha:   1,
		state: viewState{
			title:    cfg.Ready.Title,
			subtitle: cfg.Ready.Subtitle,
			body:     "",
			accent:   color.RGBA{R: 96, G: 165, B: 250, A: 255},
		},
	}, nil
}

func (o *Overlay) ShowHint(text string) {
	o.show(viewState{
		title:    o.cfg.Ready.Title,
		subtitle: o.cfg.Ready.Subtitle,
		body:     text,
		accent:   color.RGBA{R: 96, G: 165, B: 250, A: 255},
		idleWave: true,
	}, true)
}

func (o *Overlay) ShowListening(windowClass, hotkeyMode string) {
	body := ui.ListeningBody("")
	o.show(viewState{
		title:        o.cfg.Listening.Title,
		titleSuffix:  " " + o.cfg.Listening.Suffix,
		subtitle:     o.cfg.Listening.Connecting,
		body:         body,
		accent:       color.RGBA{R: 34, G: 197, B: 94, A: 255},
		reactiveWave: true,
	}, false)
}

func (o *Overlay) SetConnected(windowClass string) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if !o.visible || o.state.title != o.cfg.Listening.Title {
		return
	}
	o.state.subtitle = config.ExpandTemplate(o.cfg.Listening.Connected, map[string]string{
		"window": windowClass,
	})
	o.drawLocked()
}

func (o *Overlay) SetConnecting(attempt, max int) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if !o.visible || o.state.title != o.cfg.Listening.Title {
		return
	}
	if attempt > 1 {
		o.state.subtitle = config.ExpandTemplate(o.cfg.Listening.Reconnecting, map[string]string{
			"attempt": fmt.Sprintf("%d", attempt),
			"max":     fmt.Sprintf("%d", max),
		})
	} else {
		o.state.subtitle = o.cfg.Listening.Connecting
	}
	o.drawLocked()
}

func (o *Overlay) SetSubmitMode(enabled bool) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if !o.visible || o.state.title != o.cfg.Listening.Title {
		return
	}
	o.state.titleSuffix = " " + o.cfg.Listening.Suffix
	o.state.submitHint = enabled
	o.drawLocked()
}

func (o *Overlay) SetListeningText(windowClass, text string) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if !o.visible || o.state.title != o.cfg.Listening.Title {
		return
	}

	subtitle := config.ExpandTemplate(o.cfg.Listening.Connected, map[string]string{"window": windowClass})
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
	if !o.visible || o.state.title != o.cfg.Listening.Title {
		o.mu.Unlock()
		return
	}

	o.animToken++
	token := o.animToken
	o.animating = true
	o.state.body = ""
	o.drawLocked()
	o.mu.Unlock()

	go o.animateChunk(token, ui.Shorten(text, o.bodyTextLimit()))
}

func (o *Overlay) ShowFinishing(body, shortcut string, timeout time.Duration) {
	var suffix string
	if shortcut != "" {
		suffix = " " + config.ExpandTemplate(o.cfg.Finishing.CancelHint, map[string]string{
			"shortcut": shortcut,
		})
	}

	o.show(viewState{
		title:         o.cfg.Finishing.Title,
		titleSuffix:   suffix,
		subtitle:      formatCountdown(o.cfg.Finishing.WrappingUp, timeout),
		body:          body,
		accent:        color.RGBA{R: 96, G: 165, B: 250, A: 255},
		heartbeatWave: true,
	}, false)

	o.mu.Lock()
	o.completedPhases = nil
	o.countdownReset = make(chan countdownPhase, 1)
	o.mu.Unlock()

	go o.animateCountdown(countdownPhase{label: o.cfg.Finishing.WrappingUp, timeout: timeout})
}

func (o *Overlay) SetFinishingPhase(label string, timeout time.Duration) {
	o.mu.Lock()
	ch := o.countdownReset
	o.mu.Unlock()

	if ch != nil {
		select {
		case ch <- countdownPhase{label: label, timeout: timeout}:
		default:
		}
	}
}

func (o *Overlay) SetFinishingText(body string) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if !o.visible || o.state.title != o.cfg.Finishing.Title {
		return
	}
	o.state.body = body
	o.drawLocked()
}

func (o *Overlay) buildSubtitle(activeLine string) string {
	var lines []string
	for _, done := range o.completedPhases {
		lines = append(lines, done+" — "+o.cfg.Finishing.PhaseDone)
	}
	lines = append(lines, activeLine)
	return strings.Join(lines, "\n")
}

func formatCountdown(label string, remaining time.Duration) string {
	if remaining <= 0 {
		return label + "..."
	}
	return fmt.Sprintf("%s... (%.1fs)", label, remaining.Seconds())
}

func (o *Overlay) animateCountdown(phase countdownPhase) {
	deadline := time.Now().Add(phase.timeout)
	label := phase.label
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()

	o.mu.Lock()
	resetCh := o.countdownReset
	o.mu.Unlock()

	for {
		select {
		case newPhase := <-resetCh:
			o.mu.Lock()
			o.completedPhases = append(o.completedPhases, label)
			label = newPhase.label
			deadline = time.Now().Add(newPhase.timeout)
			o.state.subtitle = o.buildSubtitle(formatCountdown(label, newPhase.timeout))
			o.drawLocked()
			o.mu.Unlock()
		case <-ticker.C:
			o.mu.Lock()
			if !o.visible || o.state.title != o.cfg.Finishing.Title {
				o.mu.Unlock()
				return
			}
			remaining := time.Until(deadline)
			if remaining <= 0 {
				o.state.subtitle = o.buildSubtitle(config.ExpandTemplate(o.cfg.Finishing.TimedOut, map[string]string{"phase": label}))
				o.drawLocked()
				o.mu.Unlock()
				return
			}
			o.state.subtitle = o.buildSubtitle(formatCountdown(label, remaining))
			o.drawLocked()
			o.mu.Unlock()
		}
	}
}

func (o *Overlay) ShowSuccess(text string) {
	o.show(viewState{
		title:    o.cfg.Success.Title,
		subtitle: o.cfg.Success.Subtitle,
		body:     ui.Shorten(strings.ReplaceAll(text, "\n", " "), o.bodyTextLimit()),
		accent:   color.RGBA{R: 56, G: 189, B: 248, A: 255},
	}, true)
}

func (o *Overlay) ShowWarning(subtitle string) {
	o.show(viewState{
		title:    o.cfg.Warning.Title,
		subtitle: subtitle,
		accent:   color.RGBA{R: 251, G: 191, B: 36, A: 255},
	}, true)
}

func (o *Overlay) ShowError(err error) {
	o.show(viewState{
		title:    o.cfg.Error.Title,
		subtitle: ui.Shorten(err.Error(), o.subtitleTextLimit()),
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
	o.escapeDone = make(chan struct{})
	o.escapeKeycode = escapeKeycode
	o.escapeConn = conn
	o.escapeGrabbed = true
	go o.escapeEventLoop(conn)
	return o.escapeCh
}

func (o *Overlay) escapeEventLoop(conn *xgb.Conn) {
	defer close(o.escapeDone)
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
				duration := time.Duration(o.cfg.AutoHideMillis) * time.Millisecond
				o.hide = time.AfterFunc(duration, func() {
					o.fadeOut()
				})
			}
			o.mu.Unlock()
			return
		}
		o.fadeAlpha = o.cfg.Opacity
		o.fadeOffset = 0
		_ = ewmh.WmWindowOpacitySet(o.x, o.win.Id, o.cfg.Opacity)
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
		duration := time.Duration(o.cfg.AutoHideMillis) * time.Millisecond
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
	bodyLines := ui.WrapLines(state.body, o.bodyTextLimit())
	needed := o.cfg.Height
	if len(bodyLines) > 1 {
		needed = bodyStartY + len(bodyLines)*lineHeight + bodyPadBot
	}
	if needed < o.cfg.Height {
		needed = o.cfg.Height
	}
	return needed
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
	o.win.Resize(o.cfg.Width, needed)
	if state.title == o.cfg.Listening.Title {
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
		duration := time.Duration(o.cfg.AutoHideMillis) * time.Millisecond
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
	bodyStartY        = 98
	lineHeight        = 16
	bodyPadBot        = 12
	fadeSlideDistance  = 28
	fadeDuration      = 320 * time.Millisecond
	crossFadeDuration = 80 * time.Millisecond
	fadeInterval      = 12 * time.Millisecond
)

func (o *Overlay) drawLocked() {
	charsPerLine := o.bodyTextLimit()
	bodyLines := ui.WrapLines(o.state.body, charsPerLine)

	needed := o.cfg.Height
	if len(bodyLines) > 1 {
		needed = bodyStartY + len(bodyLines)*lineHeight + bodyPadBot
	}
	if needed < o.cfg.Height {
		needed = o.cfg.Height
	}
	if needed != o.targetHeight {
		o.targetHeight = needed
		if o.height != needed {
			o.resizeToken++
			go o.animateResize(o.resizeToken)
		}
	}

	img := image.NewRGBA(image.Rect(0, 0, o.cfg.Width, o.height))
	bg := color.RGBA{R: 12, G: 18, B: 31, A: 255}
	draw.Draw(img, img.Bounds(), &image.Uniform{C: bg}, image.Point{}, draw.Src)

	ui.DrawRect(img, image.Rect(0, 0, img.Bounds().Dx(), 6), o.state.accent)
	ui.WriteText(img, o.cfg.Width-len([]rune(o.cfg.Branding))*o.glyphWidth-12, 24, o.cfg.Branding, color.RGBA{R: 148, G: 163, B: 184, A: 255}, o.smallFace)
	ui.DrawRect(img, image.Rect(20, 22, 20+96, 24), color.RGBA{R: 24, G: 38, B: 65, A: 255})
	ui.DrawBars(
		img,
		image.Rect(26, 42, 132, 98),
		o.state.accent,
		o.level,
		o.state.reactiveWave,
		o.state.idleWave,
		o.state.heartbeatWave,
		o.wavePhase,
	)

	ui.WriteText(img, 150, 36, o.state.title, o.state.accent, o.face)
	if o.state.titleSuffix != "" {
		suffixX := 150 + len([]rune(o.state.title))*o.glyphWidth
		ui.WriteText(img, suffixX, 36, o.state.titleSuffix, color.RGBA{R: 226, G: 232, B: 240, A: 255}, o.face)
		if o.state.submitHint {
			hintX := suffixX + len([]rune(o.state.titleSuffix))*o.glyphWidth
			pulse := 0.5 + 0.5*math.Sin(o.wavePhase*3)
			alpha := uint8(140 + int(pulse*115))
			ui.WriteText(img, hintX, 36, " "+o.cfg.Listening.SubmitHint, color.RGBA{R: 251, G: 191, B: 36, A: alpha}, o.face)
		}
	}
	subtitleColor := color.RGBA{R: 226, G: 232, B: 240, A: 255}
	for i, line := range strings.Split(o.state.subtitle, "\n") {
		ui.WriteText(img, 150, 62+i*lineHeight, line, subtitleColor, o.face)
	}

	bodyColor := color.RGBA{R: 148, G: 163, B: 184, A: 255}
	for i, line := range bodyLines {
		ui.WriteText(img, 150, bodyStartY+i*lineHeight, line, bodyColor, o.face)
	}

	if o.crossPrevFrame != nil && o.crossFadeT < 1 {
		ui.BlendFrames(img, o.crossPrevFrame, 1-o.crossFadeT)
	}

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
		if token != o.animToken || !o.visible || o.state.title != o.cfg.Listening.Title {
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

	if token != o.animToken || !o.visible || o.state.title != o.cfg.Listening.Title {
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
		if token != o.partialToken || o.animating || !o.visible || o.state.title != o.cfg.Listening.Title {
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
	img := image.NewRGBA(image.Rect(0, 0, o.cfg.Width, o.height))
	bg := color.RGBA{R: 12, G: 18, B: 31, A: 255}
	draw.Draw(img, img.Bounds(), &image.Uniform{C: bg}, image.Point{}, draw.Src)

	ui.DrawRect(img, image.Rect(0, 0, img.Bounds().Dx(), 6), o.state.accent)
	ui.WriteText(img, o.cfg.Width-len([]rune(o.cfg.Branding))*o.glyphWidth-12, 24, o.cfg.Branding, color.RGBA{R: 148, G: 163, B: 184, A: 255}, o.smallFace)
	ui.DrawRect(img, image.Rect(20, 22, 20+96, 24), color.RGBA{R: 24, G: 38, B: 65, A: 255})
	ui.DrawBars(img, image.Rect(26, 42, 132, 98), o.state.accent, o.level,
		o.state.reactiveWave, o.state.idleWave, o.state.heartbeatWave, o.wavePhase)

	ui.WriteText(img, 150, 36, o.state.title, o.state.accent, o.face)
	if o.state.titleSuffix != "" {
		suffixX := 150 + len([]rune(o.state.title))*o.glyphWidth
		ui.WriteText(img, suffixX, 36, o.state.titleSuffix, color.RGBA{R: 226, G: 232, B: 240, A: 255}, o.face)
		if o.state.submitHint {
			hintX := suffixX + len([]rune(o.state.titleSuffix))*o.glyphWidth
			pulse := 0.5 + 0.5*math.Sin(o.wavePhase*3)
			alpha := uint8(140 + int(pulse*115))
			ui.WriteText(img, hintX, 36, " "+o.cfg.Listening.SubmitHint, color.RGBA{R: 251, G: 191, B: 36, A: alpha}, o.face)
		}
	}
	subtitleColor := color.RGBA{R: 226, G: 232, B: 240, A: 255}
	for i, line := range strings.Split(o.state.subtitle, "\n") {
		ui.WriteText(img, 150, 62+i*lineHeight, line, subtitleColor, o.face)
	}
	bodyColor := color.RGBA{R: 148, G: 163, B: 184, A: 255}
	for i, line := range ui.WrapLines(o.state.body, o.bodyTextLimit()) {
		ui.WriteText(img, 150, bodyStartY+i*lineHeight, line, bodyColor, o.face)
	}
	return img
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
		o.win.Resize(o.cfg.Width, o.height)
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
		o.fadeAlpha = linear * o.cfg.Opacity
		o.fadeOffset = int(float64(fadeSlideDistance) * (1 - eased))
		_ = ewmh.WmWindowOpacitySet(o.x, o.win.Id, o.fadeAlpha)
		o.win.Move(o.baseX, o.baseY-o.fadeOffset)
		o.mu.Unlock()
	}

	o.mu.Lock()
	o.fadeAlpha = o.cfg.Opacity
	o.fadeOffset = 0
	o.slidingIn = false
	_ = ewmh.WmWindowOpacitySet(o.x, o.win.Id, o.cfg.Opacity)
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
		o.fadeAlpha = o.cfg.Opacity * (1 - linear)
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
	x, y := position(o.x, o.cfg)
	o.baseX = x
	o.baseY = y
}

func position(xu *xgbutil.XUtil, cfg config.OverlayConfig) (int, int) {
	monX, monY, monW, _ := activeMonitor(xu)
	x := monX + monW/2 - cfg.Width/2
	if x < 0 {
		x = 0
	}
	y := monY + cfg.MarginTop
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

func (o *Overlay) subtitleTextLimit() int {
	return ui.TextLimit(o.cfg.Width, 20, o.glyphWidth)
}

func (o *Overlay) bodyTextLimit() int {
	return ui.TextLimit(o.cfg.Width, 20, o.glyphWidth)
}




func loadFont(name string, size float64) (font.Face, int) {
	path := findFont(name)
	if path != "" {
		data, err := os.ReadFile(path)
		if err == nil {
			f, err := opentype.Parse(data)
			if err == nil {
				face, err := opentype.NewFace(f, &opentype.FaceOptions{
					Size:    size,
					DPI:     72,
					Hinting: font.HintingFull,
				})
				if err == nil {
					adv, ok := face.GlyphAdvance('M')
					w := 7
					if ok {
						w = adv.Round()
					}
					sessionlog.Infof("overlay font: %s (%.0fpt, glyph %dpx)", path, size, w)
					return face, w
				}
			}
		}
		sessionlog.Warnf("failed to load font %s, falling back to basicfont", path)
	}
	return basicfont.Face7x13, 7
}

func findFont(name string) string {
	if name == "" {
		name = "monospace"
	}
	out, err := exec.Command("fc-match", name, "--format=%{file}").Output()
	if err != nil {
		return ""
	}
	path := strings.TrimSpace(string(out))
	if _, err := os.Stat(path); err != nil {
		return ""
	}
	return path
}

