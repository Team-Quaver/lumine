package drm

import (
	"os"
	"path/filepath"
	"sort"
	"testing"
)

// cardNodes returns the /dev/dri/cardN nodes present on the machine.
func cardNodes(t *testing.T) []string {
	t.Helper()
	matches, _ := filepath.Glob("/dev/dri/card*")
	sort.Strings(matches)
	return matches
}

// TestEnumerateHardware walks every card node and enumerates its connectors,
// CRTCs and planes. It is skipped on machines without DRM and never touches
// the display state (no master, no modeset), so it is safe to run inside a
// live session.
func TestEnumerateHardware(t *testing.T) {
	cards := cardNodes(t)
	if len(cards) == 0 {
		t.Skip("no /dev/dri/card* nodes")
	}
	for _, path := range cards {
		path := path
		t.Run(filepath.Base(path), func(t *testing.T) {
			dev, err := Open(path)
			if err != nil {
				t.Skipf("open %s: %v", path, err)
			}
			defer dev.Close()

			res, err := dev.Resources()
			if err != nil {
				t.Fatalf("Resources: %v", err)
			}
			if len(res.CRTCs) == 0 {
				t.Skip("no CRTCs")
			}
			for _, id := range res.Connectors {
				c, err := dev.Connector(id)
				if err != nil {
					t.Errorf("Connector(%d): %v", id, err)
					continue
				}
				t.Logf("connector %s id=%d connected=%v modes=%d mm=%dx%d props=%d",
					c.Name(), c.ID, c.Connected(), len(c.Modes), c.MMWidth, c.MMHeight, len(c.Props))
			}
			for _, id := range res.CRTCs {
				crtc, err := dev.CRTC(id)
				if err != nil {
					t.Errorf("CRTC(%d): %v", id, err)
					continue
				}
				t.Logf("crtc id=%d mode_valid=%v props=%d", crtc.ID, crtc.ModeValid, len(crtc.Props))
			}
			planes, err := dev.PlaneResources()
			if err != nil {
				t.Errorf("PlaneResources: %v", err)
			}
			for _, id := range planes {
				p, err := dev.Plane(id)
				if err != nil {
					t.Errorf("Plane(%d): %v", id, err)
					continue
				}
				t.Logf("plane id=%d formats=%d xrgb=%v props=%d",
					p.ID, len(p.Formats), p.SupportsFormat(FourccXRGB8888), len(p.Props))
			}
		})
	}
}

// TestDumbBufferHardware allocates a dumb buffer, maps it, exports it as a
// dma-buf and registers a framebuffer. The framebuffer is removed and the
// buffer destroyed before the test returns; no CRTC is touched, so this runs
// harmlessly inside a live session.
func TestDumbBufferHardware(t *testing.T) {
	cards := cardNodes(t)
	if len(cards) == 0 {
		t.Skip("no /dev/dri/card* nodes")
	}
	for _, path := range cards {
		path := path
		t.Run(filepath.Base(path), func(t *testing.T) {
			dev, err := Open(path)
			if err != nil {
				t.Skipf("open %s: %v", path, err)
			}
			defer dev.Close()
			if !dev.DumbBufferSupported() {
				t.Skip("dumb buffers unsupported")
			}
			dumb, err := dev.CreateDumb(64, 64, 32)
			if err != nil {
				t.Fatalf("CreateDumb: %v", err)
			}
			defer dumb.Destroy()
			if len(dumb.Map) != int(dumb.Size) {
				t.Errorf("mapping %d bytes, buffer size %d", len(dumb.Map), dumb.Size)
			}
			if dumb.Pitch < 64*4 {
				t.Errorf("pitch %d < %d", dumb.Pitch, 64*4)
			}
			// Write a pixel and read it back through the mapping.
			dumb.Map[0], dumb.Map[1], dumb.Map[2], dumb.Map[3] = 1, 2, 3, 4
			if dumb.Map[0] != 1 {
				t.Errorf("dumb mapping is not writable")
			}
			fb, err := dev.AddFBFor(dumb, 64, 64, 32, 24)
			if err != nil {
				t.Fatalf("AddFBFor: %v", err)
			}
			if err := dev.RmFB(fb); err != nil {
				t.Errorf("RmFB: %v", err)
			}
			if dev.CanExportDmaBuf() {
				fd, err := dumb.ExportFD()
				if err != nil {
					t.Errorf("ExportFD: %v", err)
				} else {
					os.NewFile(uintptr(fd), "dmabuf").Close()
				}
			}
		})
	}
}
