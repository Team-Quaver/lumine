package render

import (
	"encoding/binary"
	"testing"

	"github.com/Team-Quaver/lumine/internal/shm"
)

func solidImage(w, h int, format shm.Format, c uint32) *shm.Image {
	pix := make([]byte, w*h*4)
	for i := 0; i < w*h; i++ {
		binary.LittleEndian.PutUint32(pix[i*4:], c)
	}
	return &shm.Image{Pix: pix, Width: w, Height: h, Stride: w * 4, Format: format}
}

func patternImage(w, h int) *shm.Image {
	pix := make([]byte, w*h*4)
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			c := uint32(0xff000000) | uint32((x*37)%256)<<16 | uint32((y*53)%256)<<8 | uint32((x+y)*11%256)
			binary.LittleEndian.PutUint32(pix[(y*w+x)*4:], c)
		}
	}
	return &shm.Image{Pix: pix, Width: w, Height: h, Stride: w * 4, Format: shm.ARGB8888}
}

// scene is the shared input for the parity tests: a translucent overlay on top
// of an opaque window, plus an SSD-decorated window, scaled up so nearest
// sampling is exercised.
func scene() []DrawItem {
	return []DrawItem{
		{Image: patternImage(7, 5), X: 9, Y: 7, W: 61, H: 43},
		{Image: solidImage(20, 20, shm.ARGB8888, 0x802060ff), X: 40, Y: 30, W: 80, H: 60, Opacity: 0.75},
		{Image: solidImage(18, 12, shm.XRGB8888, 0x0040c040), X: 120, Y: 12, W: 60, H: 44, Frame: true, Active: true, Title: "ssd"},
	}
}

func TestCPURendererDraws(t *testing.T) {
	r := NewCPU(DefaultTheme())
	r.SetColorSpace(false, "srgb", "srgb", 0)
	fb := NewFramebuffer(200, 120)
	if err := r.Draw(fb, scene()); err != nil {
		t.Fatalf("cpu draw: %v", err)
	}
	// The framebuffer must be filled, and the XRGB window must be opaque even
	// though its fourth byte is zero.
	if got := fb.At(5, 5); got != 0xff0b0b0e {
		t.Fatalf("background = %#x, want the neutral backdrop", got)
	}
	if got := fb.At(150, 30); got&0xff000000 != 0xff000000 {
		t.Fatalf("xrgb window pixel %#x is not opaque", got)
	}
	// SSD decoration: the title bar sits above the content rect.
	if got := fb.At(150, 0); got != DefaultTheme().DecorationActive {
		t.Fatalf("title bar = %#x, want %#x", got, DefaultTheme().DecorationActive)
	}
}

func TestVulkanMatchesCPU(t *testing.T) {
	const w, h = 200, 120
	if ok, why := VulkanAvailable(); !ok {
		t.Skipf("no usable Vulkan device: %s", why)
	}
	items := scene()
	cpu := NewCPU(DefaultTheme())
	cpu.SetColorSpace(false, "srgb", "srgb", 0)
	want := NewFramebuffer(w, h)
	if err := cpu.Draw(want, items); err != nil {
		t.Fatalf("cpu draw: %v", err)
	}

	vk, err := NewVulkan(DefaultTheme(), w, h)
	if err != nil {
		t.Skipf("vulkan unavailable: %v", err)
	}
	defer vk.Close()
	vk.SetColorSpace(false, "srgb", "srgb", 0)
	got := NewFramebuffer(w, h)
	if err := vk.Draw(got, items); err != nil {
		t.Fatalf("vulkan draw: %v", err)
	}
	// The two renderers use the same source-over formula; the GPU rounds in
	// float and the CPU truncates, so allow a couple of code values of slack.
	const tol = 3
	worst, wx, wy := 0, 0, 0
	for y := 0; y < h; y++ {
		for x := 0; x < w; x++ {
			a, b := want.At(x, y), got.At(x, y)
			for s := 0; s < 24; s += 8 {
				d := int(a>>s&0xff) - int(b>>s&0xff)
				if d < 0 {
					d = -d
				}
				if d > worst {
					worst, wx, wy = d, x, y
				}
			}
		}
	}
	if worst > tol {
		t.Fatalf("vulkan differs from cpu by %d at (%d,%d): cpu %#x gpu %#x", worst, wx, wy, want.At(wx, wy), got.At(wx, wy))
	}
}

func TestVulkanUploadsOnSecondFrame(t *testing.T) {
	const w, h = 64, 64
	vk, err := NewVulkan(DefaultTheme(), w, h)
	if err != nil {
		t.Skipf("vulkan unavailable: %v", err)
	}
	defer vk.Close()
	fb := NewFramebuffer(w, h)
	a := solidImage(8, 8, shm.ARGB8888, 0xffff0000)
	if err := vk.Draw(fb, []DrawItem{{Image: a, X: 0, Y: 0, W: 16, H: 16}}); err != nil {
		t.Fatalf("first draw: %v", err)
	}
	b := solidImage(8, 8, shm.ARGB8888, 0xff00ff00)
	if err := vk.Draw(fb, []DrawItem{{Image: b, X: 0, Y: 0, W: 16, H: 16}}); err != nil {
		t.Fatalf("second draw: %v", err)
	}
	if got := fb.At(4, 4); got != 0xff00ff00 {
		t.Fatalf("second frame pixel = %#x, want green", got)
	}
	// The texture for the buffer that disappeared must be evicted while the
	// live buffer's texture remains cached for reuse.
	if _, ok := vk.texture[a]; ok {
		t.Fatalf("texture cache retained the unreferenced first-frame buffer")
	}
	if _, ok := vk.texture[b]; !ok {
		t.Fatalf("texture cache dropped the live second-frame buffer")
	}
}

func TestColorTransformPQ(t *testing.T) {
	fb := NewFramebuffer(1, 1)
	fb.Fill(0xff808080)
	applyColorTransform(fb, colorState{Enabled: true, TF: "pq", Primaries: "bt2020", MaxLuma: 400})
	if got := fb.At(0, 0); got == 0xff808080 {
		t.Fatalf("PQ transform left the pixel untouched")
	}
	// sRGB is the working space: no transform at all.
	fb2 := NewFramebuffer(1, 1)
	fb2.Fill(0xff808080)
	applyColorTransform(fb2, colorState{Enabled: true, TF: "srgb"})
	if got := fb2.At(0, 0); got != 0xff808080 {
		t.Fatalf("sRGB transform changed the pixel to %#x", got)
	}
}
