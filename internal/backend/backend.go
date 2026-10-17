// Package backend drives one Lumine output. The MVP backend is a nested
// Wayland client: Lumine runs inside an existing compositor (Hyprland, GNOME,
// another Lumine, ...), presents frames through a wl_shm buffer, and forwards
// the parent seat's input. No libwayland, no DRM, no CGO.
//
// A bare-metal DRM/KMS backend plugs into the same Backend type later; the
// compositor only depends on Present(framebuffer) and the input callbacks.
package backend

import (
	"fmt"
	"os"
	"sync"

	"github.com/Team-Quaver/lumine/internal/render"
	"github.com/Team-Quaver/lumine/internal/wl"
	"golang.org/x/sys/unix"
)

// Input is implemented by the compositor, which consumes backend events.
type Input interface {
	SetMods(wl.Mods)
	Key(code uint32, pressed bool) bool
	Motion(dx, dy float64)
	MotionAbs(x, y float64)
	Button(btn uint32, pressed bool)
	Axis(dx, dy float64)
}

// Backend is the nested Wayland output.
type Backend struct {
	conn *wl.Conn
	reg  *wl.WlRegistry

	compositor *wl.WlCompositor
	shm        *wl.WlShm
	xdg        *wl.XdgWmBase

	surface  *wl.WlSurface
	xdgSurf  *wl.XdgSurface
	toplevel *wl.XdgToplevel

	width, height int
	title         string

	// OnClose is invoked when the host compositor asks the nested window to
	// close (the user clicked its close button). It must not block.
	OnClose func()

	mu     sync.Mutex
	bufs   []*frameBuf
	closed bool

	// parent input
	seat     *wl.WlSeat
	pointer  *wl.WlPointer
	keyboard *wl.WlKeyboard
	input    Input

	mods wl.Mods
	// pending scroll accumulated between compositor frames
	axisX, axisY float64

	needConfigure bool
}

// frameBuf is one wl_shm buffer plus its sealed memfd mapping.
type frameBuf struct {
	buf    *wl.WlBuffer
	data   []byte
	fd     int
	w, h   int
	busy   bool
	closed bool
}

// New connects to the parent compositor named by WAYLAND_DISPLAY and creates a
// toplevel window of the requested size.
func New(input Input, width, height int, title string) (*Backend, error) {
	name := os.Getenv("WAYLAND_DISPLAY")
	if name == "" {
		return nil, fmt.Errorf("backend: WAYLAND_DISPLAY is not set (nested backend needs a host compositor)")
	}
	conn, err := wl.Dial(wl.SocketPath(name))
	if err != nil {
		return nil, fmt.Errorf("backend: connect to %s: %w", name, err)
	}
	b := &Backend{conn: conn, width: width, height: height, title: title, input: input}
	if err := b.setup(); err != nil {
		conn.Close()
		return nil, err
	}
	return b, nil
}

func (b *Backend) setup() error {
	if err := b.conn.RoundTrip(); err != nil {
		return fmt.Errorf("backend: round trip: %w", err)
	}
	reg, err := b.conn.DisplayGetRegistry(&registryHandler{b: b})
	if err != nil {
		return err
	}
	b.reg = reg
	if err := b.conn.RoundTrip(); err != nil {
		return fmt.Errorf("backend: registry: %w", err)
	}
	if b.compositor == nil || b.shm == nil || b.xdg == nil {
		return fmt.Errorf("backend: host compositor is missing wl_compositor/wl_shm/xdg_wm_base")
	}

	b.surface = wl.NewWlSurface(b.conn, b.conn.AllocID(), 6, &surfaceHandler{b: b})
	if err := b.compositor.ReqCreateSurface(&b.surface.Object); err != nil {
		return err
	}
	b.xdgSurf = wl.NewXdgSurface(b.conn, b.conn.AllocID(), 6, &xdgSurfaceHandler{b: b})
	if err := b.xdg.ReqGetXdgSurface(&b.xdgSurf.Object, &b.surface.Object); err != nil {
		return err
	}
	b.toplevel = wl.NewXdgToplevel(b.conn, b.conn.AllocID(), 6, &toplevelHandler{b: b})
	if err := b.xdgSurf.ReqGetToplevel(&b.toplevel.Object); err != nil {
		return err
	}
	_ = b.toplevel.ReqSetTitle(b.title)
	_ = b.toplevel.ReqSetAppId("lumine")

	// The first commit with no buffer lets the compositor send the initial
	// xdg_toplevel configure.
	if err := b.surface.ReqCommit(); err != nil {
		return err
	}
	if err := b.conn.Flush(); err != nil {
		return err
	}
	if b.seat != nil {
		if err := b.bindSeat(); err != nil {
			return err
		}
	}
	if err := b.conn.RoundTrip(); err != nil {
		return fmt.Errorf("backend: initial configure: %w", err)
	}
	if b.width <= 0 || b.height <= 0 {
		b.width, b.height = 1280, 720
	}
	return nil
}

// Size returns the current presentation size.
func (b *Backend) Size() (int, int) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.width, b.height
}

// Conn exposes the parent connection so the main loop can multiplex its fd.
func (b *Backend) Conn() *wl.Conn { return b.conn }

// FD returns the parent socket fd for the event loop.
func (b *Backend) FD() int { return b.conn.FD() }

// Present copies the composited framebuffer into a shm buffer and commits it.
func (b *Backend) Present(fb *render.Framebuffer) error {
	if fb == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed {
		return nil
	}
	buf, err := b.acquireBufLocked(fb.Width, fb.Height)
	if err != nil {
		return err
	}
	if buf == nil {
		// Every buffer is still held by the host compositor; skip this frame
		// instead of growing the buffer pool without bound.
		return nil
	}
	copyFrame(buf.data, fb.Pix, fb.Stride, fb.Width, fb.Height)
	buf.busy = true
	if err := b.surface.ReqAttach(&buf.buf.Object, 0, 0); err != nil {
		return err
	}
	if err := b.surface.ReqDamageBuffer(0, 0, int32(fb.Width), int32(fb.Height)); err != nil {
		return err
	}
	if err := b.surface.ReqCommit(); err != nil {
		return err
	}
	return b.conn.Flush()
}

func (b *Backend) acquireBufLocked(w, h int) (*frameBuf, error) {
	for _, buf := range b.bufs {
		if !buf.busy && !buf.closed && buf.w == w && buf.h == h {
			return buf, nil
		}
	}
	// Three buffers cover double buffering plus one resize; beyond that the
	// host is not releasing buffers and we drop frames instead of leaking.
	if len(b.bufs) >= maxFrameBufs {
		return nil, nil
	}
	buf, err := newFrameBuf(b, w, h)
	if err != nil {
		return nil, err
	}
	b.bufs = append(b.bufs, buf)
	return buf, nil
}

// maxFrameBufs caps the per-output shm buffer pool.
const maxFrameBufs = 3

func newFrameBuf(b *Backend, w, h int) (*frameBuf, error) {
	stride := w * 4
	size := stride * h
	fd, err := unix.MemfdCreate("lumine-frame", unix.MFD_CLOEXEC|unix.MFD_ALLOW_SEALING)
	if err != nil {
		return nil, fmt.Errorf("backend: memfd: %w", err)
	}
	if err := unix.Ftruncate(fd, int64(size)); err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("backend: ftruncate: %w", err)
	}
	data, err := unix.Mmap(fd, 0, size, unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		unix.Close(fd)
		return nil, fmt.Errorf("backend: mmap: %w", err)
	}
	// Sealing keeps the client from shrinking the file out from under the
	// compositor, which would deliver SIGBUS on the host side.
	_, _ = unix.FcntlInt(uintptr(fd), unix.F_ADD_SEALS, unix.F_SEAL_SHRINK|unix.F_SEAL_GROW)

	pool := wl.NewWlShmPool(b.conn, b.conn.AllocID(), 1, nil)
	if err := b.shm.ReqCreatePool(&pool.Object, fd, int32(size)); err != nil {
		unix.Munmap(data)
		unix.Close(fd)
		return nil, err
	}
	buf := wl.NewWlBuffer(b.conn, b.conn.AllocID(), 1, &bufferHandler{b: b})
	if err := pool.ReqCreateBuffer(&buf.Object, 0, int32(w), int32(h), int32(stride), uint32(shmFormatXRGB)); err != nil {
		unix.Munmap(data)
		unix.Close(fd)
		return nil, err
	}
	_ = pool.ReqDestroy()
	return &frameBuf{buf: buf, data: data, fd: fd, w: w, h: h}, nil
}

const shmFormatXRGB = 1 // wl_shm.format XRGB8888

func copyFrame(dst []byte, src []byte, srcStride, w, h int) {
	dstStride := w * 4
	for y := 0; y < h; y++ {
		copy(dst[y*dstStride:(y+1)*dstStride], src[y*srcStride:y*srcStride+dstStride])
	}
}

// ---- parent registry ----

type registryHandler struct{ b *Backend }

func (h *registryHandler) Global(reg *wl.WlRegistry, name uint32, iface string, version uint32) {
	b := h.b
	switch iface {
	case "wl_compositor":
		v := minU32(version, 6)
		b.compositor = wl.NewWlCompositor(b.conn, b.conn.AllocID(), v, nil)
		_ = reg.ReqBind(name, iface, v, &b.compositor.Object)
	case "wl_shm":
		v := minU32(version, 1)
		b.shm = wl.NewWlShm(b.conn, b.conn.AllocID(), v, nil)
		_ = reg.ReqBind(name, iface, v, &b.shm.Object)
	case "xdg_wm_base":
		v := minU32(version, 6)
		b.xdg = wl.NewXdgWmBase(b.conn, b.conn.AllocID(), v, &wmBaseHandler{})
		_ = reg.ReqBind(name, iface, v, &b.xdg.Object)
	case "wl_seat":
		v := minU32(version, 8)
		b.seat = wl.NewWlSeat(b.conn, b.conn.AllocID(), v, &seatHandler{b: b})
		_ = reg.ReqBind(name, iface, v, &b.seat.Object)
	}
}

func (h *registryHandler) GlobalRemove(o *wl.WlRegistry, name uint32) {}

func (b *Backend) bindSeat() error {
	if b.seat == nil {
		return nil
	}
	b.pointer = wl.NewWlPointer(b.conn, b.conn.AllocID(), 8, &pointerHandler{b: b})
	if err := b.seat.ReqGetPointer(&b.pointer.Object); err != nil {
		return err
	}
	b.keyboard = wl.NewWlKeyboard(b.conn, b.conn.AllocID(), 8, &keyboardHandler{b: b})
	return b.seat.ReqGetKeyboard(&b.keyboard.Object)
}

// ---- xdg shell events ----

type wmBaseHandler struct{}

func (h *wmBaseHandler) Ping(o *wl.XdgWmBase, serial uint32) { _ = o.ReqPong(serial) }

type surfaceHandler struct{ b *Backend }

func (h *surfaceHandler) Enter(o *wl.WlSurface, output *wl.WlOutput)                 {}
func (h *surfaceHandler) Leave(o *wl.WlSurface, output *wl.WlOutput)                 {}
func (h *surfaceHandler) PreferredBufferScale(o *wl.WlSurface, factor int32)         {}
func (h *surfaceHandler) PreferredBufferTransform(o *wl.WlSurface, transform uint32) {}

type xdgSurfaceHandler struct{ b *Backend }

func (h *xdgSurfaceHandler) Configure(o *wl.XdgSurface, serial uint32) {
	_ = o.ReqAckConfigure(serial)
	h.b.needConfigure = true
}

type toplevelHandler struct{ b *Backend }

func (h *toplevelHandler) Configure(o *wl.XdgToplevel, width, height int32, states []byte) {
	b := h.b
	b.mu.Lock()
	if width > 0 && height > 0 {
		b.width, b.height = int(width), int(height)
	}
	b.mu.Unlock()
}

func (h *toplevelHandler) Close(o *wl.XdgToplevel) {
	if h.b.OnClose != nil {
		h.b.OnClose()
	}
}
func (h *toplevelHandler) ConfigureBounds(o *wl.XdgToplevel, width, height int32) {}
func (h *toplevelHandler) WmCapabilities(o *wl.XdgToplevel, capabilities []byte)  {}

// ---- buffer release ----

type bufferHandler struct{ b *Backend }

func (h *bufferHandler) Release(o *wl.WlBuffer) {
	h.b.mu.Lock()
	defer h.b.mu.Unlock()
	for _, buf := range h.b.bufs {
		if buf.buf == o {
			buf.busy = false
			return
		}
	}
}

// ---- parent seat ----

type seatHandler struct{ b *Backend }

func (h *seatHandler) Capabilities(o *wl.WlSeat, capabilities uint32) {
	// Pointer and keyboard proxies are requested once during setup; capability
	// changes after that are rare in the MVP and simply ignored.
}

func (h *seatHandler) Name(o *wl.WlSeat, name string) {}

type pointerHandler struct{ b *Backend }

func (h *pointerHandler) Enter(o *wl.WlPointer, serial uint32, surface *wl.WlSurface, sx, sy wl.Fixed) {
	if surface != h.b.surface {
		return
	}
	h.b.input.MotionAbs(sx.Float(), sy.Float())
}
func (h *pointerHandler) Leave(o *wl.WlPointer, serial uint32, surface *wl.WlSurface) {}
func (h *pointerHandler) Motion(o *wl.WlPointer, time uint32, sx, sy wl.Fixed) {
	h.b.input.MotionAbs(sx.Float(), sy.Float())
}
func (h *pointerHandler) Button(o *wl.WlPointer, serial uint32, time uint32, button uint32, state uint32) {
	h.b.input.Button(button, state == wl.WlPointerButtonStatePressed)
}
func (h *pointerHandler) Axis(o *wl.WlPointer, time uint32, axis uint32, value wl.Fixed) {
	switch axis {
	case wl.WlPointerAxisHorizontalScroll:
		h.b.axisX += value.Float()
	case wl.WlPointerAxisVerticalScroll:
		h.b.axisY += value.Float()
	}
}
func (h *pointerHandler) Frame(o *wl.WlPointer) {
	if h.b.axisX != 0 || h.b.axisY != 0 {
		h.b.input.Axis(h.b.axisX, h.b.axisY)
		h.b.axisX, h.b.axisY = 0, 0
	}
}
func (h *pointerHandler) AxisSource(o *wl.WlPointer, axisSource uint32)                  {}
func (h *pointerHandler) AxisStop(o *wl.WlPointer, time uint32, axis uint32)             {}
func (h *pointerHandler) AxisDiscrete(o *wl.WlPointer, axis uint32, d int32)             {}
func (h *pointerHandler) AxisValue120(o *wl.WlPointer, axis uint32, v int32)             {}
func (h *pointerHandler) AxisRelativeDirection(o *wl.WlPointer, axis uint32, dir uint32) {}
func (h *pointerHandler) Warp(o *wl.WlPointer, sx, sy wl.Fixed) {
	h.b.input.MotionAbs(sx.Float(), sy.Float())
}

type keyboardHandler struct{ b *Backend }

func (h *keyboardHandler) Keymap(o *wl.WlKeyboard, format uint32, fd int, size uint32) {
	// The parent keymap is only informational for Lumine's own forwarding: we
	// pass evdev keycodes through unchanged and advertise our own US keymap to
	// Lumine's clients. Close the fd regardless.
	unix.Close(fd)
}

func (h *keyboardHandler) Enter(o *wl.WlKeyboard, serial uint32, surface *wl.WlSurface, keys []byte) {
}
func (h *keyboardHandler) Leave(o *wl.WlKeyboard, serial uint32, surface *wl.WlSurface) {}
func (h *keyboardHandler) Key(o *wl.WlKeyboard, serial uint32, time uint32, key uint32, state uint32) {
	pressed := state == wl.WlKeyboardKeyStatePressed || state == wl.WlKeyboardKeyStateRepeated
	h.b.input.Key(key, pressed)
}
func (h *keyboardHandler) Modifiers(o *wl.WlKeyboard, serial, depressed, latched, locked, group uint32) {
	m := wl.Mods{
		Super: depressed&modSuper != 0 || locked&modSuper != 0,
		Shift: depressed&modShift != 0,
		Ctrl:  depressed&modCtrl != 0,
		Alt:   depressed&modAlt != 0,
	}
	h.b.input.SetMods(m)
}
func (h *keyboardHandler) RepeatInfo(o *wl.WlKeyboard, rate, delay int32) {}

const (
	modShift = 1 << 0
	modCtrl  = 1 << 2
	modAlt   = 1 << 3
	modSuper = 1 << 6
)

func minU32(a, b uint32) uint32 {
	if a < b {
		return a
	}
	return b
}

// Close tears the nested window down.
func (b *Backend) Close() {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	b.closed = true
	bufs := b.bufs
	b.bufs = nil
	b.mu.Unlock()
	for _, buf := range bufs {
		if buf.data != nil {
			_ = unix.Munmap(buf.data)
		}
		if buf.fd > 0 {
			_ = unix.Close(buf.fd)
			buf.fd = -1
		}
	}
	if b.conn != nil {
		b.conn.Close()
	}
}
