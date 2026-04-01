package overlay

import (
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"math"
	"os"
	"os/exec"
	"strings"
	"sync"
	"time"

	"github.com/BurntSushi/xgb/xinerama"
	"github.com/BurntSushi/xgb/xproto"
	"github.com/BurntSushi/xgbutil"
	"github.com/BurntSushi/xgbutil/ewmh"
	"github.com/BurntSushi/xgbutil/xgraphics"
	"github.com/BurntSushi/xgbutil/xwindow"
	"golang.org/x/image/font"
	"golang.org/x/image/font/basicfont"
	"golang.org/x/image/font/opentype"
	"golang.org/x/image/math/fixed"

	"vocis/internal/config"
	"vocis/internal/sessionlog"
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
	smallFace    font.Face
	glyphWidth   int

	baseX      int
	baseY      int
	fadeToken  uint64
	fadeAlpha  float64
	fadeOffset int
	slidingIn  bool

	crossFadeT     float64
	crossPrevFrame *image.RGBA

	countdownReset chan countdownPhase
}

type countdownPhase struct {
	label   string
	timeout time.Duration
}

type viewState struct {
	title        string
	titleSuffix  string
	subtitle     string
	body         string
	accent       color.RGBA
	reactiveWave   bool
	idleWave       bool
	heartbeatWave  bool
}

func New(cfg config.OverlayConfig) (*Overlay, error) {
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

	face, glyphW := loadSystemFont(13)
	smallFace, _ := loadSystemFont(11)

	return &Overlay{
		cfg:        cfg,
		x:          xu,
		win:        win,
		height:     cfg.Height,
		face:       face,
		smallFace:  smallFace,
		glyphWidth: glyphW,
		baseX:      x,
		baseY:      y,
		fadeAlpha:   1,
		state: viewState{
			title:    "Ready",
			subtitle: "Voice typing is armed",
			body:     "",
			accent:   color.RGBA{R: 96, G: 165, B: 250, A: 255},
		},
	}, nil
}

func (o *Overlay) ShowHint(text string) {
	o.show(viewState{
		title:    "Ready",
		subtitle: "Voice typing is armed",
		body:     text,
		accent:   color.RGBA{R: 96, G: 165, B: 250, A: 255},
		idleWave: true,
	}, true)
}

func (o *Overlay) ShowListening(windowClass, hotkeyMode string) {
	body := listeningBody("")
	o.show(viewState{
		title:        "Listening",
		titleSuffix:  listeningHint(hotkeyMode),
		subtitle:     listeningSubtitle(windowClass),
		body:         body,
		accent:       color.RGBA{R: 34, G: 197, B: 94, A: 255},
		reactiveWave: true,
	}, false)
}

func (o *Overlay) SetListeningText(windowClass, text string) {
	o.mu.Lock()
	defer o.mu.Unlock()

	if !o.visible || o.state.title != "Listening" {
		return
	}

	subtitle := listeningSubtitle(windowClass)
	targetText := normalizeListeningText(text)
	body := listeningBody(targetText)
	o.liveBody = body
	currentText := displayedListeningText(o.state.body)
	if o.state.subtitle == subtitle && currentText == targetText {
		return
	}

	o.state.subtitle = subtitle
	if o.animating {
		return
	}
	if shouldAnimatePartial(currentText, targetText) {
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
	if !o.visible || o.state.title != "Listening" {
		o.mu.Unlock()
		return
	}

	o.animToken++
	token := o.animToken
	o.animating = true
	o.state.body = ""
	o.drawLocked()
	o.mu.Unlock()

	go o.animateChunk(token, shorten(text, o.bodyTextLimit()))
}

func (o *Overlay) ShowFinishing(body, shortcut string, timeout time.Duration) {
	var suffix string
	if shortcut != "" {
		suffix = fmt.Sprintf(" — press %s to cancel", shortcut)
	}

	phase := countdownPhase{label: "Wrapping up", timeout: timeout}
	o.show(viewState{
		title:         "Finishing",
		titleSuffix:   suffix,
		subtitle:      formatCountdown(phase.label, timeout),
		body:          body,
		accent:        color.RGBA{R: 96, G: 165, B: 250, A: 255},
		heartbeatWave: true,
	}, false)

	o.mu.Lock()
	o.countdownReset = make(chan countdownPhase, 1)
	o.mu.Unlock()

	go o.animateCountdown(phase)
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

	if !o.visible || o.state.title != "Finishing" {
		return
	}
	o.state.body = body
	o.drawLocked()
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
			label = newPhase.label
			deadline = time.Now().Add(newPhase.timeout)
		case <-ticker.C:
			o.mu.Lock()
			if !o.visible || o.state.title != "Finishing" {
				o.mu.Unlock()
				return
			}
			remaining := time.Until(deadline)
			if remaining <= 0 {
				o.state.subtitle = fmt.Sprintf("%s — timed out", label)
				o.drawLocked()
				o.mu.Unlock()
				return
			}
			o.state.subtitle = formatCountdown(label, remaining)
			o.drawLocked()
			o.mu.Unlock()
		}
	}
}

func (o *Overlay) ShowSuccess(text string) {
	o.show(viewState{
		title:    "Typed",
		subtitle: "Transcription inserted into your active app",
		body:     shorten(strings.ReplaceAll(text, "\n", " "), o.bodyTextLimit()),
		accent:   color.RGBA{R: 56, G: 189, B: 248, A: 255},
	}, true)
}

func (o *Overlay) ShowError(err error) {
	o.show(viewState{
		title:    "Error",
		subtitle: shorten(err.Error(), o.subtitleTextLimit()),
		accent:   color.RGBA{R: 248, G: 113, B: 113, A: 255},
	}, true)
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
	bodyLines := wrapLines(state.body, o.bodyTextLimit())
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
	if state.title == "Listening" {
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
	bodyStartY        = 90
	lineHeight        = 16
	bodyPadBot        = 12
	fadeSlideDistance  = 28
	fadeDuration      = 320 * time.Millisecond
	crossFadeDuration = 80 * time.Millisecond
	fadeInterval      = 12 * time.Millisecond
)

func (o *Overlay) drawLocked() {
	charsPerLine := o.bodyTextLimit()
	bodyLines := wrapLines(o.state.body, charsPerLine)

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

	drawRect(img, image.Rect(0, 0, img.Bounds().Dx(), 6), o.state.accent)
	writeText(img, o.cfg.Width-len("Vocis")*o.glyphWidth-12, 24, "Vocis", color.RGBA{R: 148, G: 163, B: 184, A: 255}, o.smallFace)
	drawRect(img, image.Rect(20, 22, 20+96, 24), color.RGBA{R: 24, G: 38, B: 65, A: 255})
	drawBars(
		img,
		image.Rect(26, 42, 132, 98),
		o.state.accent,
		o.level,
		o.state.reactiveWave,
		o.state.idleWave,
		o.state.heartbeatWave,
		o.wavePhase,
	)

	writeText(img, 150, 36, o.state.title, o.state.accent, o.face)
	if o.state.titleSuffix != "" {
		suffixX := 150 + len([]rune(o.state.title))*o.glyphWidth
		writeText(img, suffixX, 36, o.state.titleSuffix, color.RGBA{R: 226, G: 232, B: 240, A: 255}, o.smallFace)
	}
	writeText(img, 150, 62, o.state.subtitle, color.RGBA{R: 226, G: 232, B: 240, A: 255}, o.face)

	bodyColor := color.RGBA{R: 148, G: 163, B: 184, A: 255}
	for i, line := range bodyLines {
		writeText(img, 150, bodyStartY+i*lineHeight, line, bodyColor, o.face)
	}

	if o.crossPrevFrame != nil && o.crossFadeT < 1 {
		blendFrames(img, o.crossPrevFrame, 1-o.crossFadeT)
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
		if token != o.animToken || !o.visible || o.state.title != "Listening" {
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

	if token != o.animToken || !o.visible || o.state.title != "Listening" {
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
		next := nextWordBoundary(targetRunes, currentLen)
		time.Sleep(28 * time.Millisecond)

		o.mu.Lock()
		if token != o.partialToken || o.animating || !o.visible || o.state.title != "Listening" {
			o.mu.Unlock()
			return
		}
		o.state.body = listeningBody(string(targetRunes[:next]))
		o.drawLocked()
		o.mu.Unlock()

		currentLen = next
	}
}

func shouldAnimatePartial(current, target string) bool {
	current = normalizeListeningText(current)
	target = normalizeListeningText(target)
	if target == "" {
		return false
	}
	if current == "" {
		return true
	}
	if !strings.HasPrefix(target, current) {
		return false
	}
	return target != current
}

func nextWordBoundary(runes []rune, start int) int {
	if start >= len(runes) {
		return len(runes)
	}
	for i := start + 1; i < len(runes); i++ {
		if runes[i] == ' ' {
			return i + 1
		}
	}
	return len(runes)
}

func drawBars(
	dst *image.RGBA,
	rect image.Rectangle,
	accent color.RGBA,
	level float64,
	reactive bool,
	idle bool,
	heartbeat bool,
	phase float64,
) {
	width := 10
	gap := 6
	baseY := rect.Max.Y
	profile := []float64{0.38, 0.62, 0.92, 0.82, 0.58, 0.34}

	for i, weight := range profile {
		height := 14 + i%2*4
		if reactive {
			height = 10 + int((level*weight)*54)
		} else if heartbeat {
			height = 10 + int(weight*40*heartbeatPulse(phase, float64(i)*0.08))
		} else if idle {
			pulse := 0.5 + 0.5*math.Sin(phase+float64(i)*0.75)
			height = 14 + int((weight*20)*pulse)
		}
		x := rect.Min.X + i*(width+gap)
		r := image.Rect(x, baseY-height, x+width, baseY)
		drawRect(dst, r, accent)
	}
}

// heartbeatPulse returns a 0..1 amplitude for a heartbeat pattern:
// two quick beats (lub-dub) followed by a rest period.
func heartbeatPulse(phase, offset float64) float64 {
	// Full cycle is 2π; divide into lub, dub, rest
	t := math.Mod(phase+offset, 2*math.Pi) / (2 * math.Pi) // 0..1
	switch {
	case t < 0.1: // first beat (lub): sharp rise and fall
		return math.Sin(t / 0.1 * math.Pi)
	case t < 0.15: // brief gap
		return 0
	case t < 0.25: // second beat (dub): slightly smaller
		return 0.7 * math.Sin((t-0.15)/0.1*math.Pi)
	default: // rest
		return 0
	}
}

func writeText(dst *image.RGBA, x, y int, msg string, clr color.Color, face font.Face) {
	d := &font.Drawer{
		Dst:  dst,
		Src:  image.NewUniform(clr),
		Face: face,
		Dot:  fixed.P(x, y),
	}
	d.DrawString(msg)
}


func drawRect(dst *image.RGBA, rect image.Rectangle, clr color.Color) {
	draw.Draw(dst, rect, &image.Uniform{C: clr}, image.Point{}, draw.Src)
}

func (o *Overlay) captureFrameLocked() *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, o.cfg.Width, o.height))
	bg := color.RGBA{R: 12, G: 18, B: 31, A: 255}
	draw.Draw(img, img.Bounds(), &image.Uniform{C: bg}, image.Point{}, draw.Src)

	drawRect(img, image.Rect(0, 0, img.Bounds().Dx(), 6), o.state.accent)
	writeText(img, o.cfg.Width-len("Vocis")*o.glyphWidth-12, 24, "Vocis", color.RGBA{R: 148, G: 163, B: 184, A: 255}, o.smallFace)
	drawRect(img, image.Rect(20, 22, 20+96, 24), color.RGBA{R: 24, G: 38, B: 65, A: 255})
	drawBars(img, image.Rect(26, 42, 132, 98), o.state.accent, o.level,
		o.state.reactiveWave, o.state.idleWave, o.state.heartbeatWave, o.wavePhase)

	writeText(img, 150, 36, o.state.title, o.state.accent, o.face)
	if o.state.titleSuffix != "" {
		suffixX := 150 + len([]rune(o.state.title))*o.glyphWidth
		writeText(img, suffixX, 36, o.state.titleSuffix, color.RGBA{R: 226, G: 232, B: 240, A: 255}, o.smallFace)
	}
	subtitleColor := color.RGBA{R: 226, G: 232, B: 240, A: 255}
	for i, line := range strings.Split(o.state.subtitle, "\n") {
		writeText(img, 150, 62+i*lineHeight, line, subtitleColor, o.face)
	}
	bodyColor := color.RGBA{R: 148, G: 163, B: 184, A: 255}
	for i, line := range wrapLines(o.state.body, o.bodyTextLimit()) {
		writeText(img, 150, bodyStartY+i*lineHeight, line, bodyColor, o.face)
	}
	return img
}

func blendFrames(dst, src *image.RGBA, alpha float64) {
	if alpha <= 0 {
		return
	}
	bounds := dst.Bounds().Intersect(src.Bounds())
	a := uint32(alpha * 255)
	for y := bounds.Min.Y; y < bounds.Max.Y; y++ {
		for x := bounds.Min.X; x < bounds.Max.X; x++ {
			di := dst.PixOffset(x, y)
			si := src.PixOffset(x, y)
			dst.Pix[di+0] = uint8((uint32(dst.Pix[di+0])*(255-a) + uint32(src.Pix[si+0])*a) / 255)
			dst.Pix[di+1] = uint8((uint32(dst.Pix[di+1])*(255-a) + uint32(src.Pix[si+1])*a) / 255)
			dst.Pix[di+2] = uint8((uint32(dst.Pix[di+2])*(255-a) + uint32(src.Pix[si+2])*a) / 255)
		}
	}
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
		eased := easeOutCubic(linear)
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
		eased := easeInCubic(linear)
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

func easeOutCubic(t float64) float64 {
	t--
	return 1 + t*t*t
}

func easeInCubic(t float64) float64 {
	return t * t * t
}

func listeningHint(hotkeyMode string) string {
	if hotkeyMode == "toggle" {
		return " — press to stop"
	}
	return " — release to paste"
}

func listeningSubtitle(windowClass string) string {
	if strings.TrimSpace(windowClass) != "" {
		return fmt.Sprintf("Ready to type into %s", windowClass)
	}
	return "Recording from your microphone"
}

func listeningBody(text string) string {
	text = normalizeListeningText(text)
	if text == "" {
		return "Speak naturally. Release when you want it pasted."
	}
	return text
}

func normalizeListeningText(text string) string {
	return strings.TrimSpace(text)
}

func displayedListeningText(body string) string {
	text := normalizeListeningText(body)
	if text == normalizeListeningText(listeningBody("")) {
		return ""
	}
	return text
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
	return textLimit(o.cfg.Width, 20, o.glyphWidth)
}

func (o *Overlay) bodyTextLimit() int {
	return textLimit(o.cfg.Width, 20, o.glyphWidth)
}

func textLimit(width, rightPadding, glyphWidth int) int {
	const (
		textLeft = 150
		minChars = 12
	)
	if glyphWidth <= 0 {
		glyphWidth = 7
	}

	available := width - textLeft - rightPadding
	if available <= 0 {
		return minChars
	}

	chars := available / glyphWidth
	if chars < minChars {
		return minChars
	}
	return chars
}

func wrapLines(text string, maxChars int) []string {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	if maxChars <= 0 {
		return []string{text}
	}

	var lines []string
	for _, paragraph := range strings.Split(text, "\n") {
		paragraph = strings.TrimSpace(paragraph)
		if paragraph == "" {
			continue
		}
		lines = append(lines, wrapParagraph(paragraph, maxChars)...)
	}
	if len(lines) == 0 {
		return nil
	}
	return lines
}

func wrapParagraph(text string, maxChars int) []string {
	runes := []rune(text)
	if len(runes) <= maxChars {
		return []string{text}
	}

	var lines []string
	for len(runes) > 0 {
		if len(runes) <= maxChars {
			lines = append(lines, string(runes))
			break
		}
		cut := maxChars
		for i := maxChars; i > maxChars/2; i-- {
			if runes[i] == ' ' {
				cut = i
				break
			}
		}
		lines = append(lines, string(runes[:cut]))
		runes = runes[cut:]
		if len(runes) > 0 && runes[0] == ' ' {
			runes = runes[1:]
		}
	}
	return lines
}

func loadSystemFont(size float64) (font.Face, int) {
	path := findSystemMonoFont()
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

func findSystemMonoFont() string {
	out, err := exec.Command("fc-match", "monospace", "--format=%{file}").Output()
	if err != nil {
		return ""
	}
	path := strings.TrimSpace(string(out))
	if _, err := os.Stat(path); err != nil {
		return ""
	}
	return path
}

func shorten(s string, max int) string {
	s = strings.TrimSpace(s)
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	if max <= 3 {
		return string(runes[:max])
	}
	return string(runes[:max-3]) + "..."
}
