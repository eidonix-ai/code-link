// Package icon draws code-link's glyph — two links of a chain — for the
// tray (per state) and the app icon, as PNG. Drawn in code so there are no
// image assets to keep in step.
package icon

import (
	"bytes"
	"image"
	"image/color"
	"image/png"
	"math"
)

// Badge is the small dot a tray icon carries for trouble.
type Badge int

const (
	NoBadge Badge = iota
	Warn
	Error
)

// Colours of the badges and the brand.
var (
	Black  = color.NRGBA{0, 0, 0, 255}
	White  = color.NRGBA{255, 255, 255, 255}
	Ink    = color.NRGBA{0x1f, 0x1f, 0x1f, 255}
	Teal   = color.NRGBA{0x00, 0xC2, 0xC7, 255}
	Deep   = color.NRGBA{0x00, 0x7E, 0x82, 255}
	Red    = color.NRGBA{0xE5, 0x48, 0x4D, 255}
	Amber  = color.NRGBA{0xE0, 0xA6, 0x3A, 255}
	sqrt12 = 1 / math.Sqrt2
)

// capsuleRing is the signed distance to a stadium-shaped ring centred at
// (cx, cy) along direction (ux, uy): half-length l, radius r, stroke w.
func capsuleRing(x, y, cx, cy, ux, uy, l, r, w float64) float64 {
	px, py := x-cx, y-cy
	t := px*ux + py*uy
	t = math.Max(-l, math.Min(l, t))
	dx, dy := px-t*ux, py-t*uy
	return math.Abs(math.Hypot(dx, dy)-r) - w/2
}

// glyph is the distance to the two links in unit coordinates ([-1, 1],
// y down), drawn along the rising diagonal.
func glyph(x, y float64) float64 {
	ux, uy := sqrt12, -sqrt12
	const off, l, r, w = 0.34, 0.20, 0.30, 0.17
	a := capsuleRing(x, y, -off*ux, -off*uy, ux, uy, l, r, w)
	b := capsuleRing(x, y, off*ux, off*uy, ux, uy, l, r, w)
	return math.Min(a, b)
}

// coverage samples a shape over a pixel (4×4) and returns 0..1.
func coverage(px, py int, f func(x, y float64) bool) float64 {
	n := 0
	for sy := 0; sy < 4; sy++ {
		for sx := 0; sx < 4; sx++ {
			if f(float64(px)+(float64(sx)+0.5)/4, float64(py)+(float64(sy)+0.5)/4) {
				n++
			}
		}
	}
	return float64(n) / 16
}

func blend(dst color.NRGBA, src color.NRGBA, a float64) color.NRGBA {
	sa := float64(src.A) / 255 * a
	da := float64(dst.A) / 255
	oa := sa + da*(1-sa)
	if oa == 0 {
		return color.NRGBA{}
	}
	mix := func(s, d uint8) uint8 { return uint8(math.Round((float64(s)*sa + float64(d)*da*(1-sa)) / oa)) }
	return color.NRGBA{mix(src.R, dst.R), mix(src.G, dst.G), mix(src.B, dst.B), uint8(math.Round(oa * 255))}
}

// Tray draws a tray icon of size×size pixels: the glyph in fg at alpha
// (0..1) within scale of the square, and a badge dot at the bottom right
// with a clear gap around it.
func Tray(size int, fg color.NRGBA, alpha, scale float64, badge Badge, badgeColor color.NRGBA) []byte {
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	s := float64(size)
	half := s / 2
	bx, by, br := s*0.80, s*0.80, s*0.17
	toUnit := func(x, y float64) (float64, float64) { return (x - half) / (half * scale), (y - half) / (half * scale) }
	for py := 0; py < size; py++ {
		for px := 0; px < size; px++ {
			g := coverage(px, py, func(x, y float64) bool {
				if badge != NoBadge && math.Hypot(x-bx, y-by) < br*1.45 {
					return false // the gap around the dot
				}
				ux, uy := toUnit(x, y)
				return glyph(ux, uy) < 0
			})
			c := color.NRGBA{}
			if g > 0 {
				c = blend(c, fg, g*alpha)
			}
			if badge != NoBadge {
				if d := coverage(px, py, func(x, y float64) bool { return math.Hypot(x-bx, y-by) < br }); d > 0 {
					c = blend(c, badgeColor, d)
				}
			}
			img.SetNRGBA(px, py, c)
		}
	}
	return encode(img)
}

// App draws the application icon: a white glyph on a teal rounded square
// with the margins macOS icons use.
func App(size int) []byte {
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	s := float64(size)
	inset := s * 0.098 // 100 of 1024
	side := s - 2*inset
	radius := side * 0.225
	inSquare := func(x, y float64) bool {
		qx := math.Abs(x-s/2) - (side/2 - radius)
		qy := math.Abs(y-s/2) - (side/2 - radius)
		d := math.Hypot(math.Max(qx, 0), math.Max(qy, 0)) + math.Min(math.Max(qx, qy), 0) - radius
		return d < 0
	}
	for py := 0; py < size; py++ {
		for px := 0; px < size; px++ {
			sq := coverage(px, py, inSquare)
			if sq == 0 {
				continue
			}
			// Teal, a touch deeper towards the bottom.
			t := float64(py) / s
			bg := color.NRGBA{
				uint8(float64(Teal.R)*(1-t) + float64(Deep.R)*t),
				uint8(float64(Teal.G)*(1-t) + float64(Deep.G)*t),
				uint8(float64(Teal.B)*(1-t) + float64(Deep.B)*t), 255}
			c := blend(color.NRGBA{}, bg, sq)
			g := coverage(px, py, func(x, y float64) bool {
				return glyph((x-s/2)/(side*0.39), (y-s/2)/(side*0.39)) < 0
			})
			if g > 0 {
				c = blend(c, White, g*sq)
			}
			img.SetNRGBA(px, py, c)
		}
	}
	return encode(img)
}

func encode(img image.Image) []byte {
	var b bytes.Buffer
	if err := png.Encode(&b, img); err != nil {
		panic(err)
	}
	return b.Bytes()
}
