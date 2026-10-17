// Package render contains Lumine's renderers. Both the Vulkan renderer and the
// software fallback draw into a Framebuffer: a CPU-visible XRGB8888 surface
// that the backend hands to the display controller (DRM dumb buffer), to
// another compositor (nested wl_shm buffer) or keeps in memory (headless).
package render

import (
	"bytes"
	"encoding/binary"
	"fmt"
	"image"
	"image/color"
	"image/png"

	"github.com/Team-Quaver/lumine/internal/shm"
)

// Framebuffer is a little-endian XRGB8888 pixel buffer. One uint32 pixel is
// laid out as 0xAARRGGBB, i.e. bytes B, G, R, X, which is what wl_shm
// ARGB8888/XRGB8888 and DRM_FORMAT_XRGB8888 both expect.
type Framebuffer struct {
	Pix    []byte
	Stride int
	Width  int
	Height int
}

func NewFramebuffer(w, h int) *Framebuffer {
	stride := w * 4
	return &Framebuffer{Pix: make([]byte, stride*h), Stride: stride, Width: w, Height: h}
}

// Wrap wraps an existing mapping (dumb buffer, shm pool, ...).
func Wrap(pix []byte, stride, w, h int) *Framebuffer {
	return &Framebuffer{Pix: pix, Stride: stride, Width: w, Height: h}
}

func (fb *Framebuffer) Set(x, y int, c uint32) {
	if x < 0 || y < 0 || x >= fb.Width || y >= fb.Height {
		return
	}
	o := y*fb.Stride + x*4
	binary.LittleEndian.PutUint32(fb.Pix[o:], c)
}

func (fb *Framebuffer) At(x, y int) uint32 {
	if x < 0 || y < 0 || x >= fb.Width || y >= fb.Height {
		return 0
	}
	return binary.LittleEndian.Uint32(fb.Pix[y*fb.Stride+x*4:])
}

// Fill paints the whole buffer.
func (fb *Framebuffer) Fill(c uint32) {
	row := make([]byte, fb.Stride)
	for x := 0; x < fb.Width; x++ {
		binary.LittleEndian.PutUint32(row[x*4:], c)
	}
	for y := 0; y < fb.Height; y++ {
		copy(fb.Pix[y*fb.Stride:], row)
	}
}

// FillRect paints a sub-rectangle, clipped to the buffer.
func (fb *Framebuffer) FillRect(x, y, w, h int, c uint32) {
	if w <= 0 || h <= 0 {
		return
	}
	for yy := y; yy < y+h; yy++ {
		for xx := x; xx < x+w; xx++ {
			fb.Set(xx, yy, c)
		}
	}
}

// Snapshot returns a copy of the pixels.
func (fb *Framebuffer) Snapshot() []byte {
	out := make([]byte, len(fb.Pix))
	copy(out, fb.Pix)
	return out
}

// RGBA converts the buffer to image.RGBA (for PNG dumps and test assertions).
func (fb *Framebuffer) RGBA() *image.RGBA {
	img := image.NewRGBA(image.Rect(0, 0, fb.Width, fb.Height))
	for y := 0; y < fb.Height; y++ {
		for x := 0; x < fb.Width; x++ {
			p := fb.At(x, y)
			img.SetRGBA(x, y, color.RGBA{
				R: uint8(p >> 16), G: uint8(p >> 8), B: uint8(p), A: 0xff,
			})
		}
	}
	return img
}

// EncodePNG renders the framebuffer as PNG (debug screenshots, lumine-shot).
func (fb *Framebuffer) EncodePNG() ([]byte, error) {
	var buf bytes.Buffer
	if err := png.Encode(&buf, fb.RGBA()); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// ScaleFromImage blits src (already an ARGB8888 image) into the framebuffer
// with nearest-neighbour scaling and source-over compositing. It is shared by
// the software renderer and used to verify the GPU path in tests.
func (fb *Framebuffer) ScaleFromImage(img *shm.Image, x, y, w, h int, alphaScale uint16) {
	if img == nil || w <= 0 || h <= 0 {
		return
	}
	for dy := 0; dy < h; dy++ {
		ty := y + dy
		if ty < 0 || ty >= fb.Height {
			continue
		}
		sy := dy * img.Height / h
		if sy >= img.Height {
			sy = img.Height - 1
		}
		src := img.Pix[sy*img.Stride:]
		for dx := 0; dx < w; dx++ {
			tx := x + dx
			if tx < 0 || tx >= fb.Width {
				continue
			}
			sx := dx * img.Width / w
			if sx >= img.Width {
				sx = img.Width - 1
			}
			sp := binary.LittleEndian.Uint32(src[sx*4:])
			srcA := uint32(uint8(sp >> 24))
			if img.Format == shm.XRGB8888 {
				// XRGB buffers leave the fourth byte undefined.
				srcA = 0xff
			}
			if alphaScale != 0xffff {
				srcA = srcA * uint32(alphaScale) / 0xffff
			}
			if srcA == 0 {
				continue
			}
			dp := fb.At(tx, ty)
			if srcA == 0xff {
				fb.Set(tx, ty, sp&0x00ffffff|0xff000000)
				continue
			}
			r := blendChan(uint8(dp>>16), uint8(sp>>16), srcA)
			g := blendChan(uint8(dp>>8), uint8(sp>>8), srcA)
			b := blendChan(uint8(dp), uint8(sp), srcA)
			fb.Set(tx, ty, 0xff000000|uint32(r)<<16|uint32(g)<<8|uint32(b))
		}
	}
}

func blendChan(dst, src uint8, a uint32) uint8 {
	return uint8((uint32(src)*a + uint32(dst)*(0xff-a)) / 0xff)
}

// Bounds returns the overlap of a rect with the framebuffer, as
// (x0, y0, x1, y1). Used by both renderers for clipping.
func (fb *Framebuffer) Bounds(x, y, w, h int) (int, int, int, int) {
	x0, y0, x1, y1 := x, y, x+w, y+h
	if x0 < 0 {
		x0 = 0
	}
	if y0 < 0 {
		y0 = 0
	}
	if x1 > fb.Width {
		x1 = fb.Width
	}
	if y1 > fb.Height {
		y1 = fb.Height
	}
	if x1 < x0 {
		x1 = x0
	}
	if y1 < y0 {
		y1 = y0
	}
	return x0, y0, x1, y1
}

// String makes framebuffers printable in logs and test failures.
func (fb *Framebuffer) String() string {
	return fmt.Sprintf("framebuffer %dx%d stride %d", fb.Width, fb.Height, fb.Stride)
}
