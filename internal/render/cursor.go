package render

import (
	"math"
	"sort"
)

// Software cursor.
//
// Lumine composites the pointer into the finished framebuffer instead of
// programming a DRM cursor plane. A hardware plane would need its own dumb
// buffer, an ARGB format probe per driver and the legacy cursor ioctl, which
// some nvidia-drm configurations reject; the software path is identical on
// every GPU and shows up in the nested and headless outputs for free.

// cursorArrow traces the default pointer, clockwise from the tip. Coordinates
// are in a 14x21 box whose top-left pixel is the hotspot, which is why the tip
// sits at the origin.
var cursorArrow = [][2]int{
	{0, 0}, {0, 14}, {4, 10}, {7, 20}, {9, 19}, {6, 9}, {13, 7},
}

// cursorOutline is painted one pixel outside the arrow so it stays legible on
// light backgrounds; the fill comes from the theme.
const cursorOutline = 0xff101010

// CursorSize returns the default cursor's bounding box in pixels.
func CursorSize() (w, h int) { return 14, 21 }

// DrawCursor composites the default arrow so its hotspot lands on (x, y). It is
// a no-op for a nil framebuffer, matching the other framebuffer helpers.
func DrawCursor(fb *Framebuffer, x, y int, fill uint32) {
	if fb == nil {
		return
	}
	// Dilating the polygon by one pixel gives an outline without a stroke
	// rasteriser: fill it once per neighbour offset, then fill it on top.
	for dy := -1; dy <= 1; dy++ {
		for dx := -1; dx <= 1; dx++ {
			fillPolygon(fb, cursorArrow, x+dx, y+dy, cursorOutline)
		}
	}
	fillPolygon(fb, cursorArrow, x, y, fill)
}

// fillPolygon scanline-fills a convex or concave simple polygon translated by
// (ox, oy), using the even-odd rule. Sampling at pixel centres keeps shared
// vertices from producing stray spans.
func fillPolygon(fb *Framebuffer, pts [][2]int, ox, oy int, c uint32) {
	if len(pts) < 3 {
		return
	}
	minY, maxY := pts[0][1], pts[0][1]
	for _, p := range pts {
		if p[1] < minY {
			minY = p[1]
		}
		if p[1] > maxY {
			maxY = p[1]
		}
	}
	var xs []float64
	for py := minY; py <= maxY; py++ {
		sy := float64(py) + 0.5
		xs = xs[:0]
		for i := range pts {
			a, b := pts[i], pts[(i+1)%len(pts)]
			ay, by := float64(a[1]), float64(b[1])
			if (sy >= ay && sy < by) || (sy >= by && sy < ay) {
				t := (sy - ay) / (by - ay)
				xs = append(xs, float64(a[0])+t*float64(b[0]-a[0]))
			}
		}
		if len(xs) < 2 {
			continue
		}
		sort.Float64s(xs)
		for i := 0; i+1 < len(xs); i += 2 {
			x0 := int(math.Ceil(xs[i] - 0.5))
			x1 := int(math.Floor(xs[i+1] - 0.5))
			for x := x0; x <= x1; x++ {
				fb.Set(x+ox, py+oy, c)
			}
		}
	}
}
