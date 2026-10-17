package compositor_test

import (
	"encoding/binary"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/Team-Quaver/lumine/internal/compositor"
	"github.com/Team-Quaver/lumine/internal/config"
	"github.com/Team-Quaver/lumine/internal/shm"
	"github.com/Team-Quaver/lumine/internal/wl"
	"golang.org/x/sys/unix"
)

// captureRenderer records the last frame the compositor produced.
type captureRenderer struct {
	mu    sync.Mutex
	items []compositor.DrawItem
	w, h  int
	seen  int
}

func (r *captureRenderer) Draw(w, h int, items []compositor.DrawItem) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.w, r.h = w, h
	r.items = append(r.items[:0], items...)
	r.seen++
	return nil
}

func (r *captureRenderer) SetColorSpace(enabled bool, tf, primaries string, maxLuma uint32) {}

func (r *captureRenderer) snapshot() ([]compositor.DrawItem, int) {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := append([]compositor.DrawItem(nil), r.items...)
	return out, r.seen
}

// testClient is a minimal pure-Go Wayland client: it binds the globals Lumine
// serves, maps an xdg_toplevel with a wl_shm buffer and records every event it
// receives.
type testClient struct {
	t   *testing.T
	con *wl.Conn

	names    map[string]uint32
	versions map[string]uint32

	reg  *wl.WlRegistry
	comp *wl.WlCompositor
	shm  *wl.WlShm
	xdg  *wl.XdgWmBase
	seat *wl.WlSeat

	surf     *wl.WlSurface
	xdgSurf  *wl.XdgSurface
	toplevel *wl.XdgToplevel
	kb       *wl.WlKeyboard
	buf      *wl.WlBuffer

	mu            sync.Mutex
	configured    bool
	cfgW, cfgH    int32
	toplevelCfg   bool
	capsSeen      bool
	caps          []uint32
	tlStates      []uint32
	seq           int
	capsSeq       int
	configureSeq  int
	released      bool
	frameDone     bool
	keymapSeen    bool
	keymapText    string
	keyboardEnter bool
	keyboardKeys  []keyEvent
	windowTitle   string
	closeCount    int
	descReady     bool
	descFailed    bool
}

func (c *testClient) Global(o *wl.WlRegistry, name uint32, iface string, version uint32) {
	c.names[iface] = name
	c.versions[iface] = version
}

func (c *testClient) GlobalRemove(o *wl.WlRegistry, name uint32) {}

// surfaceEvents implements wl.XdgSurfaceEvents. The generated event
// interfaces name their methods after the wire events, so xdg_surface and
// xdg_toplevel both expose a "Configure"; a single Go type cannot implement
// both. Real clients (and the nested backend) therefore use one handler type
// per interface, and so does this harness.
type surfaceEvents struct{ c *testClient }

func (h *surfaceEvents) Configure(o *wl.XdgSurface, serial uint32) {
	_ = o.ReqAckConfigure(serial)
	h.c.mu.Lock()
	h.c.seq++
	if h.c.configureSeq == 0 {
		h.c.configureSeq = h.c.seq
	}
	h.c.configured = true
	h.c.mu.Unlock()
}

// toplevelEvents implements wl.XdgToplevelEvents.
type toplevelEvents struct{ c *testClient }

func (h *toplevelEvents) Configure(o *wl.XdgToplevel, width, height int32, states []byte) {
	h.c.mu.Lock()
	h.c.toplevelCfg = true
	h.c.cfgW, h.c.cfgH = width, height
	h.c.tlStates = decodeStates(states)
	h.c.mu.Unlock()
}

// decodeStates unpacks the xdg_toplevel.configure state array into enum values.
func decodeStates(raw []byte) []uint32 {
	out := make([]uint32, 0, len(raw)/4)
	for i := 0; i+4 <= len(raw); i += 4 {
		out = append(out, binary.NativeEndian.Uint32(raw[i:]))
	}
	return out
}

func containsState(states []uint32, want uint32) bool {
	for _, v := range states {
		if v == want {
			return true
		}
	}
	return false
}

func (h *toplevelEvents) Close(o *wl.XdgToplevel) {
	h.c.mu.Lock()
	h.c.closeCount++
	h.c.mu.Unlock()
}

func (h *toplevelEvents) ConfigureBounds(o *wl.XdgToplevel, width, height int32) {}

func (h *toplevelEvents) WmCapabilities(o *wl.XdgToplevel, capabilities []byte) {
	h.c.mu.Lock()
	h.c.seq++
	h.c.capsSeen = true
	h.c.capsSeq = h.c.seq
	h.c.caps = decodeStates(capabilities)
	h.c.mu.Unlock()
}

func (c *testClient) Release(o *wl.WlBuffer) {
	c.mu.Lock()
	c.released = true
	c.mu.Unlock()
}

func (c *testClient) Done(o *wl.WlCallback, data uint32) {
	o.Object.Destroy()
	c.mu.Lock()
	c.frameDone = true
	c.mu.Unlock()
}

func (c *testClient) Keymap(o *wl.WlKeyboard, format uint32, fd int, size uint32) {
	buf := make([]byte, size)
	n, err := unix.Pread(fd, buf, 0)
	unix.Close(fd)
	if err != nil {
		c.t.Errorf("read keymap fd: %v", err)
		return
	}
	c.mu.Lock()
	c.keymapSeen = format == 1
	c.keymapText = string(buf[:n])
	c.mu.Unlock()
}

func (c *testClient) Enter(o *wl.WlKeyboard, serial uint32, surface *wl.WlSurface, keys []byte) {
	c.mu.Lock()
	c.keyboardEnter = true
	c.mu.Unlock()
}

func (c *testClient) Leave(o *wl.WlKeyboard, serial uint32, surface *wl.WlSurface) {}

type keyEvent struct {
	key   uint32
	state uint32
}

func (c *testClient) Key(o *wl.WlKeyboard, serial, time uint32, key, state uint32) {
	c.mu.Lock()
	c.keyboardKeys = append(c.keyboardKeys, keyEvent{key: key, state: state})
	c.mu.Unlock()
}
func (c *testClient) Modifiers(o *wl.WlKeyboard, serial, deprecated, latched, locked, group uint32) {
}
func (c *testClient) RepeatInfo(o *wl.WlKeyboard, rate, delay int32) {}

// wait drives the client connection (single-threaded: no other goroutine
// touches the socket) until pred returns true or the deadline passes. It polls
// the socket so it never blocks forever on a compositor that is idle.
func (c *testClient) wait(pred func() bool, what string) {
	c.t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for {
		c.mu.Lock()
		ok := pred()
		c.mu.Unlock()
		if ok {
			return
		}
		if time.Now().After(deadline) {
			c.t.Fatalf("timed out waiting for %s", what)
			return
		}
		fds := []unix.PollFd{{Fd: int32(c.con.FD()), Events: unix.POLLIN}}
		n, err := unix.Poll(fds, 20)
		if err != nil {
			if err == unix.EINTR {
				continue
			}
			c.t.Fatalf("poll: %v", err)
			return
		}
		if n > 0 {
			if err := c.con.NextEvent(); err != nil && !c.con.Closed() {
				c.t.Logf("next event: %v", err)
			}
		}
	}
}

// commitShmFrame attaches and commits a bw x bh XRGB8888 wl_shm buffer on
// c.surf so the compositor has client pixels to draw for that surface.
func (c *testClient) commitShmFrame(bw, bh int) {
	c.t.Helper()
	stride := bw * 4
	fd, err := unix.MemfdCreate("lumine-frame", unix.MFD_CLOEXEC)
	if err != nil {
		c.t.Fatal(err)
	}
	defer unix.Close(fd)
	if err := unix.Ftruncate(fd, int64(stride*bh)); err != nil {
		c.t.Fatal(err)
	}
	pool := wl.NewWlShmPool(c.con, c.con.AllocID(), 1, nil)
	if err := c.shm.ReqCreatePool(&pool.Object, fd, int32(stride*bh)); err != nil {
		c.t.Fatal(err)
	}
	buf := wl.NewWlBuffer(c.con, c.con.AllocID(), 1, c)
	if err := pool.ReqCreateBuffer(&buf.Object, 0, int32(bw), int32(bh), int32(stride), uint32(shm.XRGB8888)); err != nil {
		c.t.Fatal(err)
	}
	if err := c.surf.ReqAttach(&buf.Object, 0, 0); err != nil {
		c.t.Fatal(err)
	}
	if err := c.surf.ReqDamageBuffer(0, 0, int32(bw), int32(bh)); err != nil {
		c.t.Fatal(err)
	}
	if err := c.surf.ReqCommit(); err != nil {
		c.t.Fatal(err)
	}
	if err := c.con.Flush(); err != nil {
		c.t.Fatal(err)
	}
	c.buf = buf
}

// startHarness boots a compositor with a capture renderer and a 60 Hz frame
// pump, returning its Wayland socket path. It is the shared setup for the
// full-session integration tests.
func startHarness(t *testing.T) (string, *captureRenderer, *compositor.Compositor) {
	t.Helper()
	dir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", dir)
	sock := filepath.Join(dir, "lumine-test")

	store, err := config.NewStore(filepath.Join(dir, "config.toml"))
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
	if err := comp.Listen(sock, "test"); err != nil {
		t.Fatal(err)
	}

	// Pump frames while the client works.
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

// dialClient connects to a harness socket, runs the initial round trips and
// fetches the registry, which is kept on the client so later binds stay valid.
func dialClient(t *testing.T, sock string) (*wl.Conn, *testClient) {
	t.Helper()
	con, err := wl.Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { con.Close() })
	c := &testClient{t: t, con: con, names: map[string]uint32{}, versions: map[string]uint32{}}
	if err := con.RoundTrip(); err != nil {
		t.Fatalf("initial round trip: %v", err)
	}
	reg, err := con.DisplayGetRegistry(c)
	if err != nil {
		t.Fatal(err)
	}
	if err := con.RoundTrip(); err != nil {
		t.Fatalf("registry round trip: %v", err)
	}
	c.reg = reg
	return con, c
}

// TestServesShmToplevel drives a full client session against the compositor:
// registry, surface, xdg_toplevel, shm buffer commit, buffer release, frame
// callback, keyboard keymap and SSD geometry.
func TestServesShmToplevel(t *testing.T) {
	sock, rd, comp := startHarness(t)
	con, c := dialClient(t, sock)
	reg := c.reg

	for _, want := range []string{"wl_compositor", "wl_shm", "wl_seat", "xdg_wm_base", "zxdg_decoration_manager_v1"} {
		if _, ok := c.names[want]; !ok {
			t.Fatalf("global %s not advertised", want)
		}
	}

	c.comp = wl.NewWlCompositor(con, con.AllocID(), 4, nil)
	if err := reg.ReqBind(c.names["wl_compositor"], "wl_compositor", 4, &c.comp.Object); err != nil {
		t.Fatal(err)
	}
	c.shm = wl.NewWlShm(con, con.AllocID(), 1, nil)
	if err := reg.ReqBind(c.names["wl_shm"], "wl_shm", 1, &c.shm.Object); err != nil {
		t.Fatal(err)
	}
	c.xdg = wl.NewXdgWmBase(con, con.AllocID(), 4, nil)
	if err := reg.ReqBind(c.names["xdg_wm_base"], "xdg_wm_base", 4, &c.xdg.Object); err != nil {
		t.Fatal(err)
	}
	c.seat = wl.NewWlSeat(con, con.AllocID(), 5, nil)
	if err := reg.ReqBind(c.names["wl_seat"], "wl_seat", 5, &c.seat.Object); err != nil {
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
	if err := c.toplevel.ReqSetTitle("hello lumine"); err != nil {
		t.Fatal(err)
	}
	if err := c.toplevel.ReqSetAppId("lumine-test"); err != nil {
		t.Fatal(err)
	}
	if err := c.surf.ReqCommit(); err != nil {
		t.Fatal(err)
	}
	if err := con.Flush(); err != nil {
		t.Fatal(err)
	}
	c.wait(func() bool { return c.configured }, "xdg_surface.configure")

	c.kb = wl.NewWlKeyboard(con, con.AllocID(), 5, c)
	if err := c.seat.ReqGetKeyboard(&c.kb.Object); err != nil {
		t.Fatal(err)
	}
	if err := con.Flush(); err != nil {
		t.Fatal(err)
	}
	c.wait(func() bool { return c.keymapSeen }, "wl_keyboard.keymap")
	if !strings.Contains(c.keymapText, "xkb_keycodes") || !strings.Contains(c.keymapText, "xkb_symbols") {
		t.Fatalf("keymap is not an XKB text keymap: %q", c.keymapText)
	}

	// 64x48 XRGB8888 buffer.
	const (
		bw, bh = 64, 48
		stride = bw * 4
	)
	fd, err := unix.MemfdCreate("lumine-test-buf", unix.MFD_CLOEXEC)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	if err := unix.Ftruncate(fd, int64(stride*bh)); err != nil {
		t.Fatal(err)
	}
	pool := wl.NewWlShmPool(con, con.AllocID(), 1, nil)
	if err := c.shm.ReqCreatePool(&pool.Object, fd, int32(stride*bh)); err != nil {
		t.Fatal(err)
	}
	c.buf = wl.NewWlBuffer(con, con.AllocID(), 1, c)
	if err := pool.ReqCreateBuffer(&c.buf.Object, 0, bw, bh, stride, uint32(shm.XRGB8888)); err != nil {
		t.Fatal(err)
	}
	if err := c.surf.ReqAttach(&c.buf.Object, 0, 0); err != nil {
		t.Fatal(err)
	}
	if err := c.surf.ReqDamageBuffer(0, 0, bw, bh); err != nil {
		t.Fatal(err)
	}
	cb := wl.NewWlCallback(con, con.AllocID(), 1, c)
	if err := c.surf.ReqFrame(&cb.Object); err != nil {
		t.Fatal(err)
	}
	if err := c.surf.ReqCommit(); err != nil {
		t.Fatal(err)
	}
	if err := con.Flush(); err != nil {
		t.Fatal(err)
	}

	c.wait(func() bool { return c.released }, "wl_buffer.release")
	c.wait(func() bool { return c.frameDone }, "wl_callback.done")

	// Keyboard events are forwarded with raw evdev keycodes. In particular,
	// do not apply the XKB +8 offset here: clients add it themselves when they
	// resolve the advertised keymap.
	c.wait(func() bool { return c.keyboardEnter }, "wl_keyboard.enter")
	comp.Key(30, true) // KEY_A
	comp.Key(30, false)
	c.wait(func() bool { return len(c.keyboardKeys) >= 2 }, "wl_keyboard.key")
	c.mu.Lock()
	keys := append([]keyEvent(nil), c.keyboardKeys...)
	c.mu.Unlock()
	if len(keys) != 2 {
		t.Fatalf("got %d key events, want 2: %+v", len(keys), keys)
	}
	if keys[0] != (keyEvent{key: 30, state: wl.WlKeyboardKeyStatePressed}) ||
		keys[1] != (keyEvent{key: 30, state: wl.WlKeyboardKeyStateReleased}) {
		t.Fatalf("forwarded keys = %+v, want raw KEY_A press/release", keys)
	}

	// The compositor must have drawn the client's pixels into the tile.
	items, frames := rd.snapshot()
	if frames == 0 {
		t.Fatal("renderer never ran")
	}
	var found bool
	for _, it := range items {
		if it.Image == nil {
			continue
		}
		if it.Image.Width != bw || it.Image.Height != bh {
			t.Fatalf("draw item image is %dx%d, want %dx%d", it.Image.Width, it.Image.Height, bw, bh)
		}
		if it.Title != "hello lumine" || it.AppID != "lumine-test" {
			t.Fatalf("draw item title/app_id = %q/%q", it.Title, it.AppID)
		}
		if !it.Frame {
			t.Fatal("window should use server-side decorations (SSD)")
		}
		if it.Y < 28 {
			t.Fatalf("decorated window content starts at y=%v, want the title bar reserved", it.Y)
		}
		found = true
	}
	if !found {
		t.Fatal("renderer never received the committed client buffer")
	}

	// luminectl-style control commands operate on the live tree.
	comp.CloseFocused()
	c.wait(func() bool { return c.closeCount > 0 }, "xdg_toplevel.close")
}

// TestShmBufferOutlivesPool exercises the Wayland guarantee that a wl_buffer
// stays valid after the wl_shm_pool it was created from is destroyed. Clients
// such as kitty create their buffers and immediately destroy the pool; a
// compositor that munmaps (or frees) the pool on destroy would then crash or
// read freed memory on the next commit. This is a regression test for a real
// SIGSEGV hit by kitty against Lumine.
func TestShmBufferOutlivesPool(t *testing.T) {
	sock, rd, _ := startHarness(t)
	con, c := dialClient(t, sock)
	reg := c.reg

	c.comp = wl.NewWlCompositor(con, con.AllocID(), 4, nil)
	if err := reg.ReqBind(c.names["wl_compositor"], "wl_compositor", 4, &c.comp.Object); err != nil {
		t.Fatal(err)
	}
	c.shm = wl.NewWlShm(con, con.AllocID(), 1, nil)
	if err := reg.ReqBind(c.names["wl_shm"], "wl_shm", 1, &c.shm.Object); err != nil {
		t.Fatal(err)
	}
	c.xdg = wl.NewXdgWmBase(con, con.AllocID(), 4, nil)
	if err := reg.ReqBind(c.names["xdg_wm_base"], "xdg_wm_base", 4, &c.xdg.Object); err != nil {
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
	if err := c.toplevel.ReqSetTitle("pool-life"); err != nil {
		t.Fatal(err)
	}
	if err := c.toplevel.ReqSetAppId("lumine-pool"); err != nil {
		t.Fatal(err)
	}
	if err := c.surf.ReqCommit(); err != nil {
		t.Fatal(err)
	}
	if err := con.Flush(); err != nil {
		t.Fatal(err)
	}
	c.wait(func() bool { return c.configured }, "xdg_surface.configure")

	// 32x32 XRGB8888 buffer with a known pattern in every byte.
	const (
		bw, bh = 32, 32
		stride = bw * 4
	)
	fd, err := unix.MemfdCreate("lumine-pool-buf", unix.MFD_CLOEXEC)
	if err != nil {
		t.Fatal(err)
	}
	defer unix.Close(fd)
	if err := unix.Ftruncate(fd, int64(stride*bh)); err != nil {
		t.Fatal(err)
	}
	pix := make([]byte, stride*bh)
	for i := range pix {
		pix[i] = 0xAB
	}
	if _, err := unix.Pwrite(fd, pix, 0); err != nil {
		t.Fatal(err)
	}

	pool := wl.NewWlShmPool(con, con.AllocID(), 1, nil)
	if err := c.shm.ReqCreatePool(&pool.Object, fd, int32(stride*bh)); err != nil {
		t.Fatal(err)
	}
	c.buf = wl.NewWlBuffer(con, con.AllocID(), 1, c)
	if err := pool.ReqCreateBuffer(&c.buf.Object, 0, bw, bh, stride, uint32(shm.XRGB8888)); err != nil {
		t.Fatal(err)
	}
	// Destroy the pool *before* the buffer is ever committed. The wl_buffer,
	// and the mapping it points at, must stay usable.
	if err := pool.ReqDestroy(); err != nil {
		t.Fatal(err)
	}
	if err := c.surf.ReqAttach(&c.buf.Object, 0, 0); err != nil {
		t.Fatal(err)
	}
	if err := c.surf.ReqDamageBuffer(0, 0, bw, bh); err != nil {
		t.Fatal(err)
	}
	cb := wl.NewWlCallback(con, con.AllocID(), 1, c)
	if err := c.surf.ReqFrame(&cb.Object); err != nil {
		t.Fatal(err)
	}
	if err := c.surf.ReqCommit(); err != nil {
		t.Fatal(err)
	}
	if err := con.Flush(); err != nil {
		t.Fatal(err)
	}

	c.wait(func() bool { return c.released }, "wl_buffer.release")
	c.wait(func() bool { return c.frameDone }, "wl_callback.done")

	items, _ := rd.snapshot()
	var found bool
	for _, it := range items {
		if it.Image == nil || it.Title != "pool-life" {
			continue
		}
		if it.Image.Width != bw || it.Image.Height != bh {
			t.Fatalf("draw item image is %dx%d, want %dx%d", it.Image.Width, it.Image.Height, bw, bh)
		}
		if len(it.Image.Pix) < stride*bh {
			t.Fatalf("image pix is %d bytes, want at least %d", len(it.Image.Pix), stride*bh)
		}
		// Every byte must still match the pattern we wrote, proving the
		// compositor read the mapping after the pool was destroyed rather than
		// reading unmapped/freed memory.
		for i, b := range it.Image.Pix[:stride*bh] {
			if b != 0xAB {
				t.Fatalf("pixel byte %d = %#x, want 0xAB (pool mapping lost?)", i, b)
			}
		}
		found = true
	}
	if !found {
		t.Fatal("renderer never received the buffer committed after pool destroy")
	}
}

// TestToplevelWmCapabilitiesAndMaximize checks the xdg-shell contract GLFW
// (and therefore kitty) depends on: the compositor must send wm_capabilities
// advertising maximize *before* the first xdg_surface.configure, and a
// set_maximized request must produce a configure with the maximized state at
// the output size. Missing this made GLFW fail with "compositor does not
// support maximizing windows".
func TestToplevelWmCapabilitiesAndMaximize(t *testing.T) {
	sock, rd, _ := startHarness(t)
	con, c := dialClient(t, sock)
	reg := c.reg

	c.comp = wl.NewWlCompositor(con, con.AllocID(), 4, nil)
	if err := reg.ReqBind(c.names["wl_compositor"], "wl_compositor", 4, &c.comp.Object); err != nil {
		t.Fatal(err)
	}
	c.shm = wl.NewWlShm(con, con.AllocID(), 1, nil)
	if err := reg.ReqBind(c.names["wl_shm"], "wl_shm", 1, &c.shm.Object); err != nil {
		t.Fatal(err)
	}
	c.xdg = wl.NewXdgWmBase(con, con.AllocID(), 5, nil)
	if err := reg.ReqBind(c.names["xdg_wm_base"], "xdg_wm_base", 5, &c.xdg.Object); err != nil {
		t.Fatal(err)
	}
	c.surf = wl.NewWlSurface(con, con.AllocID(), 4, nil)
	if err := c.comp.ReqCreateSurface(&c.surf.Object); err != nil {
		t.Fatal(err)
	}
	c.xdgSurf = wl.NewXdgSurface(con, con.AllocID(), 5, &surfaceEvents{c})
	if err := c.xdg.ReqGetXdgSurface(&c.xdgSurf.Object, &c.surf.Object); err != nil {
		t.Fatal(err)
	}
	c.toplevel = wl.NewXdgToplevel(con, con.AllocID(), 5, &toplevelEvents{c})
	if err := c.xdgSurf.ReqGetToplevel(&c.toplevel.Object); err != nil {
		t.Fatal(err)
	}
	if err := c.toplevel.ReqSetAppId("lumine-caps"); err != nil {
		t.Fatal(err)
	}
	c.commitShmFrame(64, 48)
	if err := c.surf.ReqCommit(); err != nil {
		t.Fatal(err)
	}
	if err := con.Flush(); err != nil {
		t.Fatal(err)
	}
	c.wait(func() bool { return c.capsSeen && c.configured }, "wm_capabilities + configure")

	c.mu.Lock()
	caps, capsSeq, cfgSeq := append([]uint32(nil), c.caps...), c.capsSeq, c.configureSeq
	c.mu.Unlock()

	if !containsState(caps, wl.XdgToplevelWmCapabilitiesMaximize) {
		t.Fatalf("wm_capabilities %v does not advertise maximize", caps)
	}
	if !containsState(caps, wl.XdgToplevelWmCapabilitiesFullscreen) {
		t.Fatalf("wm_capabilities %v does not advertise fullscreen", caps)
	}
	if capsSeq == 0 || cfgSeq == 0 || capsSeq > cfgSeq {
		t.Fatalf("wm_capabilities must precede the first configure (capsSeq=%d configureSeq=%d)", capsSeq, cfgSeq)
	}

	// Maximize: expect a configure with the maximized state at the output size.
	if err := c.toplevel.ReqSetMaximized(); err != nil {
		t.Fatal(err)
	}
	if err := con.Flush(); err != nil {
		t.Fatal(err)
	}
	c.wait(func() bool {
		return containsState(c.tlStates, wl.XdgToplevelStateMaximized)
	}, "xdg_toplevel.configure with maximized state")

	c.mu.Lock()
	states, w, h := append([]uint32(nil), c.tlStates...), c.cfgW, c.cfgH
	c.mu.Unlock()
	if !containsState(states, wl.XdgToplevelStateMaximized) {
		t.Fatalf("configure states %v missing maximized", states)
	}
	if w != 800 || h != 600 {
		t.Fatalf("maximized configure is %dx%d, want the 800x600 output", w, h)
	}

	// The renderer must draw the window covering the whole output.
	c.wait(func() bool {
		items, _ := rd.snapshot()
		for _, it := range items {
			if it.AppID == "lumine-caps" && it.W == 800 && it.H == 600 && it.X == 0 && it.Y == 0 {
				return true
			}
		}
		return false
	}, "maximized window drawn at output size")

	// Unmaximize returns the window to its tiling rect.
	if err := c.toplevel.ReqUnsetMaximized(); err != nil {
		t.Fatal(err)
	}
	if err := con.Flush(); err != nil {
		t.Fatal(err)
	}
	c.wait(func() bool {
		return !containsState(c.tlStates, wl.XdgToplevelStateMaximized)
	}, "xdg_toplevel.configure without maximized state")
}

// descEvents implements wl.WpImageDescriptionV1Events, recording whether the
// compositor materialised a parametric image description.
type descEvents struct{ c *testClient }

func (h *descEvents) Failed(o *wl.WpImageDescriptionV1, cause uint32, msg string) {
	h.c.mu.Lock()
	h.c.descFailed = true
	h.c.mu.Unlock()
}
func (h *descEvents) Ready(o *wl.WpImageDescriptionV1, identity uint32) {
	h.c.mu.Lock()
	h.c.descReady = true
	h.c.mu.Unlock()
}
func (h *descEvents) Ready2(o *wl.WpImageDescriptionV1, hi, lo uint32) {
	h.c.mu.Lock()
	h.c.descReady = true
	h.c.mu.Unlock()
}

// TestColorManagerParametric drives the wp_color_manager_v1 path a colour-aware
// client uses: bind the manager, build a parametric (BT.2020 + PQ) image
// description and wait for it to be marked ready instead of hanging.
func TestColorManagerParametric(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", dir)
	sock := filepath.Join(dir, "lumine-color")

	store, err := config.NewStore(filepath.Join(dir, "config.toml"))
	if err != nil {
		t.Fatal(err)
	}
	srv, err := wl.NewServer(sock)
	if err != nil {
		t.Fatal(err)
	}
	comp := compositor.New(srv, store)
	comp.RegisterGlobals()
	comp.AddOutput(&compositor.Output{Name: "test-0", W: 800, H: 600, Refresh: 60_000, Renderer: &captureRenderer{}})
	if err := comp.Listen(sock, "test-color"); err != nil {
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
	defer func() { close(stop); wg.Wait() }()

	con, err := wl.Dial(sock)
	if err != nil {
		t.Fatal(err)
	}
	defer con.Close()
	c := &testClient{t: t, con: con, names: map[string]uint32{}, versions: map[string]uint32{}}
	if err := con.RoundTrip(); err != nil {
		t.Fatal(err)
	}
	reg, err := con.DisplayGetRegistry(c)
	if err != nil {
		t.Fatal(err)
	}
	if err := con.RoundTrip(); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.names["wp_color_manager_v1"]; !ok {
		t.Fatal("wp_color_manager_v1 not advertised")
	}

	cm := wl.NewWpColorManagerV1(con, con.AllocID(), 3, nil)
	if err := reg.ReqBind(c.names["wp_color_manager_v1"], "wp_color_manager_v1", 3, &cm.Object); err != nil {
		t.Fatal(err)
	}
	creator := wl.NewWpImageDescriptionCreatorParamsV1(con, con.AllocID(), 3, nil)
	if err := cm.ReqCreateParametricCreator(&creator.Object); err != nil {
		t.Fatal(err)
	}
	if err := creator.ReqSetTfNamed(wl.WpColorManagerV1TransferFunctionSt2084Pq); err != nil {
		t.Fatal(err)
	}
	if err := creator.ReqSetPrimariesNamed(wl.WpColorManagerV1PrimariesBt2020); err != nil {
		t.Fatal(err)
	}
	if err := creator.ReqSetLuminances(0, 1000, 100); err != nil {
		t.Fatal(err)
	}
	desc := wl.NewWpImageDescriptionV1(con, con.AllocID(), 3, &descEvents{c})
	if err := creator.ReqCreate(&desc.Object); err != nil {
		t.Fatal(err)
	}
	if err := con.Flush(); err != nil {
		t.Fatal(err)
	}
	c.wait(func() bool { return c.descReady || c.descFailed }, "wp_image_description ready")
	if c.descFailed {
		t.Fatal("compositor failed a supported parametric description")
	}
}
