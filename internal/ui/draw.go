package ui

import (
	"image"
	"image/color"
	"image/draw"
	"math"

	"golang.org/x/image/font"
	"golang.org/x/image/math/fixed"
)

// WriteText renders a string at the given position on an RGBA image.
func WriteText(dst *image.RGBA, x, y int, msg string, clr color.Color, face font.Face) {
	d := &font.Drawer{
		Dst:  dst,
		Src:  image.NewUniform(clr),
		Face: face,
		Dot:  fixed.P(x, y),
	}
	d.DrawString(msg)
}

// DrawRect fills a rectangle on an RGBA image.
func DrawRect(dst *image.RGBA, rect image.Rectangle, clr color.Color) {
	draw.Draw(dst, rect, &image.Uniform{C: clr}, image.Point{}, draw.Src)
}

// BlendFrames alpha-blends src onto dst with the given alpha (0..1).
func BlendFrames(dst, src *image.RGBA, alpha float64) {
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

// DrawBars renders the waveform bar visualization.
func DrawBars(
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
			height = 10 + int(weight*40*HeartbeatPulse(phase, float64(i)*0.08))
		} else if idle {
			pulse := 0.5 + 0.5*math.Sin(phase+float64(i)*0.75)
			height = 14 + int((weight*20)*pulse)
		}
		x := rect.Min.X + i*(width+gap)
		r := image.Rect(x, baseY-height, x+width, baseY)
		DrawRect(dst, r, accent)
	}
}

// HeartbeatPulse returns a 0..1 amplitude for a heartbeat pattern:
// two quick beats (lub-dub) followed by a rest period.
func HeartbeatPulse(phase, offset float64) float64 {
	t := math.Mod(phase+offset, 2*math.Pi) / (2 * math.Pi)
	switch {
	case t < 0.1:
		return math.Sin(t / 0.1 * math.Pi)
	case t < 0.15:
		return 0
	case t < 0.25:
		return 0.7 * math.Sin((t-0.15)/0.1*math.Pi)
	default:
		return 0
	}
}

// EaseOutCubic applies ease-out cubic easing.
func EaseOutCubic(t float64) float64 {
	t--
	return 1 + t*t*t
}

// EaseInCubic applies ease-in cubic easing.
func EaseInCubic(t float64) float64 {
	return t * t * t
}
