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

// Design tokens (mockups/web/*.html --ln-* custom properties).
var (
	colBg    = color.NRGBA{R: 0x06, G: 0x0d, B: 0x18, A: 0xff} // --ln-navy-900
	colHead  = color.NRGBA{R: 0x14, G: 0x25, B: 0x44, A: 0xff} // --ln-surface-raised
	colBand  = color.NRGBA{R: 0x0a, G: 0x14, B: 0x24, A: 0xff} // --ln-navy
	colTeal  = color.NRGBA{R: 0x22, G: 0xe0, B: 0xd0, A: 0xff} // --ln-teal
	colTeal2 = color.NRGBA{R: 0x22, G: 0xe0, B: 0xd0, A: 0xd9} // headband, 85% alpha
)

// shape is an analytic inside-test in the 512x512 unit design space.
type shape struct {
	inside func(x, y float64) bool
	col    color.NRGBA
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

// glyphShapes returns the layered glyph in the 512-unit design space.
// scale shrinks the foreground glyph about the center (1.0 = as drawn in
// ninja.svg); bgRadius is the background corner radius (0 = full bleed).
func glyphShapes(scale, bgRadius float64) []shape {
	s := func(v float64) float64 { return 256 + (v-256)*scale }
	r := func(v float64) float64 { return v * scale }
	return []shape{
		{insideRoundedRect(0, 0, 512, 512, bgRadius), colBg},
		{insideCircle(s(256), s(264), r(148)), colHead},
		{insideRoundedRect(s(108), s(200), r(296), r(96), r(48)), colBand},
		{insideRoundedRect(s(126), s(184), r(260), r(14), r(7)), colTeal2},
		{insideCircle(s(204), s(248), r(24)), colTeal},
		{insideCircle(s(308), s(248), r(24)), colTeal},
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
						if !sh.inside(x, y) {
							continue
						}
						a := float64(sh.col.A) / 255
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
