// Package compositor implements Lumine's compositor state: the Wayland
// globals, surface state machines, seat, layout glue, and the control
// socket. Rendering and backend details live behind small interfaces.
package compositor

import (
	"encoding/binary"
	"os"
	"sort"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"

	"github.com/Team-Quaver/lumine/internal/config"
	"github.com/Team-Quaver/lumine/internal/ipc"
	"github.com/Team-Quaver/lumine/internal/layout"
	"github.com/Team-Quaver/lumine/internal/shm"
	"github.com/Team-Quaver/lumine/internal/wl"
)

// DrawItem is one textured quad for the renderer.
type DrawItem struct {
	Image     *shm.Image
	X, Y      float64
	W, H      float64
	Activated bool

	// Frame asks for server-side decorations around the quad. Clients that
	// negotiate client-side decorations leave it false, which is how CSD and
	// SSD windows coexist in the same output.
	Frame bool
	Title string
	AppID string
}

// Renderer draws one frame into the output's present target.
type Renderer interface {
	Draw(w, h int, items []DrawItem) error
	SetColorSpace(enabled bool, tf string, primaries string, maxLuma uint32)
}

// Output is one present target (a nested Wayland window or a headless
// surface today).
type Output struct {
	Name     string
	WidthMM  int32
	HeightMM int32
	W        int
	H        int
	Refresh  int32 // mHz
	Renderer Renderer
	Global   *wl.Global
	Make     string
	Model    string
}

// Compositor owns all shared state.
type Compositor struct {
	mu    sync.Mutex
	srv   *wl.Server
	ipc   *ipc.Server
	Store *config.Store

	Outputs []*Output

	surfaces map[*wl.WlSurface]*Surface
	trees    map[*Output]*layout.Tree
	conns    map[*wl.Conn]struct{}

	pointersByConn  map[*wl.Conn][]*wl.WlPointer
	keyboardsByConn map[*wl.Conn][]*wl.WlKeyboard
	keyboardFocusID atomic.Uint64

	// pointer state
	px, py       float64
	pointSurface *Surface

	// modifier state from the backend
	mods wl.Mods

	// keyboard focus: the surface that currently receives key events, plus the
	// keys held down on it so a focus change can send an accurate Enter list.
	kbFocus    *Surface
	kbSerial   uint32
	kbHeldKeys []uint32

	terminalCmd []string
	Spawn       func(argv []string)
	serial      atomic.Uint32
}

func New(srv *wl.Server, store *config.Store) *Compositor {
	c := &Compositor{
		srv:             srv,
		Store:           store,
		surfaces:        map[*wl.WlSurface]*Surface{},
		trees:           map[*Output]*layout.Tree{},
		conns:           map[*wl.Conn]struct{}{},
		pointersByConn:  map[*wl.Conn][]*wl.WlPointer{},
		keyboardsByConn: map[*wl.Conn][]*wl.WlKeyboard{},
	}
	srv.NewConn = c.newConn
	return c
}

func (c *Compositor) newConn(conn *wl.Conn) {
	c.mu.Lock()
	c.conns[conn] = struct{}{}
	c.mu.Unlock()
}

// flushAll pushes queued events to every connected client.
func (c *Compositor) flushAll() {
	c.mu.Lock()
	conns := make([]*wl.Conn, 0, len(c.conns))
	for conn := range c.conns {
		conns = append(conns, conn)
	}
	c.mu.Unlock()
	for _, conn := range conns {
		_ = conn.Flush()
	}
}

// AddOutput advertises an output and creates its layout tree.
func (c *Compositor) AddOutput(o *Output) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Outputs = append(c.Outputs, o)
	c.trees[o] = layout.New(c.Store.Config().Layout.Gap, c.Store.Config().Layout.ColumnRatio)
	c.trees[o].ViewW = float64(o.W)
	c.trees[o].ViewH = float64(o.H)
	c.trees[o].DecorH = ssdTitleHeight
	c.trees[o].Mode = layout.Mode(c.Store.Config().Layout.Mode)
	name, make_, model := o.Name, o.Make, o.Model
	if make_ == "" {
		make_ = "Lumine"
	}
	if model == "" {
		model = "Virtual"
	}
	o.Global = c.srv.AddGlobal("wl_output", 4, func(conn *wl.Conn, id, version uint32) {
		out := wl.NewWlOutput(conn, id, version, nil)
		out.Geometry(0, 0, o.WidthMM, o.HeightMM, 0, make_, model, 0)
		out.Mode(0, int32(o.W), int32(o.H), o.Refresh)
		out.Scale(1)
		out.Name(name)
		out.Description("Lumine output")
		out.Done()
	})
}

// RegisterGlobals advertises the protocols Lumine serves.
func (c *Compositor) RegisterGlobals() {
	c.srv.AddGlobal("wl_compositor", 6, func(conn *wl.Conn, id, version uint32) {
		wl.NewWlCompositor(conn, id, version, &compositorHandler{c})
	})
	c.srv.AddGlobal("wl_shm", 1, func(conn *wl.Conn, id, version uint32) {
		s := wl.NewWlShm(conn, id, version, &shmHandler{c})
		// Advertise the formats the renderer understands. Clients pick one of
		// these when they create buffers; anything else is rejected.
		s.Format(uint32(shm.ARGB8888))
		s.Format(uint32(shm.XRGB8888))
	})
	c.srv.AddGlobal("wl_seat", 8, func(conn *wl.Conn, id, version uint32) {
		seat := wl.NewWlSeat(conn, id, version, &seatHandler{c})
		seat.Capabilities(3) // pointer | keyboard
		seat.Name("lumine-seat")
	})
	c.srv.AddGlobal("xdg_wm_base", 7, func(conn *wl.Conn, id, version uint32) {
		wl.NewXdgWmBase(conn, id, version, &xdgWmBaseHandler{c})
	})
	c.srv.AddGlobal("zxdg_decoration_manager_v1", 1, func(conn *wl.Conn, id, version uint32) {
		wl.NewZxdgDecorationManagerV1(conn, id, version, &decorationHandler{c})
	})
	c.srv.AddGlobal("wp_viewporter", 1, func(conn *wl.Conn, id, version uint32) {
		wl.NewWpViewporter(conn, id, version, &viewporterHandler{c})
	})
	if !c.Store.Config().Color.Enabled {
		return
	}
	c.srv.AddGlobal("wp_color_manager_v1", 3, func(conn *wl.Conn, id, version uint32) {
		cm := wl.NewWpColorManagerV1(conn, id, version, &colorManagerHandler{c})
		for _, intent := range []uint32{
			wl.WpColorManagerV1RenderIntentPerceptual,
			wl.WpColorManagerV1RenderIntentRelative,
			wl.WpColorManagerV1RenderIntentAbsolute,
		} {
			cm.SupportedIntent(intent)
		}
		for _, feature := range []uint32{
			wl.WpColorManagerV1FeatureParametric,
			wl.WpColorManagerV1FeatureSetLuminances,
			wl.WpColorManagerV1FeatureWindowsScrgb,
			wl.WpColorManagerV1FeatureWindowsBt2100,
		} {
			cm.SupportedFeature(feature)
		}
		for _, tf := range []uint32{
			wl.WpColorManagerV1TransferFunctionSrgb,
			wl.WpColorManagerV1TransferFunctionExtSrgb,
			wl.WpColorManagerV1TransferFunctionSt2084Pq,
			wl.WpColorManagerV1TransferFunctionHlg,
			wl.WpColorManagerV1TransferFunctionExtLinear,
		} {
			cm.SupportedTfNamed(tf)
		}
		for _, p := range []uint32{
			wl.WpColorManagerV1PrimariesSrgb,
			wl.WpColorManagerV1PrimariesBt2020,
			wl.WpColorManagerV1PrimariesDciP3,
		} {
			cm.SupportedPrimariesNamed(p)
		}
		cm.Done()
	})
}

// Listen starts accepting Wayland clients and control connections.
func (c *Compositor) Listen(socketPath, ctlName string) error {
	go c.srv.Accept()
	runtime := os.Getenv("XDG_RUNTIME_DIR")
	if runtime == "" {
		runtime = "/run/user/" + strconv.Itoa(os.Getuid())
	}
	ctl, err := ipc.Listen(runtime, ctlName)
	if err != nil {
		return err
	}
	c.ipc = ctl
	ctl.Handler = c.handleCommand
	go ctl.Serve()
	return nil
}

// ---- frame pump ----

// Frame renders one frame on every output, pushes geometry configures and
// releases frame callbacks.
func (c *Compositor) Frame() {
	type job struct {
		o     *Output
		items []DrawItem
	}
	c.mu.Lock()
	jobs := make([]job, 0, len(c.Outputs))
	for _, o := range c.Outputs {
		tree := c.trees[o]
		tree.ViewW = float64(o.W)
		tree.ViewH = float64(o.H)
		for _, w := range tree.Windows() {
			if s := c.surfaceForWinLocked(w.ID); s != nil {
				w.Decorated = s.serverDecorated()
			}
		}
		focused := tree.Arrange()
		var items []DrawItem
		for _, w := range tree.Windows() {
			s := c.surfaceForWinLocked(w.ID)
			if s == nil || s.image == nil {
				continue
			}
			r := effectiveRect(o, w, s)
			items = append(items, DrawItem{
				Image: s.image, X: r.X, Y: r.Y,
				W: r.W, H: r.H, Activated: w.ID == focused,
				Frame: s.serverDecorated(), Title: s.windowTitle(), AppID: s.appID,
			})
		}
		jobs = append(jobs, job{o: o, items: items})
	}
	c.mu.Unlock()

	// Render outside the state lock: GPU submissions must not stall protocol
	// handling. Every renderer copies into its own framebuffer, so the
	// snapshots taken above stay valid.
	for _, j := range jobs {
		if j.o.Renderer != nil {
			_ = j.o.Renderer.Draw(j.o.W, j.o.H, j.items)
		}
	}

	c.mu.Lock()
	for _, o := range c.Outputs {
		focused := c.trees[o].FocusedID()
		c.releaseBuffersLocked()
		c.releaseFrameCallbacksLocked()
		c.applyConfiguresLocked(o, focused)
		c.keyboardFocusID.Store(focused)
		c.setKeyboardFocusLocked(c.surfaceForWinLocked(focused))
	}
	c.mu.Unlock()
	c.flushAll()
}

func (c *Compositor) surfaceForWinLocked(id uint64) *Surface {
	for _, s := range c.surfaces {
		if s.winID == id {
			return s
		}
	}
	return nil
}

// bufferGone forgets a wl_buffer the client destroyed. Any surface still
// holding it must not send wl_buffer.release to the deleted object.
func (c *Compositor) bufferGone(o *wl.WlBuffer) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, s := range c.surfaces {
		if s.buf == o {
			s.buf = nil
			s.bufPending = false
		}
	}
}

// releaseBuffersLocked returns committed shm buffers to their clients. The
// compositor owns private copies of every frame it draws, so a buffer is safe
// to hand back as soon as the frame has been rendered.
func (c *Compositor) releaseBuffersLocked() {
	for _, s := range c.surfaces {
		if s.bufPending && s.buf != nil {
			s.buf.Release()
			s.bufPending = false
		}
		for _, cb := range s.releaseCallbacks {
			cb.Done(c.nextSerial())
			cb.Object.Destroy()
		}
		s.releaseCallbacks = nil
	}
}

func (c *Compositor) releaseFrameCallbacksLocked() {
	for _, s := range c.surfaces {
		for len(s.pendingFrames) > 0 {
			cb := s.pendingFrames[0]
			s.pendingFrames = s.pendingFrames[1:]
			cb.Done(c.nextSerial())
			cb.Object.Destroy()
		}
	}
}

func (c *Compositor) applyConfiguresLocked(o *Output, focused uint64) {
	tree := c.trees[o]
	for _, w := range tree.Windows() {
		s := c.surfaceForWinLocked(w.ID)
		if s == nil || s.toplevel == nil {
			continue
		}
		r := effectiveRect(o, w, s)
		w0, h0 := int32(r.W), int32(r.H)
		foc := w.ID == focused
		// Skip identical configures: the frame pump runs at 60 Hz and a
		// configure per frame would be a storm. The client is already at this
		// size/state (it acked the configure that produced it), so re-sending
		// buys nothing.
		if s.cfgSent && s.cfgW == w0 && s.cfgH == h0 && s.cfgFocused == foc &&
			s.cfgMax == s.maximized && s.cfgFull == s.fullscreen {
			continue
		}
		s.cfgSent, s.cfgW, s.cfgH, s.cfgFocused = true, w0, h0, foc
		s.cfgMax, s.cfgFull = s.maximized, s.fullscreen
		s.toplevel.Configure(w0, h0, s.toplevelState(foc))
		s.xdg.Configure(c.nextSerial())
	}
}

func (c *Compositor) nextSerial() uint32 { return c.serial.Add(1) }

// ---- input entry points (called by the backend) ----

// Mods is the modifier state, shared with input backends.
type Mods = wl.Mods

// SetMods updates the modifier state from the input backend and forwards it to
// the focused client, which needs modifier state to resolve keysyms.
func (c *Compositor) SetMods(m Mods) {
	c.mu.Lock()
	c.mods = m
	c.sendModifiersLocked()
	c.mu.Unlock()
}

// Key handles one evdev keycode press/release. Returns true when consumed
// by a compositor binding; unconsumed keys are forwarded to the focused
// client so applications receive input.
func (c *Compositor) Key(code uint32, pressed bool) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if pressed && c.mods.Super {
		if binding := c.bindingForLocked(code); binding != "" {
			return c.runActionLocked(binding)
		}
	}
	// wl_keyboard.key events carry raw evdev scancodes. The advertised XKB
	// keymap lets clients map them; do not apply the XKB +8 offset here.
	evdev := code
	if pressed {
		for _, k := range c.kbHeldKeys {
			if k == evdev {
				return false
			}
		}
		c.kbHeldKeys = append(c.kbHeldKeys, evdev)
	} else {
		for i, k := range c.kbHeldKeys {
			if k == evdev {
				c.kbHeldKeys = append(c.kbHeldKeys[:i], c.kbHeldKeys[i+1:]...)
				break
			}
		}
	}
	c.forwardKeyLocked(evdev, pressed)
	return false
}

var keyCodeNames = map[uint32]string{
	105: "left", 106: "right", 103: "up", 108: "down",
	30: "a", 48: "b", 46: "c", 32: "d", 18: "e", 33: "f", 34: "g",
	35: "h", 23: "i", 36: "j", 37: "k", 38: "l", 50: "m", 49: "n",
	24: "o", 25: "p", 16: "q", 19: "r", 31: "s", 20: "t", 22: "u",
	47: "v", 17: "w", 45: "x", 21: "y", 44: "z", 28: "return",
}

func (c *Compositor) bindingForLocked(code uint32) string {
	cfg := c.Store.Config()
	dir, ok := keyCodeNames[code]
	if !ok {
		return ""
	}
	prefix := "super+" + dir
	if c.mods.Shift {
		prefix = "super+shift+" + dir
	}
	return cfg.Bindings[prefix]
}

func (c *Compositor) runActionLocked(action string) bool {
	switch action {
	case "focus-left", "focus-right", "focus-up", "focus-down":
		tree := c.primaryTreeLocked()
		if tree != nil {
			tree.Focus(strings.TrimPrefix(action, "focus-"))
		}
	case "move-left", "move-right":
		if tree := c.primaryTreeLocked(); tree != nil {
			tree.Move(strings.TrimPrefix(action, "move-"))
		}
	case "scroll-left":
		if tree := c.primaryTreeLocked(); tree != nil {
			tree.ScrollBy(-tree.ViewW * 0.3)
		}
	case "scroll-right":
		if tree := c.primaryTreeLocked(); tree != nil {
			tree.ScrollBy(tree.ViewW * 0.3)
		}
	case "mode-toggle":
		for _, tree := range c.trees {
			if tree.Mode == layout.ModeTile {
				tree.SetMode(layout.ModeStack)
			} else {
				tree.SetMode(layout.ModeTile)
			}
		}
	case "close":
		c.closeFocusedLocked()
	case "spawn-terminal":
		if c.Spawn != nil && len(c.terminalCmd) > 0 {
			c.Spawn(c.terminalCmd)
		}
	default:
		return false
	}
	return true
}

func (c *Compositor) primaryTreeLocked() *layout.Tree {
	if len(c.Outputs) == 0 {
		return nil
	}
	return c.trees[c.Outputs[0]]
}

// CloseFocused asks the focused xdg_toplevel to close.
func (c *Compositor) CloseFocused() {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.closeFocusedLocked()
}

func (c *Compositor) closeFocusedLocked() {
	for _, tree := range c.trees {
		fid := tree.FocusedID()
		if fid == 0 {
			continue
		}
		for _, s := range c.surfaces {
			if s.winID == fid && s.toplevel != nil {
				s.toplevel.Close()
				return
			}
		}
	}
}

// Motion updates the pointer position by a relative delta and routes pointer
// events (including enter/leave) to the surface under the cursor.
func (c *Compositor) Motion(dx, dy float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.movePointerLocked(c.px+dx, c.py+dy)
}

// MotionAbs moves the pointer to an absolute output-local position. Nested
// backends report absolute coordinates from the host compositor.
func (c *Compositor) MotionAbs(x, y float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.movePointerLocked(x, y)
}

func (c *Compositor) movePointerLocked(x, y float64) {
	o := c.primaryOutputLocked()
	if o == nil {
		return
	}
	c.px = clamp(x, 0, float64(o.W-1))
	c.py = clamp(y, 0, float64(o.H-1))
	s := c.surfaceAtLocked(c.px, c.py)
	if s != c.pointSurface {
		if c.pointSurface != nil {
			for _, p := range c.pointersByConn[c.pointSurface.conn] {
				p.Leave(c.nextSerial(), c.pointSurface.surface)
				p.Frame()
			}
		}
		c.pointSurface = s
		if s != nil {
			for _, p := range c.pointersByConn[s.conn] {
				p.Enter(c.nextSerial(), s.surface, wl.Fixed(c.px-s.rect.X), wl.Fixed(c.py-s.rect.Y))
				p.Frame()
			}
		}
	}
	if s == nil {
		return
	}
	for _, p := range c.pointersByConn[s.conn] {
		p.Motion(0, wl.Fixed(c.px-s.rect.X), wl.Fixed(c.py-s.rect.Y))
	}
}

// Axis forwards scroll deltas to the surface under the cursor.
func (c *Compositor) Axis(dx, dy float64) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.pointSurface
	if s == nil {
		s = c.surfaceAtLocked(c.px, c.py)
	}
	if s == nil {
		return
	}
	for _, p := range c.pointersByConn[s.conn] {
		if dx != 0 {
			p.AxisSource(wl.WlPointerAxisSourceWheel)
			p.Axis(0, wl.WlPointerAxisHorizontalScroll, wl.Fixed(dx))
		}
		if dy != 0 {
			p.AxisSource(wl.WlPointerAxisSourceWheel)
			p.Axis(0, wl.WlPointerAxisVerticalScroll, wl.Fixed(dy))
		}
		p.Frame()
	}
}

// Button forwards pointer buttons to the surface under the cursor; in
// stack mode, clicking a window raises and focuses it.
func (c *Compositor) Button(btn uint32, pressed bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	s := c.pointSurface
	if s == nil {
		s = c.surfaceAtLocked(c.px, c.py)
		c.pointSurface = s
	}
	if s == nil {
		return
	}
	if pressed {
		for _, tree := range c.trees {
			if tree.Mode == layout.ModeStack && s.winID != 0 {
				tree.SetFocus(s.winID)
			}
		}
		c.setKeyboardFocusLocked(s)
	}
	for _, p := range c.pointersByConn[s.conn] {
		p.Button(c.nextSerial(), 0, btn, pressedUint(pressed))
		p.Frame()
	}
}

// ---- keyboard focus ----

// setKeyboardFocusLocked moves keyboard focus to s, emitting leave/enter with
// an accurate list of held keys.
func (c *Compositor) setKeyboardFocusLocked(s *Surface) {
	if s == c.kbFocus {
		return
	}
	if c.kbFocus != nil {
		kbs := c.keyboardsByConn[c.kbFocus.conn]
		serial := c.nextSerial()
		for _, kb := range kbs {
			kb.Leave(serial, c.kbFocus.surface)
		}
	}
	c.kbFocus = s
	if s == nil {
		return
	}
	serial := c.nextSerial()
	keys := make([]byte, 4*len(c.kbHeldKeys))
	for i, k := range c.kbHeldKeys {
		binary.LittleEndian.PutUint32(keys[i*4:], k)
	}
	for _, kb := range c.keyboardsByConn[s.conn] {
		kb.Enter(serial, s.surface, keys)
	}
	c.sendModifiersLocked()
}

// forwardKeyLocked delivers one key event to the focused client.
func (c *Compositor) forwardKeyLocked(evdev uint32, pressed bool) {
	s := c.kbFocus
	if s == nil {
		return
	}
	var state uint32 = wl.WlKeyboardKeyStateReleased
	if pressed {
		state = wl.WlKeyboardKeyStatePressed
	}
	for _, kb := range c.keyboardsByConn[s.conn] {
		kb.Key(c.nextSerial(), 0, evdev, state)
	}
}

// sendModifiersLocked republishes modifier state to the focused client.
func (c *Compositor) sendModifiersLocked() {
	s := c.kbFocus
	if s == nil {
		return
	}
	var depressed uint32
	if c.mods.Shift {
		depressed |= 1 << 0
	}
	if c.mods.Ctrl {
		depressed |= 1 << 2
	}
	if c.mods.Alt {
		depressed |= 1 << 3
	}
	if c.mods.Super {
		depressed |= 1 << 6
	}
	for _, kb := range c.keyboardsByConn[s.conn] {
		kb.Modifiers(c.nextSerial(), depressed, 0, 0, 0)
	}
}

func (c *Compositor) surfaceAtLocked(x, y float64) *Surface {
	for o, tree := range c.trees {
		for _, w := range tree.Windows() {
			s := c.surfaceForWinLocked(w.ID)
			if s == nil {
				continue
			}
			r := effectiveRect(o, w, s)
			if x >= r.X && x < r.X+r.W && y >= r.Y && y < r.Y+r.H {
				s.rect = r
				return s
			}
		}
	}
	return nil
}

// effectiveRect reports the on-screen geometry for a window: its tiling rect,
// or the whole output when the client maximized or went fullscreen. Keeping
// this in one place keeps drawing, configuring and hit-testing consistent.
func effectiveRect(o *Output, w *layout.Win, s *Surface) layout.Rect {
	if s != nil && (s.maximized || s.fullscreen) {
		return layout.Rect{X: 0, Y: 0, W: float64(o.W), H: float64(o.H)}
	}
	return w.Rect
}

func (c *Compositor) primaryOutputLocked() *Output {
	if len(c.Outputs) == 0 {
		return nil
	}
	return c.Outputs[0]
}

// ---- control socket ----

func (c *Compositor) handleCommand(cmd, arg string) any {
	switch cmd {
	case "focus":
		c.mu.Lock()
		var id uint64
		if tree := c.primaryTreeLocked(); tree != nil {
			id = tree.Focus(arg)
		}
		c.mu.Unlock()
		return map[string]any{"focused": id}
	case "scroll":
		px, _ := strconv.ParseFloat(arg, 64)
		c.mu.Lock()
		if tree := c.primaryTreeLocked(); tree != nil {
			tree.ScrollBy(px)
		}
		c.mu.Unlock()
		return map[string]any{"ok": true}
	case "move":
		c.mu.Lock()
		if tree := c.primaryTreeLocked(); tree != nil {
			tree.Move(arg)
		}
		c.mu.Unlock()
		return map[string]any{"ok": true}
	case "mode":
		c.mu.Lock()
		for _, tree := range c.trees {
			switch arg {
			case "tile":
				tree.SetMode(layout.ModeTile)
			case "stack":
				tree.SetMode(layout.ModeStack)
			default:
				if tree.Mode == layout.ModeTile {
					tree.SetMode(layout.ModeStack)
				} else {
					tree.SetMode(layout.ModeTile)
				}
			}
		}
		c.mu.Unlock()
		return map[string]any{"ok": true}
	case "close":
		c.CloseFocused()
		return map[string]any{"ok": true}
	case "spawn":
		if c.Spawn != nil {
			parts := strings.Fields(arg)
			if len(parts) > 0 {
				c.Spawn(parts)
				return map[string]any{"ok": true}
			}
		}
		return map[string]any{"error": "spawn unavailable"}
	case "reload":
		if err := c.Store.ReloadNow(); err != nil {
			return map[string]any{"error": err.Error()}
		}
		return map[string]any{"ok": true}
	case "outputs":
		c.mu.Lock()
		defer c.mu.Unlock()
		var out []map[string]any
		for _, o := range c.Outputs {
			out = append(out, map[string]any{"name": o.Name, "size": itoa(o.W) + "x" + itoa(o.H), "refresh_mhz": o.Refresh})
		}
		return map[string]any{"outputs": out}
	case "status":
		c.mu.Lock()
		defer c.mu.Unlock()
		var out []map[string]any
		for o, tree := range c.trees {
			var wins []map[string]any
			for _, w := range tree.Windows() {
				r := w.Rect
				if s := c.surfaceForWinLocked(w.ID); s != nil {
					r = effectiveRect(o, w, s)
				}
				wins = append(wins, map[string]any{
					"id": w.ID, "title": w.Title, "app_id": w.AppID,
					"rect": itoa(int(r.W)) + "x" + itoa(int(r.H)) + "@" + itoa(int(r.X)) + "," + itoa(int(r.Y)),
				})
			}
			out = append(out, map[string]any{"output": o.Name, "mode": string(tree.Mode), "windows": wins})
		}
		sort.Slice(out, func(i, j int) bool {
			a, _ := out[i]["output"].(string)
			b, _ := out[j]["output"].(string)
			return a < b
		})
		return map[string]any{"outputs": out}
	}
	return map[string]any{"error": "unknown command " + cmd}
}

// ApplyConfig re-reads tuning knobs after a hot reload.
func (c *Compositor) ApplyConfig(cfg *config.Config) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for _, tree := range c.trees {
		tree.Gap = cfg.Layout.Gap
		tree.ColumnRatio = cfg.Layout.ColumnRatio
		if cfg.Layout.Mode == "tile" || cfg.Layout.Mode == "stack" {
			tree.SetMode(layout.Mode(cfg.Layout.Mode))
		}
	}
	for _, o := range c.Outputs {
		if o.Renderer != nil {
			o.Renderer.SetColorSpace(cfg.Color.Enabled, cfg.Color.TFName, cfg.Color.Primaries, cfg.Color.MaxLuma)
		}
	}
}

// SetTerminal records the spawn command used by the terminal binding.
func (c *Compositor) SetTerminal(argv []string) { c.terminalCmd = argv }

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func fixed(v float64) int32 { return int32(v * 256) }

func pressedUint(p bool) uint32 {
	if p {
		return 1
	}
	return 0
}

func itoa(v int) string { return strconv.Itoa(v) }
