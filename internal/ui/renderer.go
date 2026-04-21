package ui

import (
	"image"
	"image/color"
	"image/draw"
	"math"
	"os"
	"os/exec"
	"strings"

	"golang.org/x/image/font"
	"golang.org/x/image/font/basicfont"
	"golang.org/x/image/font/opentype"

	"vocis/internal/config"
	"vocis/internal/sessionlog"
)

// Vertical layout constants for the overlay body text.
const (
	OverlayBodyStartY = 98
	OverlayLineHeight = 16
	OverlayBodyPadBot = 12
)

// State describes what the overlay should display. It is rendering input
// only — no animation or timing state.
type State struct {
	Title         string
	TitleSuffix   string
	SubmitHint    bool
	Subtitle      string
	Body          string
	Accent        color.RGBA
	ReactiveWave  bool
	IdleWave      bool
	HeartbeatWave bool
}

// Frame bundles a State with the per-frame transient values that drive
// animation (level bars, wave phase, crossfade).
type Frame struct {
	State
	Level      float64
	WavePhase  float64
	Height     int
	CrossFadeT float64
	CrossPrev  *image.RGBA
}

// OverlayRenderer paints an overlay Frame into an *image.RGBA. It owns
// the fonts and layout config so it can be shared across the X11 and
// any future Wayland/native backend without re-loading resources.
type OverlayRenderer struct {
	cfg             config.OverlayConfig
	face            font.Face
	smallFace       font.Face
	glyphWidth      int
	smallGlyphWidth int
}

func NewOverlayRenderer(cfg config.OverlayConfig) *OverlayRenderer {
	fontSize := cfg.FontSize
	if fontSize <= 0 {
		fontSize = 13
	}
	face, gw := loadFont(cfg.Font, fontSize)
	smallFace, sgw := loadFont(cfg.Font, fontSize-2)
	return &OverlayRenderer{
		cfg:             cfg,
		face:            face,
		smallFace:       smallFace,
		glyphWidth:      gw,
		smallGlyphWidth: sgw,
	}
}

func (r *OverlayRenderer) Config() config.OverlayConfig { return r.cfg }
func (r *OverlayRenderer) Face() font.Face              { return r.face }
func (r *OverlayRenderer) SmallFace() font.Face         { return r.smallFace }
func (r *OverlayRenderer) GlyphWidth() int              { return r.glyphWidth }
func (r *OverlayRenderer) SmallGlyphWidth() int         { return r.smallGlyphWidth }

// BodyTextLimit returns the maximum characters per line of body text
// that fit within the overlay width given the current font.
func (r *OverlayRenderer) BodyTextLimit() int {
	return TextLimit(r.cfg.Width, 20, r.glyphWidth)
}

// SubtitleTextLimit returns the character budget for a subtitle line.
func (r *OverlayRenderer) SubtitleTextLimit() int {
	return TextLimit(r.cfg.Width, 20, r.glyphWidth)
}

// NeededHeight returns the overlay height required to fit the given
// body text, never shrinking below the configured minimum.
func (r *OverlayRenderer) NeededHeight(body string) int {
	lines := WrapLines(body, r.BodyTextLimit())
	needed := r.cfg.Height
	if len(lines) > 1 {
		needed = OverlayBodyStartY + len(lines)*OverlayLineHeight + OverlayBodyPadBot
	}
	if needed < r.cfg.Height {
		needed = r.cfg.Height
	}
	return needed
}

// Render paints the overlay frame into a freshly-allocated RGBA image.
func (r *OverlayRenderer) Render(f Frame) *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, r.cfg.Width, f.Height))
	bg := color.RGBA{R: 12, G: 18, B: 31, A: 255}
	draw.Draw(img, img.Bounds(), &image.Uniform{C: bg}, image.Point{}, draw.Src)

	DrawRect(img, image.Rect(0, 0, img.Bounds().Dx(), 6), f.Accent)
	WriteText(img, r.cfg.Width-len([]rune(r.cfg.Branding))*r.glyphWidth-12, 24, r.cfg.Branding, color.RGBA{R: 148, G: 163, B: 184, A: 255}, r.smallFace)
	DrawRect(img, image.Rect(20, 22, 20+96, 24), color.RGBA{R: 24, G: 38, B: 65, A: 255})
	DrawBars(img, image.Rect(26, 42, 132, 98), f.Accent, f.Level,
		f.ReactiveWave, f.IdleWave, f.HeartbeatWave, f.WavePhase)

	WriteText(img, 150, 36, f.Title, f.Accent, r.face)
	if f.TitleSuffix != "" {
		suffixX := 150 + len([]rune(f.Title))*r.glyphWidth
		WriteText(img, suffixX, 36, f.TitleSuffix, color.RGBA{R: 226, G: 232, B: 240, A: 255}, r.face)
		if f.SubmitHint {
			hintX := suffixX + len([]rune(f.TitleSuffix))*r.glyphWidth
			pulse := 0.5 + 0.5*math.Sin(f.WavePhase*3)
			alpha := uint8(140 + int(pulse*115))
			WriteText(img, hintX, 36, " "+r.cfg.Listening.SubmitHint, color.RGBA{R: 251, G: 191, B: 36, A: alpha}, r.face)
		}
	}
	subtitleColor := color.RGBA{R: 226, G: 232, B: 240, A: 255}
	for i, line := range strings.Split(f.Subtitle, "\n") {
		WriteText(img, 150, 62+i*OverlayLineHeight, line, subtitleColor, r.face)
	}
	bodyColor := color.RGBA{R: 148, G: 163, B: 184, A: 255}
	for i, line := range WrapLines(f.Body, r.BodyTextLimit()) {
		WriteText(img, 150, OverlayBodyStartY+i*OverlayLineHeight, line, bodyColor, r.face)
	}

	if f.CrossPrev != nil && f.CrossFadeT < 1 {
		BlendFrames(img, f.CrossPrev, 1-f.CrossFadeT)
	}
	return img
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
