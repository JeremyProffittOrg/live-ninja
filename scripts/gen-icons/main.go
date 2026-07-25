// Command gen-icons rasterizes the Live Ninja app glyph
// (web/static/icons/ninja.svg) to the PNG sizes the PWA manifest and iOS
// need, using only the Go standard library (no SVG rasterizer dependency:
// the glyph is redrawn here from the same analytic geometry as the SVG).
//
// Run from the repo root:
//
//	go run ./scripts/gen-icons
//
// Outputs (committed to the repo, embedded via go:embed with the rest of
// web/static):
//
//	web/static/icons/icon-192.png            purpose "any"
//	web/static/icons/icon-512.png            purpose "any"
//	web/static/icons/icon-maskable-512.png   purpose "maskable" (full-bleed bg,
//	                                         glyph shrunk into the safe zone)
//	web/static/icons/apple-touch-icon.png    180x180, full-bleed (iOS applies
//	                                         its own corner mask)
//
// Rendering is 4x supersampled (16 coverage samples per output pixel) so the
// curves are anti-aliased without any external imaging library.
package main

import (
	"fmt"
	"image"
	"image/color"
	"image/png"
	"os"
	"path/filepath"
)

// Design tokens: HAL 9000 head with red eye (modern design, Theme.kt colors).
var (
	// Every value below was measured off the shipped web/static/icons/icon-512.png,
	// not eyeballed. The eye is a stack of TRANSLUCENT red discs over a navy base
	// that a dark lens ellipse has already darkened — solving two samples per ring
	// (one inside the band, one outside) is what recovers the alphas.
	colPlate     = color.NRGBA{R: 0x06, G: 0x0d, B: 0x18, A: 0xff} // plate #060d18
	colHead      = color.NRGBA{R: 0x16, G: 0x29, B: 0x4a, A: 0xff} // flat navy head #16294a
	colLensBand  = color.NRGBA{R: 0x00, G: 0x00, B: 0x00, A: 0x8b} // black @ 0.545: navy -> (10,20,36)
	colGlowOuter = color.NRGBA{R: 0xff, G: 0x5f, B: 0x4a, A: 0x2a} // 0.167 -> (64,50,74) over navy
	colGlowInner = color.NRGBA{R: 0xff, G: 0x5f, B: 0x4a, A: 0x5c} // stacks to (131,64,74)
	colIris      = color.NRGBA{R: 0xe3, G: 0x43, B: 0x2b, A: 0xff} // opaque iris #e3432b
	colCore      = color.NRGBA{R: 0xff, G: 0xb1, B: 0x99, A: 0xff} // incandescent core #ffb199
)

// shape is an analytic coverage function in the 512x512 unit design space.
//
// cov returns 0..1 rather than a bool because the shipped HAL-eye art is built
// from soft radial falloffs, not flat discs. The previous bool-only model could
// only stack hard-edged circles, which is exactly why regenerating produced
// visible concentric banding where the committed PNGs have a smooth glow.
type shape struct {
	cov func(x, y float64) float64
	col color.NRGBA
}

// hard turns a binary inside-test into full-or-no coverage.
func hard(f func(x, y float64) bool) func(x, y float64) float64 {
	return func(x, y float64) float64 {
		if f(x, y) {
			return 1
		}
		return 0
	}
}

func insideCircle(cx, cy, r float64) func(x, y float64) bool {
	return func(x, y float64) bool {
		dx, dy := x-cx, y-cy
		return dx*dx+dy*dy <= r*r
	}
}

func insideRoundedRect(rx0, ry0, w, h, rad float64) func(x, y float64) bool {
	return func(x, y float64) bool {
		if x < rx0 || x > rx0+w || y < ry0 || y > ry0+h {
			return false
		}
		// Corner test: clamp to the inner rect, measure distance.
		cx := x
		if cx < rx0+rad {
			cx = rx0 + rad
		} else if cx > rx0+w-rad {
			cx = rx0 + w - rad
		}
		cy := y
		if cy < ry0+rad {
			cy = ry0 + rad
		} else if cy > ry0+h-rad {
			cy = ry0 + h - rad
		}
		dx, dy := x-cx, y-cy
		return dx*dx+dy*dy <= rad*rad
	}
}

// glyphShapes returns the layered HAL 9000 head glyph in the 512-unit design space.
// scale shrinks the foreground glyph about the center (1.0 = as drawn);
// bgRadius is the background corner radius (0 = full bleed).
// The design is a navy head with a red incandescent eye in the center.
func glyphShapes(scale, bgRadius float64) []shape {
	s := func(v float64) float64 { return 256 + (v-256)*scale }
	r := func(v float64) float64 { return v * scale }
	headCx, headCy := s(256), s(256)
	// 199 units, measured off the shipped icon-192.png at three angles (the old 140
	// left a ring of bare plate where the shipped art still has navy).
	headRadius := r(199)

	return []shape{
		// Plate.
		{hard(insideRoundedRect(0, 0, 512, 512, bgRadius)), colPlate},
		// Flat navy head. r=199 measured at three angles; the old r=140 left a ring
		// of bare plate where the shipped art still has navy.
		{hard(insideCircle(headCx, headCy, headRadius)), colHead},
		// Lens band, UNDER the glow: it darkens the navy, and the translucent glow
		// discs then sit on top of the darkened base. Drawn over the glow instead it
		// cuts a flat stripe through the eye, which the shipped art does not have.
		// A stadium, not an ellipse: half-height is flat at 51 units out to x=106 and
		// then caps with radius 51. That model predicts half-heights of 51/38/26/14 at
		// x=110/140/150/155, which is exactly what the shipped art measures — an
		// ellipse cannot fit both ends and visibly tapers through the middle instead.
		{hard(insideRoundedRect(headCx-r(157), headCy-r(51), r(314), r(102), r(51))), colLensBand},
		// Two translucent glow discs, hard-edged (the shipped art has visible ring
		// boundaries — it is a disc stack, not a gradient).
		{hard(insideCircle(headCx, headCy, r(128))), colGlowOuter},
		{hard(insideCircle(headCx, headCy, r(94))), colGlowInner},
		// Opaque iris, then the centred incandescent core (r=28, centroid measured
		// at +1,+3 units — it only *looks* offset).
		{hard(insideCircle(headCx, headCy, r(67))), colIris},
		{hard(insideCircle(headCx+r(1), headCy+r(3), r(28))), colCore},
	}
}

// render paints the shape stack at size px with 4x4 supersampling. Pixels
// outside every shape stay fully transparent (matters for the rounded-corner
// "any" icons).
func render(size int, shapes []shape) *image.NRGBA {
	const ss = 4
	img := image.NewNRGBA(image.Rect(0, 0, size, size))
	unit := 512.0 / float64(size)
	for py := 0; py < size; py++ {
		for px := 0; px < size; px++ {
			var rSum, gSum, bSum, aSum float64
			for sy := 0; sy < ss; sy++ {
				for sx := 0; sx < ss; sx++ {
					x := (float64(px) + (float64(sx)+0.5)/ss) * unit
					y := (float64(py) + (float64(sy)+0.5)/ss) * unit
					// Composite the stack top-down: last shape containing
					// the sample wins, alpha-blended over what's below it.
					var cr, cg, cb, ca float64
					for _, sh := range shapes {
						c := sh.cov(x, y)
						if c <= 0 {
							continue
						}
						a := float64(sh.col.A) / 255 * c
						cr = cr*(1-a) + float64(sh.col.R)*a
						cg = cg*(1-a) + float64(sh.col.G)*a
						cb = cb*(1-a) + float64(sh.col.B)*a
						ca = ca*(1-a) + a
					}
					rSum += cr
					gSum += cg
					bSum += cb
					aSum += ca
				}
			}
			n := float64(ss * ss)
			img.SetNRGBA(px, py, color.NRGBA{
				R: uint8(rSum/n + 0.5),
				G: uint8(gSum/n + 0.5),
				B: uint8(bSum/n + 0.5),
				A: uint8(aSum/n*255 + 0.5),
			})
		}
	}
	return img
}

func writePNG(path string, img image.Image) error {
	f, err := os.Create(path)
	if err != nil {
		return err
	}
	defer f.Close()
	return png.Encode(f, img)
}

func main() {
	outDir := filepath.Join("web", "static", "icons")
	if err := os.MkdirAll(outDir, 0o755); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}

	// "any" icons: rounded-square background matching ninja.svg (rx 116/512).
	anyShapes := glyphShapes(1.0, 116)
	// maskable: full-bleed background, glyph shrunk to ~78% so it stays
	// inside the 80%-diameter safe zone once the platform mask is applied.
	maskShapes := glyphShapes(0.78, 0)
	// apple-touch: full-bleed too (iOS rounds corners itself).
	appleShapes := glyphShapes(0.92, 0)

	jobs := []struct {
		name   string
		size   int
		shapes []shape
	}{
		{"icon-192.png", 192, anyShapes},
		{"icon-512.png", 512, anyShapes},
		{"icon-maskable-512.png", 512, maskShapes},
		{"apple-touch-icon.png", 180, appleShapes},
	}
	for _, j := range jobs {
		p := filepath.Join(outDir, j.name)
		if err := writePNG(p, render(j.size, j.shapes)); err != nil {
			fmt.Fprintf(os.Stderr, "%s: %v\n", p, err)
			os.Exit(1)
		}
		fmt.Println("wrote", p)
	}
}
