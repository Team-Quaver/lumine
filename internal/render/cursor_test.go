package render

import "testing"

func TestDrawCursorPaintsHotspot(t *testing.T) {
	const bg = 0xff101014
	fill := DefaultTheme().Cursor
	fb := NewFramebuffer(64, 64)
	fb.Fill(bg)

	DrawCursor(fb, 10, 10, fill)

	// The hotspot is the arrow tip, which must be the theme fill colour.
	if got := fb.At(10, 10); got != fill {
		t.Fatalf("hotspot = %#x, want fill %#x", got, fill)
	}
	// The arrow carries an outline, so somebody adjacent to the tip is dark.
	if got := fb.At(9, 10); got == bg || got == fill {
		t.Fatalf("outline pixel = %#x, want neither background nor fill", got)
	}
	// Pixels far outside the arrow are untouched.
	if got := fb.At(50, 50); got != bg {
		t.Fatalf("far pixel = %#x, want background %#x", got, bg)
	}
}

func TestDrawCursorCoversExpectedBounds(t *testing.T) {
	w, h := CursorSize()
	if w <= 0 || h <= 0 {
		t.Fatalf("CursorSize = %dx%d, want positive", w, h)
	}
	fb := NewFramebuffer(64, 64)
	DrawCursor(fb, 4, 4, 0xffffffff)
	painted := 0
	for y := 0; y < 64; y++ {
		for x := 0; x < 64; x++ {
			if fb.At(x, y) != 0 {
				painted++
			}
		}
	}
	if painted == 0 {
		t.Fatal("DrawCursor painted nothing")
	}
	// The outline only reaches one pixel past the polygon, so the painted area
	// must fit inside the declared bounding box plus that border.
	maxW, maxH := w+2, h+2
	for y := 0; y < 64; y++ {
		for x := 0; x < 64; x++ {
			if fb.At(x, y) == 0 {
				continue
			}
			if x < 4-1 || y < 4-1 || x > 4+maxW || y > 4+maxH {
				t.Fatalf("painted pixel (%d,%d) outside the cursor box", x, y)
			}
		}
	}
}

func TestDrawCursorClipsToFramebuffer(t *testing.T) {
	fb := NewFramebuffer(16, 16)
	// Every corner must be safe: the rasteriser relies on Framebuffer.Set to
	// clip, so a panic or a slice bounds error would surface here.
	for _, p := range [][2]int{{0, 0}, {15, 0}, {0, 15}, {15, 15}, {-4, -4}, {20, 20}} {
		DrawCursor(fb, p[0], p[1], 0xffffffff)
	}
	if fb.At(0, 0) == 0 {
		t.Fatal("cursor drawn at the origin painted nothing")
	}
}

func TestDrawCursorNilFramebuffer(t *testing.T) {
	DrawCursor(nil, 0, 0, 0xffffffff)
}
