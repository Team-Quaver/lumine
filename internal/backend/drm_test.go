package backend

import (
	"errors"
	"testing"

	"github.com/Team-Quaver/lumine/internal/drm"
	"github.com/Team-Quaver/lumine/internal/evdev"
	"github.com/Team-Quaver/lumine/internal/render"
	"github.com/Team-Quaver/lumine/internal/wl"
)

// fakeInput records every call the DRM input translation makes, so the tests
// can assert on the compositor-visible stream rather than the ioctl details.
type fakeInput struct {
	mods    []wl.Mods
	keys    []fakeKey
	buttons []fakeKey
	motion  [][2]float64
	abs     [][2]float64
	axis    [][2]float64
}

type fakeKey struct {
	code    uint32
	pressed bool
}

func (f *fakeInput) SetMods(m wl.Mods) { f.mods = append(f.mods, m) }
func (f *fakeInput) Key(code uint32, p bool) bool {
	f.keys = append(f.keys, fakeKey{code, p})
	return true
}
func (f *fakeInput) Button(btn uint32, p bool) { f.buttons = append(f.buttons, fakeKey{btn, p}) }
func (f *fakeInput) Motion(dx, dy float64)     { f.motion = append(f.motion, [2]float64{dx, dy}) }
func (f *fakeInput) MotionAbs(x, y float64)    { f.abs = append(f.abs, [2]float64{x, y}) }
func (f *fakeInput) Axis(dx, dy float64)       { f.axis = append(f.axis, [2]float64{dx, dy}) }

func TestCardNumber(t *testing.T) {
	cases := []struct {
		path string
		want int
	}{
		{"/dev/dri/card0", 0},
		{"/dev/dri/card1", 1},
		{"/dev/dri/card10", 10},
		{"card2", 2},
		{"/dev/dri/renderD128", 1 << 30},
	}
	for _, c := range cases {
		if got := cardNumber(c.path); got != c.want {
			t.Errorf("cardNumber(%q) = %d, want %d", c.path, got, c.want)
		}
	}
}

// TestCardNodesExplicit checks every explicit device source short-circuits the
// /dev/dri glob: the flag, LUMINE_DRM_DEVICE and the first WLR_DRM_DEVICES
// entry.
func TestCardNodesExplicit(t *testing.T) {
	t.Setenv("LUMINE_DRM_DEVICE", "")
	t.Setenv("WLR_DRM_DEVICES", "")

	if got, err := cardNodes("/dev/dri/card9"); err != nil || len(got) != 1 || got[0] != "/dev/dri/card9" {
		t.Fatalf("explicit flag = %v (err %v)", got, err)
	}
	t.Setenv("LUMINE_DRM_DEVICE", "/dev/dri/card7")
	if got, _ := cardNodes(""); len(got) != 1 || got[0] != "/dev/dri/card7" {
		t.Fatalf("LUMINE_DRM_DEVICE = %v", got)
	}
	t.Setenv("LUMINE_DRM_DEVICE", "")
	t.Setenv("WLR_DRM_DEVICES", "/dev/dri/card3:/dev/dri/card4")
	if got, _ := cardNodes(""); len(got) != 1 || got[0] != "/dev/dri/card3" {
		t.Fatalf("WLR_DRM_DEVICES = %v", got)
	}
}

// TestCardNodesAutodetectSorted checks the autodetect path returns the card
// nodes in numeric order when the host has any.
func TestCardNodesAutodetectSorted(t *testing.T) {
	t.Setenv("LUMINE_DRM_DEVICE", "")
	t.Setenv("WLR_DRM_DEVICES", "")
	nodes, err := cardNodes("")
	if err != nil {
		t.Skipf("no /dev/dri/card* nodes: %v", err)
	}
	for i := 1; i < len(nodes); i++ {
		if cardNumber(nodes[i-1]) > cardNumber(nodes[i]) {
			t.Fatalf("card nodes not sorted: %v", nodes)
		}
	}
}

func TestCrtcMaskHas(t *testing.T) {
	res := &drm.Resources{CRTCs: []uint32{10, 20, 30}}
	if !crtcMaskHas(res, 10, 1<<0) {
		t.Errorf("crtc 10 (index 0) not matched by bit 0")
	}
	if !crtcMaskHas(res, 30, 1<<2) {
		t.Errorf("crtc 30 (index 2) not matched by bit 2")
	}
	if crtcMaskHas(res, 10, 1<<1) {
		t.Errorf("bit 1 should not match index 0")
	}
	if crtcMaskHas(res, 99, ^uint32(0)) {
		t.Errorf("unknown crtc id matched")
	}

	// A 40-entry list pushes index 32 out of the uint32 bitmask; the guard must
	// reject it rather than shifting a uint32 by 32.
	long := &drm.Resources{}
	for i := 0; i < 40; i++ {
		long.CRTCs = append(long.CRTCs, uint32(100+i))
	}
	if crtcMaskHas(long, 100+32, ^uint32(0)) {
		t.Errorf("index 32 should be rejected (out of uint32 mask)")
	}
	if !crtcMaskHas(long, 100+31, 1<<31) {
		t.Errorf("index 31 should match the high bit")
	}
}

func TestRequestedMode(t *testing.T) {
	cases := []struct {
		cfg  DRMConfig
		want string
	}{
		{DRMConfig{}, "the connector's modes"},
		{DRMConfig{Width: 1920, Height: 1080}, "1920x1080"},
		{DRMConfig{Width: 1920, Height: 1080, RefreshMHz: 60000}, "1920x1080@60000mHz"},
	}
	for _, c := range cases {
		if got := requestedMode(c.cfg); got != c.want {
			t.Errorf("requestedMode(%+v) = %q, want %q", c.cfg, got, c.want)
		}
	}
}

func TestConnectionWord(t *testing.T) {
	if got := connectionWord(&drm.Connector{}); got != "not connected" {
		t.Errorf("disconnected = %q", got)
	}
	if got := connectionWord(&drm.Connector{Modes: []drm.Mode{{HDisplay: 1}}}); got != "connected but reports no modes" {
		t.Errorf("connected = %q", got)
	}
}

// TestCopyScanoutSamePitch checks a framebuffer whose stride equals the dumb
// pitch copies byte for byte.
func TestCopyScanoutSamePitch(t *testing.T) {
	fb := render.NewFramebuffer(4, 3)
	fill(fb.Pix)
	d := &drm.Dumb{Pitch: uint32(fb.Stride), Map: make([]byte, fb.Stride*3)}
	copyScanout(d, fb)
	if string(d.Map) != string(fb.Pix) {
		t.Fatalf("copy mismatch: %v vs %v", d.Map[:16], fb.Pix[:16])
	}
}

// TestCopyScanoutPitchLarger checks the padding between rows stays untouched
// when the dumb buffer's pitch exceeds the framebuffer stride.
func TestCopyScanoutPitchLarger(t *testing.T) {
	fb := render.NewFramebuffer(4, 3)
	fill(fb.Pix)
	pitch := fb.Stride + 16
	d := &drm.Dumb{Pitch: uint32(pitch), Map: make([]byte, pitch*3)}
	copyScanout(d, fb)
	for y := 0; y < fb.Height; y++ {
		got := d.Map[y*pitch : y*pitch+fb.Stride]
		want := fb.Pix[y*fb.Stride : (y+1)*fb.Stride]
		if string(got) != string(want) {
			t.Fatalf("row %d mismatch", y)
		}
		for _, b := range d.Map[y*pitch+fb.Stride : (y+1)*pitch] {
			if b != 0 {
				t.Fatalf("row %d padding byte = %d, want 0", y, b)
			}
		}
	}
}

// TestCopyScanoutTruncatesRow checks a pitch narrower than one framebuffer row
// copies only what fits instead of writing past the mapping.
func TestCopyScanoutTruncatesRow(t *testing.T) {
	fb := render.NewFramebuffer(8, 2) // 32-byte rows
	fill(fb.Pix)
	d := &drm.Dumb{Pitch: 16, Map: make([]byte, 16*2)}
	copyScanout(d, fb)
	if string(d.Map) != string(fb.Pix[:16])+string(fb.Pix[32:48]) {
		t.Fatalf("truncated copy = %v", d.Map)
	}
}

// TestCopyScanoutShortMap checks a mapping too small for even one row is left
// alone rather than panicking.
func TestCopyScanoutShortMap(t *testing.T) {
	fb := render.NewFramebuffer(4, 4)
	d := &drm.Dumb{Pitch: 16, Map: make([]byte, 8)}
	copyScanout(d, fb) // must not panic
}

func fill(b []byte) {
	for i := range b {
		b[i] = byte(i%251 + 1)
	}
}

// TestNextBuffer checks the scanout ring never hands back the buffer on screen
// or the one in flight, and reports exhaustion when both are taken.
func TestNextBuffer(t *testing.T) {
	b := &DRM{bufs: make([]*scanoutBuf, 3), scanout: 0, queued: 1}
	if got := b.nextBuffer(); got != 2 {
		t.Errorf("triple buffer free = %d, want 2", got)
	}
	b = &DRM{bufs: make([]*scanoutBuf, 2), scanout: 0, queued: 1}
	if got := b.nextBuffer(); got != -1 {
		t.Errorf("double buffer exhausted = %d, want -1", got)
	}
	b = &DRM{bufs: make([]*scanoutBuf, 2), scanout: 1, queued: -1}
	if got := b.nextBuffer(); got != 0 {
		t.Errorf("idle buffer = %d, want 0", got)
	}
}

// TestFatalError checks the sentinel used to stop card autodetection unwraps to
// the underlying cause and keeps its message.
func TestFatalError(t *testing.T) {
	cause := errors.New("connector HDMI-A-1 is not connected")
	err := fatal(cause)
	if err.Error() != cause.Error() {
		t.Errorf("Error() = %q, want %q", err.Error(), cause.Error())
	}
	var fe fatalError
	if !errors.As(err, &fe) {
		t.Fatalf("errors.As failed to match fatalError")
	}
	if !errors.Is(err, cause) {
		t.Errorf("errors.Is did not reach the wrapped cause")
	}
	if errors.As(cause, &fe) {
		t.Errorf("a plain error should not match fatalError")
	}
}

// TestScaleAxis checks the device-range to output-range mapping, including
// clamping outside the range and degenerate ranges.
func TestScaleAxis(t *testing.T) {
	info := evdev.AbsInfo{Minimum: 0, Maximum: 1000}
	cases := []struct {
		value  int32
		length int
		want   float64
	}{
		{0, 2000, 0},
		{500, 2000, 1000},
		{1000, 2000, 2000},
		{-50, 2000, 0},     // clamp low
		{5000, 2000, 2000}, // clamp high
		{500, 0, 0},        // no output
	}
	for _, c := range cases {
		if got := scaleAxis(info, c.value, c.length); got != c.want {
			t.Errorf("scaleAxis(%d,%d) = %v, want %v", c.value, c.length, got, c.want)
		}
	}
	if got := scaleAxis(evdev.AbsInfo{Minimum: 5, Maximum: 5}, 100, 200); got != 0 {
		t.Errorf("degenerate range = %v, want 0", got)
	}
}

// TestHandleKeyRoutesButtonsAndKeys checks the BTN_MISC split: buttons go to
// wl_pointer, keys to wl_keyboard.
func TestHandleKeyRoutesButtonsAndKeys(t *testing.T) {
	in := &fakeInput{}
	b := &DRM{in: in}
	b.handleKey(nil, evdev.BtnLeft, true)
	b.handleKey(nil, 30 /* KEY_A */, true)

	if len(in.buttons) != 1 || in.buttons[0].code != evdev.BtnLeft || !in.buttons[0].pressed {
		t.Fatalf("buttons = %+v", in.buttons)
	}
	if len(in.keys) != 1 || in.keys[0].code != 30 || !in.keys[0].pressed {
		t.Fatalf("keys = %+v", in.keys)
	}
	if len(in.mods) != 0 {
		t.Fatalf("a non-modifier key should not send SetMods: %+v", in.mods)
	}
}

// TestUpdateMods checks modifier keys recompute the modifier state on both
// press and release.
func TestUpdateMods(t *testing.T) {
	in := &fakeInput{}
	b := &DRM{in: in}
	b.handleKey(nil, evdev.KeyLeftShift, true)
	b.handleKey(nil, evdev.KeyLeftCtrl, true)
	b.handleKey(nil, evdev.KeyLeftShift, false)

	if len(in.mods) != 3 {
		t.Fatalf("SetMods calls = %d, want 3", len(in.mods))
	}
	if got := in.mods[0]; !got.Shift || got.Ctrl || got.Alt {
		t.Errorf("after shift press = %+v", got)
	}
	if got := in.mods[1]; !got.Shift || !got.Ctrl {
		t.Errorf("after ctrl press = %+v", got)
	}
	if got := in.mods[2]; got.Shift || !got.Ctrl {
		t.Errorf("after shift release = %+v", got)
	}
}

// TestHandleRelAndFlush checks relative motion, wheel and hi-res wheel are
// accumulated and then applied as one batch per SYN_REPORT.
func TestHandleRelAndFlush(t *testing.T) {
	in := &fakeInput{}
	b := &DRM{in: in}
	b.handleRel(evdev.RelX, 5)
	b.handleRel(evdev.RelY, -3)
	b.handleRel(evdev.RelWheel, 1)          // one notch = wheelStep
	b.handleRel(evdev.RelWheelHiRes, 120)   // 120 hi-res units = one notch
	b.handleRel(evdev.RelHWheelHiRes, -240) // two notches left
	b.flushPointer()

	if len(in.motion) != 1 || in.motion[0] != [2]float64{5, -3} {
		t.Fatalf("motion = %v", in.motion)
	}
	if len(in.axis) != 1 {
		t.Fatalf("axis calls = %v", in.axis)
	}
	wantX := -240.0 / 120.0 * wheelStep
	wantY := wheelStep + 120.0/120.0*wheelStep
	if in.axis[0] != [2]float64{wantX, wantY} {
		t.Fatalf("axis = %v, want [%v %v]", in.axis[0], wantX, wantY)
	}

	// A second flush with nothing accumulated must not emit anything.
	b.flushPointer()
	if len(in.motion) != 1 || len(in.axis) != 1 {
		t.Fatalf("empty flush produced events: %v %v", in.motion, in.axis)
	}
}

// TestHandleAbs checks absolute devices scale their coordinates onto the output
// and only report on flush.
func TestHandleAbs(t *testing.T) {
	in := &fakeInput{}
	b := &DRM{in: in, w: 2000, h: 1000}
	d := &inputDevice{axes: map[uint16]evdev.AbsInfo{
		evdev.AbsX: {Minimum: 0, Maximum: 1000},
		evdev.AbsY: {Minimum: 0, Maximum: 500},
	}}
	b.handleAbs(d, evdev.AbsX, 500)
	b.handleAbs(d, evdev.AbsY, 250)
	if len(in.abs) != 0 {
		t.Fatalf("absolute motion reported before flush")
	}
	b.flushPointer()
	if len(in.abs) != 1 || in.abs[0] != [2]float64{1000, 500} {
		t.Fatalf("abs = %v, want [[1000 500]]", in.abs)
	}
	if len(in.motion) != 0 {
		t.Fatalf("absolute device produced relative motion: %v", in.motion)
	}
}

// TestSynDroppedDiscardsReport checks a dropped report is thrown away rather
// than applied as a bogus delta.
func TestSynDroppedDiscardsReport(t *testing.T) {
	in := &fakeInput{}
	b := &DRM{in: in}
	b.handleRel(evdev.RelX, 42)
	b.handleEvent(nil, evdev.Event{Type: evdev.EvSyn, Code: evdev.SynDropped})
	b.flushPointer()
	if len(in.motion) != 0 {
		t.Fatalf("dropped report still applied: %v", in.motion)
	}
}

// TestHandleEventSynReport checks a SYN_REPORT drives the flush.
func TestHandleEventSynReport(t *testing.T) {
	in := &fakeInput{}
	b := &DRM{in: in}
	b.handleEvent(nil, evdev.Event{Type: evdev.EvRel, Code: evdev.RelX, Value: 7})
	b.handleEvent(nil, evdev.Event{Type: evdev.EvSyn, Code: evdev.SynReport})
	if len(in.motion) != 1 || in.motion[0] != [2]float64{7, 0} {
		t.Fatalf("motion = %v", in.motion)
	}
}

// make sure fakeInput satisfies the interface the DRM backend depends on.
var _ Input = (*fakeInput)(nil)
