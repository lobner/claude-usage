// Package icon renders the menu-bar icon programmatically (no binary assets):
// two vertical 0–100% meter bars side by side — left = current session, right =
// weekly. Each bar is an always-visible rounded "track" outline with a solid
// fill rising from the bottom in proportion to its percentage.
//
// The image is monochrome and used via systray.SetTemplateIcon, so macOS tints
// it automatically to match a light or dark menu bar (filled portion = solid
// tint, empty portion = thin outline). The rendering technique — a signed
// distance field rasterised with 4×4 supersampling — mirrors the sibling
// github-status-tracker project.
package icon

import (
	"bytes"
	"image"
	"image/color"
	"image/draw"
	"image/png"
	"math"
)

// size is the canvas edge in pixels: 22pt @2x. macOS scales it to the menu-bar
// height, so the two-bar icon ends up roughly square (~22pt wide).
const size = 44

// Bar geometry within the size×size canvas.
const (
	marginY  = 5.0             // top/bottom padding
	trackTop = marginY         // 5
	trackBot = size - marginY  // 39
	trackH   = trackBot - trackTop
	barCY    = size / 2.0      // 22 — bars are vertically centred
	barHalfH = trackH / 2.0    // 17
	barHalfW = 7.0             // 14px-wide bars
	barR     = 4.0             // corner radius
	stroke   = 2.5             // track outline thickness
	leftCX   = 12.0            // left (session) bar centre x  → spans [5,19]
	rightCX  = 32.0            // right (weekly)  bar centre x  → spans [25,39]
)

// colMono is the only colour used: for a template icon just the alpha matters.
var colMono = color.NRGBA{R: 0x00, G: 0x00, B: 0x00, A: 0xff}

// sdRoundRect is the signed distance from (px,py) to a rounded rectangle centred
// at the origin with half-extents (hx,hy) and corner radius r (negative=inside).
func sdRoundRect(px, py, hx, hy, r float64) float64 {
	qx := math.Abs(px) - hx + r
	qy := math.Abs(py) - hy + r
	return math.Hypot(math.Max(qx, 0), math.Max(qy, 0)) + math.Min(math.Max(qx, qy), 0) - r
}

// maskFor builds an anti-aliased coverage mask for a shape predicate by 4×4
// supersampling each pixel.
func maskFor(b image.Rectangle, inside func(x, y float64) bool) *image.Alpha {
	const ss = 4
	m := image.NewAlpha(b)
	for py := b.Min.Y; py < b.Max.Y; py++ {
		for px := b.Min.X; px < b.Max.X; px++ {
			hits := 0
			for sy := 0; sy < ss; sy++ {
				for sx := 0; sx < ss; sx++ {
					x := float64(px) + (float64(sx)+0.5)/ss
					y := float64(py) + (float64(sy)+0.5)/ss
					if inside(x, y) {
						hits++
					}
				}
			}
			if hits > 0 {
				m.SetAlpha(px, py, color.Alpha{A: uint8(hits * 255 / (ss * ss))})
			}
		}
	}
	return m
}

func drawLayer(dst *image.NRGBA, inside func(x, y float64) bool, col color.NRGBA) {
	mask := maskFor(dst.Bounds(), inside)
	draw.DrawMask(dst, dst.Bounds(), image.NewUniform(col), image.Point{}, mask, image.Point{}, draw.Over)
}

func clampPct(p float64) float64 {
	switch {
	case p < 0:
		return 0
	case p > 100:
		return 100
	default:
		return p
	}
}

// barTrack is the rounded outline (ring) of the bar centred at cx — the empty
// "0–100" boundary, always drawn.
func barTrack(cx float64) func(x, y float64) bool {
	return func(x, y float64) bool {
		d := sdRoundRect(x-cx, y-barCY, barHalfW, barHalfH, barR)
		return d <= 0 && d >= -stroke
	}
}

// barFill is the solid fill of the bar centred at cx, rising from the bottom to
// pct% of the track height. A flat top edge cuts across at the fill line; the
// bottom corners follow the track's rounding.
func barFill(cx, pct float64) func(x, y float64) bool {
	p := clampPct(pct) / 100.0
	if p <= 0 {
		return func(x, y float64) bool { return false }
	}
	fillLineY := trackBot - p*trackH // smaller y = higher up = fuller
	return func(x, y float64) bool {
		if y < fillLineY {
			return false
		}
		return sdRoundRect(x-cx, y-barCY, barHalfW, barHalfH, barR) <= 0
	}
}

// barsImage draws the two meters into a fresh canvas (session left, weekly right).
func barsImage(sessionPct, weeklyPct float64) *image.NRGBA {
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	drawLayer(img, barTrack(leftCX), colMono)
	drawLayer(img, barFill(leftCX, sessionPct), colMono)
	drawLayer(img, barTrack(rightCX), colMono)
	drawLayer(img, barFill(rightCX, weeklyPct), colMono)
	return img
}

func encode(img *image.NRGBA) []byte {
	var buf bytes.Buffer
	_ = png.Encode(&buf, img)
	return buf.Bytes()
}

// BarsPNG returns the template PNG for the two meters: left bar = session,
// right bar = weekly, each filled to its percentage (0–100, clamped).
func BarsPNG(sessionPct, weeklyPct float64) []byte {
	return encode(barsImage(sessionPct, weeklyPct))
}
