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

	mu       sync.Mutex
	x        *xgbutil.XUtil
	win      *xwindow.Window
	visible  bool
	state    viewState
	phase    float64
	animTick *time.Ticker
	animStop chan struct{}
	hide     *time.Timer
}

type viewState struct {
	title     string
	subtitle  string
	body      string
	accent    color.RGBA
	animating bool
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
	}, true)
}

func (o *Overlay) ShowListening(windowClass string) {
	subtitle := "Recording from your microphone"
	if strings.TrimSpace(windowClass) != "" {
		subtitle = fmt.Sprintf("Ready to type into %s", windowClass)
	}
	o.show(viewState{
		title:     "Listening",
		subtitle:  subtitle,
		body:      "Press the hotkey again to transcribe and paste.",
		accent:    color.RGBA{R: 34, G: 197, B: 94, A: 255},
		animating: true,
	}, false)
}

func (o *Overlay) ShowTranscribing(path string) {
	o.show(viewState{
		title:     "Transcribing",
		subtitle:  "Sending audio to OpenAI",
		body:      shorten(path, o.bodyTextLimit()),
		accent:    color.RGBA{R: 245, G: 158, B: 11, A: 255},
		animating: true,
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

	o.stopAnimationLocked()
	if o.hide != nil {
		o.hide.Stop()
	}
	if o.win != nil {
		o.win.Destroy()
	}
}

func (o *Overlay) show(state viewState, autoHide bool) {
	o.mu.Lock()
	defer o.mu.Unlock()

	o.state = state
	o.phase = 0
	o.drawLocked()

	if !o.visible {
		o.visible = true
		o.win.Map()
		o.win.Stack(xproto.StackModeAbove)
	}

	if state.animating {
		o.startAnimationLocked()
	} else {
		o.stopAnimationLocked()
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
}

func (o *Overlay) startAnimationLocked() {
	if o.animTick != nil {
		return
	}

	o.animTick = time.NewTicker(110 * time.Millisecond)
	o.animStop = make(chan struct{})
	ticker := o.animTick
	stop := o.animStop
	go func() {
		for {
			select {
			case <-ticker.C:
			case <-stop:
				return
			}

			o.mu.Lock()
			o.phase += 0.55
			o.drawLocked()
			o.mu.Unlock()
		}
	}()
}

func (o *Overlay) stopAnimationLocked() {
	if o.animTick == nil {
		return
	}
	o.animTick.Stop()
	close(o.animStop)
	o.animTick = nil
	o.animStop = nil
}

func (o *Overlay) drawLocked() {
	img := image.NewRGBA(image.Rect(0, 0, o.cfg.Width, o.cfg.Height))
	bg := color.RGBA{R: 12, G: 18, B: 31, A: 255}
	draw.Draw(img, img.Bounds(), &image.Uniform{C: bg}, image.Point{}, draw.Src)

	drawRect(img, image.Rect(0, 0, img.Bounds().Dx(), 6), o.state.accent)
	drawRect(img, image.Rect(20, 22, 20+96, 24), color.RGBA{R: 24, G: 38, B: 65, A: 255})
	drawBars(img, image.Rect(26, 42, 132, 98), o.state.accent, o.phase, o.state.animating)

	writeText(img, 150, 36, o.state.title, o.state.accent, basicfont.Face7x13)
	writeText(img, 150, 62, o.state.subtitle, color.RGBA{R: 226, G: 232, B: 240, A: 255}, basicfont.Face7x13)
	if strings.TrimSpace(o.state.body) != "" {
		writeText(img, 150, 90, o.state.body, color.RGBA{R: 148, G: 163, B: 184, A: 255}, basicfont.Face7x13)
	}

	ximg := xgraphics.NewConvert(o.x, img)
	ximg.XSurfaceSet(o.win.Id)
	ximg.XDraw()
	ximg.XPaint(o.win.Id)
	ximg.Destroy()
}

func drawBars(dst *image.RGBA, rect image.Rectangle, accent color.RGBA, phase float64, animate bool) {
	width := 10
	gap := 6
	count := 6
	baseY := rect.Max.Y
	for i := 0; i < count; i++ {
		height := 18
		if animate {
			height = 18 + int(math.Abs(math.Sin(phase+float64(i)*0.55))*34)
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
	if len(s) <= max {
		return s
	}
	if max <= 1 {
		return s[:max]
	}
	return s[:max-1] + "…"
}
