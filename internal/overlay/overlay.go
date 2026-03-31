package overlay

import (
	"fmt"
	"image"
	"image/color"
	"image/draw"
	"math"
	"strings"
	"sync"
	"time"

	"github.com/BurntSushi/xgb/xproto"
	"github.com/BurntSushi/xgbutil"
	"github.com/BurntSushi/xgbutil/ewmh"
	"github.com/BurntSushi/xgbutil/xgraphics"
	"github.com/BurntSushi/xgbutil/xwindow"
	"golang.org/x/image/font"
	"golang.org/x/image/font/basicfont"
	"golang.org/x/image/math/fixed"

	"vtt/internal/config"
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
}

type viewState struct {
	title        string
	subtitle     string
	body         string
	accent       color.RGBA
	reactiveWave bool
	idleWave     bool
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

	return &Overlay{
		cfg: cfg,
		x:   xu,
		win: win,
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
		subtitle: text,
		accent:   color.RGBA{R: 96, G: 165, B: 250, A: 255},
		idleWave: true,
	}, true)
}

func (o *Overlay) ShowListening(windowClass string) {
	body := listeningBody("")
	o.show(viewState{
		title:        "Listening",
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

func (o *Overlay) ShowTranscribing() {
	o.show(viewState{
		title:    "Transcribing",
		subtitle: "Turning your speech into polished text",
		body:     "Keeping code, Git, and GitHub terms intact.",
		accent:   color.RGBA{R: 245, G: 158, B: 11, A: 255},
	}, false)
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

	o.animToken++
	o.animating = false
	o.partialToken++
	if o.hide != nil {
		o.hide.Stop()
	}
	if o.win != nil {
		o.win.Destroy()
	}
}

func (o *Overlay) Hide() {
	o.mu.Lock()
	defer o.mu.Unlock()

	o.animToken++
	o.animating = false
	o.partialToken++
	if o.hide != nil {
		o.hide.Stop()
		o.hide = nil
	}
	o.visible = false
	if o.win != nil {
		o.win.Unmap()
	}
}

func (o *Overlay) show(state viewState, autoHide bool) {
	o.mu.Lock()
	defer o.mu.Unlock()

	o.animToken++
	o.animating = false
	o.partialToken++
	o.state = state
	o.level = 0
	o.wavePhase = 0
	if state.title == "Listening" {
		o.liveBody = state.body
	} else {
		o.liveBody = ""
	}
	o.drawLocked()

	if !o.visible {
		o.visible = true
		o.win.Map()
		o.win.Stack(xproto.StackModeAbove)
	}

	if o.hide != nil {
		o.hide.Stop()
		o.hide = nil
	}
	if autoHide {
		duration := time.Duration(o.cfg.AutoHideMillis) * time.Millisecond
		o.hide = time.AfterFunc(duration, func() {
			o.mu.Lock()
			defer o.mu.Unlock()
			o.visible = false
			o.win.Unmap()
		})
	}
	if state.idleWave {
		go o.animateIdleWave(o.animToken)
	}
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

func (o *Overlay) drawLocked() {
	img := image.NewRGBA(image.Rect(0, 0, o.cfg.Width, o.cfg.Height))
	bg := color.RGBA{R: 12, G: 18, B: 31, A: 255}
	draw.Draw(img, img.Bounds(), &image.Uniform{C: bg}, image.Point{}, draw.Src)

	drawRect(img, image.Rect(0, 0, img.Bounds().Dx(), 6), o.state.accent)
	drawRect(img, image.Rect(20, 22, 20+96, 24), color.RGBA{R: 24, G: 38, B: 65, A: 255})
	drawBars(
		img,
		image.Rect(26, 42, 132, 98),
		o.state.accent,
		o.level,
		o.state.reactiveWave,
		o.state.idleWave,
		o.wavePhase,
	)

	writeText(img, 150, 36, o.state.title, o.state.accent, basicfont.Face7x13)
	writeText(img, 150, 62, o.state.subtitle, color.RGBA{R: 226, G: 232, B: 240, A: 255}, basicfont.Face7x13)
	if strings.TrimSpace(o.state.body) != "" {
		writeText(img, 150, 90, shorten(o.state.body, o.bodyTextLimit()), color.RGBA{R: 148, G: 163, B: 184, A: 255}, basicfont.Face7x13)
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
		} else if idle {
			pulse := 0.5 + 0.5*math.Sin(phase+float64(i)*0.75)
			height = 14 + int((weight*20)*pulse)
		}
		x := rect.Min.X + i*(width+gap)
		r := image.Rect(x, baseY-height, x+width, baseY)
		drawRect(dst, r, accent)
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
	return strings.TrimSpace(strings.ReplaceAll(text, "\n", " "))
}

func displayedListeningText(body string) string {
	text := normalizeListeningText(body)
	if text == normalizeListeningText(listeningBody("")) {
		return ""
	}
	return text
}

func position(xu *xgbutil.XUtil, cfg config.OverlayConfig) (int, int) {
	screen := xu.Screen()
	x := int(screen.WidthInPixels)/2 - cfg.Width/2
	if x < 0 {
		x = 0
	}
	y := cfg.MarginTop
	if y < 0 {
		y = 0
	}
	return x, y
}

func (o *Overlay) subtitleTextLimit() int {
	return textLimit(o.cfg.Width, 20)
}

func (o *Overlay) bodyTextLimit() int {
	return textLimit(o.cfg.Width, 20)
}

func textLimit(width int, rightPadding int) int {
	const (
		textLeft   = 150
		glyphWidth = 7
		minChars   = 12
	)

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
