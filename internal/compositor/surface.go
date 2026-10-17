package compositor

import (
	"encoding/binary"

	"github.com/Team-Quaver/lumine/internal/layout"
	"github.com/Team-Quaver/lumine/internal/shm"
	"github.com/Team-Quaver/lumine/internal/wl"
	"golang.org/x/sys/unix"
)

// Compile-time conformance checks. The generated dispatchers type-assert on
// these exact interfaces; a missing method silently disables the whole
// resource (Surface.CreateSurface would never run), so keep the assertions
// coupled to the generated interfaces.
var (
	_ wl.WlCompositorHandler               = (*compositorHandler)(nil)
	_ wl.WlRegionHandler                   = (*regionHandler)(nil)
	_ wl.WlSurfaceHandler                  = (*surfaceHandler)(nil)
	_ wl.WlShmHandler                      = (*shmHandler)(nil)
	_ wl.WlShmPoolHandler                  = (*shmPoolHandler)(nil)
	_ wl.WlSeatHandler                     = (*seatHandler)(nil)
	_ wl.WlPointerHandler                  = (*pointerHandler)(nil)
	_ wl.WlKeyboardHandler                 = (*keyboardHandler)(nil)
	_ wl.XdgWmBaseHandler                  = (*xdgWmBaseHandler)(nil)
	_ wl.XdgSurfaceHandler                 = (*xdgSurfaceHandler)(nil)
	_ wl.XdgToplevelHandler                = (*toplevelHandler)(nil)
	_ wl.ZxdgDecorationManagerV1Handler    = (*decorationHandler)(nil)
	_ wl.ZxdgToplevelDecorationV1Handler   = (*decoHandler)(nil)
	_ wl.WpViewporterHandler               = (*viewporterHandler)(nil)
	_ wl.WpViewportHandler                 = (*viewportHandler)(nil)
	_ wl.WpColorManagerV1Handler           = (*colorManagerHandler)(nil)
	_ wl.WpColorManagementOutputV1Handler  = (*colorOutputHandler)(nil)
	_ wl.WpColorManagementSurfaceV1Handler = (*colorSurfaceHandler)(nil)
)

// Surface is the compositor state for one wl_surface. Roles (toplevel) and
// buffers hang off it; layout geometry mirrors layout.Win.Rect.
type Surface struct {
	c       *Compositor
	surface *wl.WlSurface
	conn    *wl.Conn

	// committed buffer state
	image *shm.Image

	// buffer lifetime: buf is the wl_buffer committed with image; bufPending
	// means the compositor still reads it and must send wl_buffer.release.
	buf        *wl.WlBuffer
	bufPending bool

	// releaseCallbacks holds wl_surface.get_release callbacks that fire once
	// the current buffer is no longer read.
	releaseCallbacks []*wl.WlCallback

	rect  layout.Rect
	winID uint64

	title string
	appID string

	// role objects
	toplevel  *wl.XdgToplevel
	xdg       *wl.XdgSurface
	deco      *wl.ZxdgToplevelDecorationV1
	decorMode uint32 // 0 unset, 1 SSD, 2 CSD

	// xdg_toplevel maximize/fullscreen state. Both make the window cover the
	// whole output; the layout still computes the underlying tiling rect so the
	// window returns to its slot on unset.
	maximized  bool
	fullscreen bool

	// srcColor is the wp_color_manager surface image description, if a client
	// assigned one. Nil means "untagged" (the client is treated as sRGB).
	srcColor *imageDesc

	// input proxies
	pointer  *wl.WlPointer
	keyboard *wl.WlKeyboard

	pendingFrames []*wl.WlCallback

	// pending wl_surface state, applied on commit
	pendBuffer  *wl.WlBuffer
	hasPend     bool
	ackedSerial uint32

	// last xdg_toplevel configure pushed to the client. A configure is only
	// re-sent when the wanted geometry or activation state actually changes;
	// without this guard the frame pump would emit one every frame (a
	// configure storm).
	cfgSent    bool
	cfgW, cfgH int32
	cfgFocused bool
	cfgMax     bool
	cfgFull    bool

	// owned is the compositor's private copy of the committed buffer, so the
	// renderer never touches client memory after the commit (the client may
	// unmap its pool at any time).
	owned     []byte
	ownedW    int
	ownedH    int
	ownedStrd int
	ownedFmt  shm.Format
}

// ssdTitleHeight is the title-bar height the renderer paints for decorated
// windows. The layout reserves the same amount of space above the window.
const ssdTitleHeight = 28.0

// serverDecorated reports whether the compositor paints decorations for this
// surface. A client that requested client-side decorations never does, which
// is how CSD and SSD coexist.
func (s *Surface) serverDecorated() bool {
	csd := uint32(wl.ZxdgToplevelDecorationV1ModeClientSide)
	if s.decorMode == csd {
		return false
	}
	if s.decorMode == uint32(wl.ZxdgToplevelDecorationV1ModeServerSide) {
		return true
	}
	return s.c.Store.Config().Decor.Mode != "csd"
}

// windowTitle is the title used for server-side decoration rendering.
func (s *Surface) windowTitle() string {
	if s.title != "" {
		return s.title
	}
	return s.appID
}

// ---- wl_compositor ----

type compositorHandler struct{ c *Compositor }

func (h *compositorHandler) CreateSurface(o *wl.WlCompositor, id *wl.WlSurface) {
	s := &Surface{c: h.c, surface: id, conn: o.Conn}
	id.Impl = &surfaceHandler{c: h.c}
	h.c.mu.Lock()
	h.c.surfaces[id] = s
	h.c.mu.Unlock()
}

func (h *compositorHandler) CreateRegion(o *wl.WlCompositor, id *wl.WlRegion) {
	id.Impl = &regionHandler{}
}

func (h *compositorHandler) Release(o *wl.WlCompositor) {}

// ---- wl_region ----

type regionHandler struct{}

func (h *regionHandler) Destroy(o *wl.WlRegion)                             {}
func (h *regionHandler) Add(o *wl.WlRegion, x, y, width, height int32)      {}
func (h *regionHandler) Subtract(o *wl.WlRegion, x, y, width, height int32) {}

// ---- wl_surface ----

type surfaceHandler struct{ c *Compositor }

func (h *surfaceHandler) Attach(o *wl.WlSurface, buffer *wl.WlBuffer, x, y int32) {
	h.c.mu.Lock()
	defer h.c.mu.Unlock()
	if s := h.c.surfaces[o]; s != nil {
		s.pendBuffer = buffer
		s.hasPend = true
	}
}

func (h *surfaceHandler) Damage(o *wl.WlSurface, x, y, width, height int32) {}

func (h *surfaceHandler) Frame(o *wl.WlSurface, callback *wl.WlCallback) {
	h.c.mu.Lock()
	defer h.c.mu.Unlock()
	if s := h.c.surfaces[o]; s != nil {
		s.pendingFrames = append(s.pendingFrames, callback)
	}
}

func (h *surfaceHandler) SetOpaqueRegion(o *wl.WlSurface, region *wl.WlRegion) {}
func (h *surfaceHandler) SetInputRegion(o *wl.WlSurface, region *wl.WlRegion)  {}
func (h *surfaceHandler) Commit(o *wl.WlSurface) {
	h.c.mu.Lock()
	defer h.c.mu.Unlock()
	s := h.c.surfaces[o]
	if s == nil || !s.hasPend {
		return
	}
	s.hasPend = false
	if s.pendBuffer == nil {
		s.image = nil
		s.buf = nil
		return
	}
	img, ok := s.pendBuffer.Data.(*shm.Image)
	if !ok || img == nil {
		return
	}
	s.buf = s.pendBuffer
	s.bufPending = true
	s.snapshotLocked(img)
}

// snapshotLocked copies the freshly committed pixels into compositor-owned
// storage. The double buffer keeps allocations down on the common path where
// a client commits buffers of the same geometry.
func (s *Surface) snapshotLocked(img *shm.Image) {
	need := img.Stride * img.Height
	if need <= 0 {
		s.image = nil
		return
	}
	if cap(s.owned) < need {
		s.owned = make([]byte, need)
	}
	s.owned = s.owned[:need]
	// Only copy the bytes described by the buffer; rows past the image width
	// belong to the client, so keep the per-row stride semantics.
	for y := 0; y < img.Height; y++ {
		src := img.Pix[y*img.Stride : y*img.Stride+img.Stride]
		copy(s.owned[y*img.Stride:], src)
	}
	s.ownedW, s.ownedH, s.ownedStrd, s.ownedFmt = img.Width, img.Height, img.Stride, img.Format
	s.image = &shm.Image{Pix: s.owned, Width: img.Width, Height: img.Height, Stride: img.Stride, Format: img.Format}
}

func (h *surfaceHandler) SetBufferTransform(o *wl.WlSurface, transform int32)     {}
func (h *surfaceHandler) SetBufferScale(o *wl.WlSurface, scale int32)             {}
func (h *surfaceHandler) DamageBuffer(o *wl.WlSurface, x, y, width, height int32) {}
func (h *surfaceHandler) Offset(o *wl.WlSurface, x, y int32)                      {}

// GetRelease serves wl_surface.get_release (protocol version >= 6): the
// callback fires once the compositor will no longer read the current buffer.
func (h *surfaceHandler) GetRelease(o *wl.WlSurface, callback *wl.WlCallback) {
	h.c.mu.Lock()
	defer h.c.mu.Unlock()
	s := h.c.surfaces[o]
	if s == nil {
		callback.Done(h.c.nextSerial())
		callback.Object.Destroy()
		return
	}
	s.releaseCallbacks = append(s.releaseCallbacks, callback)
}
func (h *surfaceHandler) Destroy(o *wl.WlSurface) {
	h.c.removeSurface(o)
}

// removeSurface detaches a surface and drops its layout window.
func (c *Compositor) removeSurface(o *wl.WlSurface) {
	c.mu.Lock()
	s := c.surfaces[o]
	delete(c.surfaces, o)
	if c.kbFocus == s {
		c.setKeyboardFocusLocked(nil)
	}
	if c.pointSurface == s {
		c.pointSurface = nil
	}
	c.mu.Unlock()
	if s == nil {
		return
	}
	if s.winID != 0 {
		for _, tree := range c.trees {
			if tree.Remove(s.winID) {
				break
			}
		}
	}
}

// ---- wl_shm ----

type shmHandler struct{ c *Compositor }

func (h *shmHandler) CreatePool(o *wl.WlShm, id *wl.WlShmPool, fd int, size int32) {
	pool, err := shm.MapPool(fd, size)
	if err != nil {
		// MapPool did not take ownership on error, so close the fd ourselves.
		_ = unix.Close(fd)
		o.Conn.PostError(o, wl.WlShmErrorInvalidFd, err.Error())
		return
	}
	id.Data = &poolBuf{pool: pool}
	id.Impl = &shmPoolHandler{c: h.c}
}

func (h *shmHandler) Release(o *wl.WlShm) {}

// poolBuf hangs buffer metadata off the generated resource via Data.
type poolBuf struct {
	pool *shm.Pool
}

type shmPoolHandler struct{ c *Compositor }

func (h *shmPoolHandler) CreateBuffer(o *wl.WlShmPool, id *wl.WlBuffer, offset, width, height, stride int32, format uint32) {
	pb, _ := o.Data.(*poolBuf)
	if pb == nil {
		o.Conn.PostError(o, wl.WlShmPoolErrorInvalidFormat, "shm pool is not mapped")
		return
	}
	if format != uint32(shm.ARGB8888) && format != uint32(shm.XRGB8888) {
		o.Conn.PostError(o, wl.WlShmPoolErrorInvalidFormat, "unsupported shm format")
		return
	}
	img, err := pb.pool.Image(offset, width, height, stride, shm.Format(format))
	if err != nil {
		o.Conn.PostError(o, wl.WlShmPoolErrorInvalidStride, err.Error())
		return
	}
	// Image holds a reference on the pool: the client may destroy the
	// wl_shm_pool before it is done committing buffers made from it.
	id.Data = img
	id.Impl = &bufferHandler{c: h.c}
}

func (h *shmPoolHandler) Destroy(o *wl.WlShmPool) {
	if pb, ok := o.Data.(*poolBuf); ok && pb.pool != nil {
		pb.pool.Destroy()
	}
}

func (h *shmPoolHandler) Resize(o *wl.WlShmPool, size int32) {
	if pb, ok := o.Data.(*poolBuf); ok && pb.pool != nil {
		if err := pb.pool.Resize(size); err != nil {
			o.Conn.PostError(o, wl.WlShmPoolErrorInvalidStride, err.Error())
		}
	}
}

// ---- wl_buffer ----

// bufferHandler drops the shm mapping reference a wl_buffer holds, and stops
// the frame pump from sending wl_buffer.release to an object the client has
// already deleted.
type bufferHandler struct{ c *Compositor }

func (h *bufferHandler) Destroy(o *wl.WlBuffer) {
	if img, ok := o.Data.(*shm.Image); ok {
		img.Release()
	}
	h.c.bufferGone(o)
}
func (h *bufferHandler) Release(o *wl.WlBuffer) {}

// ---- wl_seat ----

type seatHandler struct{ c *Compositor }

func (h *seatHandler) GetPointer(o *wl.WlSeat, id *wl.WlPointer) {
	c := h.c
	id.Impl = &pointerHandler{c: c}
	c.mu.Lock()
	defer c.mu.Unlock()
	// The pointer proxy binds to the seat's client; we route events through
	// whichever surface of that client is under the cursor.
	c.pointersByConn[o.Conn] = append(c.pointersByConn[o.Conn], id)
}

func (h *seatHandler) GetRegion(o *wl.WlSeat, id *wl.WlRegion) {}
func (h *seatHandler) GetKeyboard(o *wl.WlSeat, id *wl.WlKeyboard) {
	c := h.c
	id.Impl = &keyboardHandler{c: c}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.keyboardsByConn[o.Conn] = append(c.keyboardsByConn[o.Conn], id)
	sendKeymap(id)
	// A client that binds the seat late still needs the current focus state.
	if fs := c.kbFocus; fs != nil && fs.conn == o.Conn {
		keys := make([]byte, 4*len(c.kbHeldKeys))
		for i, k := range c.kbHeldKeys {
			binary.LittleEndian.PutUint32(keys[i*4:], k)
		}
		id.Enter(c.nextSerial(), fs.surface, keys)
	}
	rep := c.Store.Config().Input.Repeat
	rate, delay := 25, 300
	if rep.Rate > 0 {
		rate = rep.Rate
	}
	if rep.Delay > 0 {
		delay = rep.Delay
	}
	id.RepeatInfo(int32(rate), int32(delay))
}

func (h *seatHandler) GetTouch(o *wl.WlSeat, id *wl.WlTouch) {
	id.Impl = &touchHandler{}
}
func (h *seatHandler) Release(o *wl.WlSeat) {}

type touchHandler struct{}

func (h *touchHandler) Release(o *wl.WlTouch) {}

type pointerHandler struct{ c *Compositor }

func (h *pointerHandler) SetCursor(o *wl.WlPointer, serial uint32, surface *wl.WlSurface, hotspotX, hotspotY int32) {
}
func (h *pointerHandler) Release(o *wl.WlPointer) {}

type keyboardHandler struct{ c *Compositor }

func (h *keyboardHandler) Release(o *wl.WlKeyboard) {}

// ---- xdg_wm_base ----

type xdgWmBaseHandler struct{ c *Compositor }

func (h *xdgWmBaseHandler) Destroy(o *wl.XdgWmBase)                                {}
func (h *xdgWmBaseHandler) CreatePositioner(o *wl.XdgWmBase, id *wl.XdgPositioner) {}
func (h *xdgWmBaseHandler) GetXdgSurface(o *wl.XdgWmBase, id *wl.XdgSurface, surface *wl.WlSurface) {
	h.c.mu.Lock()
	defer h.c.mu.Unlock()
	s := h.c.surfaces[surface]
	if s == nil {
		o.Conn.PostError(id, 3, "unknown wl_surface")
		return
	}
	s.xdg = id
	id.Impl = &xdgSurfaceHandler{c: h.c, s: s}
}

func (h *xdgWmBaseHandler) Pong(o *wl.XdgWmBase, serial uint32) {}

type xdgSurfaceHandler struct {
	c *Compositor
	s *Surface
}

func (h *xdgSurfaceHandler) Destroy(o *wl.XdgSurface) {}
func (h *xdgSurfaceHandler) GetToplevel(o *wl.XdgSurface, id *wl.XdgToplevel) {
	c := h.c
	c.mu.Lock()
	defer c.mu.Unlock()
	h.s.toplevel = id
	id.Impl = &toplevelHandler{c: c, s: h.s}
	// xdg-shell v5 requires wm_capabilities before the first xdg_surface.configure.
	// Send it before the output check: a toplevel may be created before an output
	// is available, but the capability event must still precede any later configure.
	// Older bindings must not receive the event at all.
	if id.Version >= 5 {
		id.WmCapabilities(wmCapabilities())
	}
	tree := c.primaryTreeLocked()
	if tree == nil {
		return
	}
	win := tree.Add(h.s.title, h.s.appID)
	h.s.winID = win.ID
	// Initial configure: full output size minus a margin; layout refines on
	// the next frame.
	if out := c.primaryOutputLocked(); out != nil {
		id.Configure(int32(out.W), int32(out.H), h.s.toplevelState(false))
	}
	o.Configure(c.nextSerial())
}

// wmCapabilities builds the xdg_toplevel.wm_capabilities array. Lumine honours
// maximize and fullscreen by covering the output. window_menu and minimize are
// intentionally not advertised because those requests are not implemented.
func wmCapabilities() []byte {
	caps := []uint32{
		wl.XdgToplevelWmCapabilitiesMaximize,
		wl.XdgToplevelWmCapabilitiesFullscreen,
	}
	buf := make([]byte, 4*len(caps))
	for i, c := range caps {
		binary.NativeEndian.PutUint32(buf[i*4:], c)
	}
	return buf
}

func (h *xdgSurfaceHandler) GetPopup(o *wl.XdgSurface, id *wl.XdgPopup, parent *wl.XdgSurface, positioner *wl.XdgPositioner) {
}
func (h *xdgSurfaceHandler) SetWindowGeometry(o *wl.XdgSurface, x, y, width, height int32) {}
func (h *xdgSurfaceHandler) AckConfigure(o *wl.XdgSurface, serial uint32) {
	h.s.ackedSerial = serial
}

type toplevelHandler struct {
	c *Compositor
	s *Surface
}

func (h *toplevelHandler) Destroy(o *wl.XdgToplevel) {
	h.c.removeSurface(h.s.surface)
}

func (h *toplevelHandler) SetParent(o *wl.XdgToplevel, parent *wl.XdgToplevel) {}
func (h *toplevelHandler) SetTitle(o *wl.XdgToplevel, title string) {
	h.c.mu.Lock()
	defer h.c.mu.Unlock()
	h.s.title = title
	h.c.updateWindowLocked(h.s, func(w *layout.Win) { w.Title = title })
}

func (h *toplevelHandler) SetAppId(o *wl.XdgToplevel, appID string) {
	h.c.mu.Lock()
	defer h.c.mu.Unlock()
	h.s.appID = appID
	h.c.updateWindowLocked(h.s, func(w *layout.Win) { w.AppID = appID })
}

// updateWindowLocked mutates the layout window backing a surface.
func (c *Compositor) updateWindowLocked(s *Surface, fn func(*layout.Win)) {
	if s == nil || s.winID == 0 {
		return
	}
	for _, tree := range c.trees {
		for _, w := range tree.Windows() {
			if w.ID == s.winID {
				fn(w)
				return
			}
		}
	}
}

func (h *toplevelHandler) ShowWindowMenu(o *wl.XdgToplevel, seat *wl.WlSeat, serial uint32, x, y int32) {
}
func (h *toplevelHandler) Move(o *wl.XdgToplevel, seat *wl.WlSeat, serial uint32)                 {}
func (h *toplevelHandler) Resize(o *wl.XdgToplevel, seat *wl.WlSeat, serial uint32, edges uint32) {}
func (h *toplevelHandler) SetMaxSize(o *wl.XdgToplevel, width, height int32)                      {}
func (h *toplevelHandler) SetMinSize(o *wl.XdgToplevel, width, height int32)                      {}

// SetMaximized / UnsetMaximized make the window cover its output; the next
// frame re-configures it with (or without) the maximized state.
func (h *toplevelHandler) SetMaximized(o *wl.XdgToplevel)   { h.setMaximizedLocked(true) }
func (h *toplevelHandler) UnsetMaximized(o *wl.XdgToplevel) { h.setMaximizedLocked(false) }

func (h *toplevelHandler) SetFullscreen(o *wl.XdgToplevel, output *wl.WlOutput) {
	h.c.mu.Lock()
	defer h.c.mu.Unlock()
	if h.s.fullscreen {
		return
	}
	h.s.fullscreen = true
	h.s.cfgSent = false
}

func (h *toplevelHandler) UnsetFullscreen(o *wl.XdgToplevel) {
	h.c.mu.Lock()
	defer h.c.mu.Unlock()
	if !h.s.fullscreen {
		return
	}
	h.s.fullscreen = false
	h.s.cfgSent = false
}

// SetMinimized is ignored defensively. Minimize is not advertised because
// Lumine has no task switcher.
func (h *toplevelHandler) SetMinimized(o *wl.XdgToplevel) {}

// toplevelState builds the xdg_toplevel state array for a configure.
func (s *Surface) toplevelState(focused bool) []byte {
	add := func(v uint32) []byte {
		buf := make([]byte, 4)
		binary.NativeEndian.PutUint32(buf, v)
		return buf
	}
	var states []byte
	if s.maximized {
		states = append(states, add(wl.XdgToplevelStateMaximized)...)
	}
	if s.fullscreen {
		states = append(states, add(wl.XdgToplevelStateFullscreen)...)
	}
	if focused {
		states = append(states, add(wl.XdgToplevelStateActivated)...)
	}
	return states
}

// setMaximizedLocked / setFullscreenLocked record a client maximize/fullscreen
// request and force a configure so the state reaches the client.
func (h *toplevelHandler) setMaximizedLocked(on bool) {
	h.c.mu.Lock()
	defer h.c.mu.Unlock()
	if h.s.maximized == on {
		return
	}
	h.s.maximized = on
	h.s.cfgSent = false // force re-configure with the new state
}

// ---- xdg-decoration (CSD/SSD coexistence) ----

type decorationHandler struct{ c *Compositor }

func (h *decorationHandler) Destroy(o *wl.ZxdgDecorationManagerV1) {}
func (h *decorationHandler) GetToplevelDecoration(o *wl.ZxdgDecorationManagerV1, id *wl.ZxdgToplevelDecorationV1, toplevel *wl.XdgToplevel) {
	c := h.c
	c.mu.Lock()
	var s *Surface
	for _, cand := range c.surfaces {
		if cand.toplevel == toplevel {
			s = cand
			break
		}
	}
	c.mu.Unlock()
	if s == nil {
		return
	}
	s.deco = id
	id.Impl = &decoHandler{c: c, s: s}
	// Server preference: SSD when the client has not chosen CSD. Clients may
	// override at any time and the compositor honors the last request.
	mode := uint32(wl.ZxdgToplevelDecorationV1ModeServerSide)
	if cfg := c.Store.Config(); cfg.Decor.Mode == "csd" {
		mode = wl.ZxdgToplevelDecorationV1ModeClientSide
	}
	if s.decorMode != 0 {
		// The client already picked a mode (it may have raced ahead of the
		// manager); honor it.
		mode = s.decorMode
	} else {
		s.decorMode = mode
	}
	id.Configure(mode)
}

type decoHandler struct {
	c *Compositor
	s *Surface
}

func (h *decoHandler) Destroy(o *wl.ZxdgToplevelDecorationV1) {}
func (h *decoHandler) SetMode(o *wl.ZxdgToplevelDecorationV1, mode uint32) {
	h.s.decorMode = mode
	o.Configure(mode) // confirm the final mode
}
func (h *decoHandler) UnsetMode(o *wl.ZxdgToplevelDecorationV1) {}

// ---- wp_viewporter ----

type viewporterHandler struct{ c *Compositor }

func (h *viewporterHandler) Destroy(o *wl.WpViewporter) {}
func (h *viewporterHandler) GetViewport(o *wl.WpViewporter, id *wl.WpViewport, surface *wl.WlSurface) {
	id.Impl = &viewportHandler{}
}

type viewportHandler struct{}

func (h *viewportHandler) Destroy(o *wl.WpViewport)                                 {}
func (h *viewportHandler) SetSource(o *wl.WpViewport, x, y, width, height wl.Fixed) {}
func (h *viewportHandler) SetDestination(o *wl.WpViewport, width, height int32)     {}
