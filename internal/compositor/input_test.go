package compositor_test

import (
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/Team-Quaver/lumine/internal/compositor"
	"github.com/Team-Quaver/lumine/internal/config"
	"github.com/Team-Quaver/lumine/internal/wl"
)

// startHarnessWithConfig is startHarness with an explicit config body, so a
// test can dial the key-repeat timer down to something a CI run can wait on.
func startHarnessWithConfig(t *testing.T, contents string) (string, *captureRenderer, *compositor.Compositor) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", dir)
	cfgPath := filepath.Join(dir, "config.conf")
	if err := os.WriteFile(cfgPath, []byte(contents), 0o644); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(dir, "lumine-test")

	store, err := config.NewStore(cfgPath)
	if err != nil {
		t.Fatal(err)
	}
	srv, err := wl.NewServer(sock)
	if err != nil {
		t.Fatal(err)
	}
	comp := compositor.New(srv, store)
	comp.RegisterGlobals()
	rd := &captureRenderer{}
	comp.AddOutput(&compositor.Output{Name: "test-0", W: 800, H: 600, Refresh: 60_000, Renderer: rd})
	if err := comp.Listen(sock, "test-input"); err != nil {
		t.Fatal(err)
	}

	stop := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for {
			select {
			case <-stop:
				return
			default:
			}
			comp.Frame()
			time.Sleep(5 * time.Millisecond)
		}
	}()
	t.Cleanup(func() { close(stop); wg.Wait() })
	return sock, rd, comp
}

// mapToplevel binds compositor/shm/seat/xdg, maps a titled toplevel, commits
// one shm frame and (with wantInput) requests keyboard. It returns once the
// compositor has configured the surface, mirroring what a small real client
// does before it expects input.
func mapToplevel(t *testing.T, sock, appID string, wantInput bool) (*wl.Conn, *testClient) {
	t.Helper()
	con, c := dialClient(t, sock)
	c.comp = wl.NewWlCompositor(con, con.AllocID(), 4, nil)
	if err := c.reg.ReqBind(c.names["wl_compositor"], "wl_compositor", 4, &c.comp.Object); err != nil {
		t.Fatal(err)
	}
	c.shm = wl.NewWlShm(con, con.AllocID(), 1, nil)
	if err := c.reg.ReqBind(c.names["wl_shm"], "wl_shm", 1, &c.shm.Object); err != nil {
		t.Fatal(err)
	}
	c.xdg = wl.NewXdgWmBase(con, con.AllocID(), 4, nil)
	if err := c.reg.ReqBind(c.names["xdg_wm_base"], "xdg_wm_base", 4, &c.xdg.Object); err != nil {
		t.Fatal(err)
	}
	c.seat = wl.NewWlSeat(con, con.AllocID(), 5, nil)
	if err := c.reg.ReqBind(c.names["wl_seat"], "wl_seat", 5, &c.seat.Object); err != nil {
		t.Fatal(err)
	}

	c.surf = wl.NewWlSurface(con, con.AllocID(), 4, nil)
	if err := c.comp.ReqCreateSurface(&c.surf.Object); err != nil {
		t.Fatal(err)
	}
	c.xdgSurf = wl.NewXdgSurface(con, con.AllocID(), 4, &surfaceEvents{c})
	if err := c.xdg.ReqGetXdgSurface(&c.xdgSurf.Object, &c.surf.Object); err != nil {
		t.Fatal(err)
	}
	c.toplevel = wl.NewXdgToplevel(con, con.AllocID(), 4, &toplevelEvents{c})
	if err := c.xdgSurf.ReqGetToplevel(&c.toplevel.Object); err != nil {
		t.Fatal(err)
	}
	if err := c.toplevel.ReqSetAppId(appID); err != nil {
		t.Fatal(err)
	}
	if err := c.surf.ReqCommit(); err != nil {
		t.Fatal(err)
	}
	if err := con.Flush(); err != nil {
		t.Fatal(err)
	}
	c.wait(func() bool { return c.configured }, "xdg_surface.configure")
	c.commitShmFrame(120, 90)
	c.wait(func() bool { return c.released }, "wl_buffer.release")

	if wantInput {
		c.kb = wl.NewWlKeyboard(con, con.AllocID(), 5, c)
		if err := c.seat.ReqGetKeyboard(&c.kb.Object); err != nil {
			t.Fatal(err)
		}
		if err := con.Flush(); err != nil {
			t.Fatal(err)
		}
		c.wait(func() bool { return c.keymapSeen }, "wl_keyboard.keymap")
	}
	return con, c
}

// windowCentre returns the middle of the drawn rect for appID from the last
// rendered frame, or false while the window is not mapped.
func windowCentre(rd *captureRenderer, appID string) (float64, float64, bool) {
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		items, _ := rd.snapshot()
		for _, it := range items {
			if it.AppID == appID && it.W > 0 && it.H > 0 {
				return it.X + it.W/2, it.Y + it.H/2, true
			}
		}
		time.Sleep(5 * time.Millisecond)
	}
	return 0, 0, false
}

// pointerSpy records wl_pointer events in arrival order.
type pointerSpy struct {
	mu      sync.Mutex
	kinds   []string
	motions []uint32
	buttons []uint32
}

func (p *pointerSpy) note(kind string) {
	p.mu.Lock()
	p.kinds = append(p.kinds, kind)
	p.mu.Unlock()
}

func (p *pointerSpy) count(kind string) int {
	p.mu.Lock()
	defer p.mu.Unlock()
	var n int
	for _, k := range p.kinds {
		if k == kind {
			n++
		}
	}
	return n
}

func (p *pointerSpy) Enter(o *wl.WlPointer, serial uint32, surface *wl.WlSurface, sx, sy wl.Fixed) {
	p.note("enter")
}
func (p *pointerSpy) Leave(o *wl.WlPointer, serial uint32, surface *wl.WlSurface) { p.note("leave") }
func (p *pointerSpy) Motion(o *wl.WlPointer, time uint32, sx, sy wl.Fixed) {
	p.mu.Lock()
	p.kinds = append(p.kinds, "motion")
	p.motions = append(p.motions, time)
	p.mu.Unlock()
}
func (p *pointerSpy) Button(o *wl.WlPointer, serial, time, button, state uint32) {
	p.mu.Lock()
	p.kinds = append(p.kinds, "button")
	p.buttons = append(p.buttons, time)
	p.mu.Unlock()
}
func (p *pointerSpy) Axis(o *wl.WlPointer, time uint32, axis uint32, value wl.Fixed) {
	p.note("axis")
}
func (p *pointerSpy) Frame(o *wl.WlPointer)                              { p.note("frame") }
func (p *pointerSpy) AxisSource(o *wl.WlPointer, axisSource uint32)      {}
func (p *pointerSpy) AxisStop(o *wl.WlPointer, time uint32, axis uint32) {}
func (p *pointerSpy) AxisDiscrete(o *wl.WlPointer, axis uint32, d int32) {}
func (p *pointerSpy) AxisValue120(o *wl.WlPointer, axis uint32, v int32) {}
func (p *pointerSpy) AxisRelativeDirection(o *wl.WlPointer, axis uint32, dir uint32) {
}
func (p *pointerSpy) Warp(o *wl.WlPointer, sx, sy wl.Fixed) {}

func (p *pointerSpy) snapshot() []string {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]string(nil), p.kinds...)
}

// TestPointerMotionEndsFrameAndCarriesTime pins the two contract points that
// made clients look mouse-dead even though the cursor moved: every motion
// batch must close with wl_pointer.frame (frame-driven toolkits like GTK do
// not apply motion before it), and events must carry the backend timestamp
// instead of a constant zero (acceleration and smooth-scroll code divide by
// elapsed time).
func TestPointerMotionEndsFrameAndCarriesTime(t *testing.T) {
	sock, rd, comp := startHarness(t)
	_, c := mapToplevel(t, sock, "lumine-pointer", false)

	con := c.con
	spy := &pointerSpy{}
	pt := wl.NewWlPointer(con, con.AllocID(), 5, spy)
	if err := c.seat.ReqGetPointer(&pt.Object); err != nil {
		t.Fatal(err)
	}
	// get_pointer is a request with no reply event, so the bind must reach the
	// server before the motion below or it would be routed to zero pointers.
	if err := con.RoundTrip(); err != nil {
		t.Fatal(err)
	}

	cx, cy, ok := windowCentre(rd, "lumine-pointer")
	if !ok {
		t.Fatal("window never drawn")
	}
	comp.MotionAbs(1234, cx, cy)
	c.wait(func() bool { return spy.count("frame") >= 2 && spy.hasMotion(1234) }, "pointer enter+motion+frame")

	kinds := spy.snapshot()
	// The motion must be terminated by a frame event.
	var motionSeen, frameAfter bool
	for _, k := range kinds {
		if k == "motion" {
			motionSeen = true
		}
		if k == "frame" && motionSeen {
			frameAfter = true
		}
	}
	if !motionSeen || !frameAfter {
		t.Fatalf("events %v lack motion+frame pairing", kinds)
	}

	comp.Button(4321, 0x110 /* BTN_LEFT */, true)
	c.wait(func() bool { _, b := spy.buttonTime(); return b == 4321 }, "button timestamp")
}

func (p *pointerSpy) hasMotion(time uint32) bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	for _, m := range p.motions {
		if m == time {
			return true
		}
	}
	return false
}

func (p *pointerSpy) buttonTime() (int, uint32) {
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.buttons) == 0 {
		return 0, 0
	}
	return len(p.buttons), p.buttons[len(p.buttons)-1]
}

// TestClickFocusesTiledWindow hands keyboard input to the window under the
// cursor. Regression: mapping a window already steals layout focus (Tile.Add),
// clicks only moved that focus in stack mode, and every frame re-asserted
// keyboard focus from the layout tree. So clicking the older tiled window
// appeared to focus it for one frame and the keyboard snapped straight back
// to the newest window.
func TestClickFocusesTiledWindow(t *testing.T) {
	sock, rd, comp := startHarness(t)
	_, a := mapToplevel(t, sock, "win-a", true)
	_, b := mapToplevel(t, sock, "win-b", true)

	// The newest window holds layout (and therefore keyboard) focus.
	comp.Key(9, 30, true)
	comp.Key(10, 30, false)
	b.wait(func() bool { return len(b.keyboardKeys) >= 2 }, "keys at the newest window")
	if n := len(a.keyboardKeys); n != 0 {
		t.Fatalf("first window took %d keys before any click", n)
	}

	cx, cy, ok := windowCentre(rd, "win-a")
	if !ok {
		t.Fatal("first window never drawn")
	}
	comp.MotionAbs(11, cx, cy)
	comp.Button(12, 0x110 /* BTN_LEFT */, true)

	a.wait(func() bool { return a.kbEnters >= 2 }, "keyboard enter on the clicked window")
	comp.Key(13, 46, true) // KEY_C
	a.wait(func() bool { return len(a.keyboardKeys) >= 1 }, "key at the clicked window")
	comp.Key(14, 46, false)

	// The frame pump must not snap focus back: the newest window may not see
	// any key after the click moved focus away.
	before := len(b.keyboardKeys)
	time.Sleep(80 * time.Millisecond)
	a.wait(func() bool { return true }, "settle")
	if len(b.keyboardKeys) != before {
		t.Fatalf("keyboard focus snapped back to the newest window after the click")
	}
}

// TestKeyAutoRepeat synthesises the repeat stream the compositor promised via
// wl_keyboard.repeat_info. The nested backend used to lean on the host for
// repeats; on bare DRM evdev only reports the initial press, so holding a
// key typed nothing.
func TestKeyAutoRepeat(t *testing.T) {
	sock, _, comp := startHarnessWithConfig(t, "[input.repeat]\ndelay = 20\nrate = 100\n")
	_, c := mapToplevel(t, sock, "lumine-repeat", true)
	c.wait(func() bool { return c.keyboardEnter }, "wl_keyboard.enter")

	comp.Key(1, 30, true) // KEY_A
	c.wait(func() bool { return len(c.keyboardKeys) >= 3 }, "auto-repeat key events")

	c.mu.Lock()
	n := len(c.keyboardKeys)
	c.mu.Unlock()
	comp.Key(2, 30, false)
	c.wait(func() bool { return len(c.keyboardKeys) > n }, "release event")

	c.mu.Lock()
	keys := append([]keyEvent(nil), c.keyboardKeys...)
	c.mu.Unlock()
	if keys[len(keys)-1].state != wl.WlKeyboardKeyStateReleased {
		t.Fatalf("last key = %+v, want release", keys[len(keys)-1])
	}
	time.Sleep(100 * time.Millisecond)
	c.mu.Lock()
	grown := len(c.keyboardKeys)
	c.mu.Unlock()
	if grown != len(keys) {
		t.Fatalf("repeat kept firing after release: %d -> %d", len(keys), grown)
	}
}

// TestExternalRepeatPassesThrough checks the nested path: when auto-repeat is
// disabled locally (or before our timer takes over), the host's repeated
// press events are forwarded instead of swallowed as duplicates.
func TestExternalRepeatPassesThrough(t *testing.T) {
	sock, _, comp := startHarnessWithConfig(t, "[input.repeat]\nrate = 0\n")
	_, c := mapToplevel(t, sock, "lumine-extrepeat", true)
	c.wait(func() bool { return c.keyboardEnter }, "wl_keyboard.enter")

	comp.Key(1, 30, true)
	comp.Key(2, 30, true) // host repeat
	comp.Key(3, 30, false)
	c.wait(func() bool { return len(c.keyboardKeys) >= 3 }, "press, external repeat, release")
	c.mu.Lock()
	keys := append([]keyEvent(nil), c.keyboardKeys...)
	c.mu.Unlock()
	if len(keys) != 3 ||
		keys[0].state != wl.WlKeyboardKeyStatePressed ||
		keys[1].state != wl.WlKeyboardKeyStatePressed ||
		keys[2].state != wl.WlKeyboardKeyStateReleased {
		t.Fatalf("keys = %+v, want pressed, pressed (repeat), released", keys)
	}
	time.Sleep(60 * time.Millisecond)
	c.mu.Lock()
	grown := len(c.keyboardKeys)
	c.mu.Unlock()
	if grown != 3 {
		t.Fatalf("compositor synthesised repeats although rate = 0")
	}
}
