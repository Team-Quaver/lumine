package backend

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/Team-Quaver/lumine/internal/drm"
	"github.com/Team-Quaver/lumine/internal/evdev"
	"github.com/Team-Quaver/lumine/internal/render"
	"golang.org/x/sys/unix"
)

// DRMConfig selects the connector and mode the DRM backend brings up. Every
// field is optional: the zero value drives the first connected connector at
// its preferred mode and native refresh rate.
type DRMConfig struct {
	// Device forces a specific node ("/dev/dri/card1"). Empty autodetects the
	// first card that owns the requested connector. LUMINE_DRM_DEVICE and
	// WLR_DRM_DEVICES (first entry) are honoured when this is empty.
	Device string

	// Connector names the KMS connector to drive, e.g. "eDP-1" or "HDMI-A-1".
	// It maps onto [Display].monitor; empty picks the first connected one.
	Connector string

	// Width/Height request an exact mode resolution. Zero uses the connector's
	// preferred mode, falling back to its largest mode.
	Width, Height int

	// RefreshMHz picks among same-resolution modes; zero means the highest the
	// connector offers.
	RefreshMHz int32

	// TripleBuffering allocates a third scanout buffer. The kernel allows only
	// one page flip in flight per CRTC, so this is headroom for the flip
	// completion to land early rather than a latency win.
	TripleBuffering bool

	// Logf, when set, receives human-readable progress lines: the negotiated
	// mode and the first flip's completion. A bare-metal bring-up that shows a
	// black screen is far easier to diagnose with these than with silence.
	Logf func(format string, args ...any)
}

// DRM drives one KMS connector. Frames arrive as a CPU-visible render
// Framebuffer (the Vulkan renderer rasterises offscreen and reads the result
// back) and are copied into a DRM dumb buffer, which is then flipped onto the
// CRTC with a page-flip ioctl.
//
// The dumb-buffer copy is deliberate. It is the only present path that works
// on Intel, AMD and NVIDIA alike: importing a linear dumb buffer as a Vulkan
// image and rendering straight into it is rejected by i915 (the subresource
// layout is larger than the allocation) and GPU writes to an imported dma-buf
// are not coherent through the CPU mapping on NVIDIA. Copying a readback into
// a dumb buffer avoids both traps and keeps the renderer backend-agnostic.
type DRM struct {
	mu     sync.Mutex
	closed bool
	err    error

	dev   *drm.Device
	conn  *drm.Connector
	crtc  *drm.CRTC
	plane *drm.Plane
	mode  drm.Mode

	bufs    []*scanoutBuf
	scanout int // index currently being scanned out
	queued  int // index of an in-flight page flip, -1 when none
	flipSeq uint64

	w, h int

	epfd   int
	inputs []*inputDevice
	in     Input

	logger func(format string, args ...any)

	// firstFlip records whether a page flip has completed yet, so the log says
	// something once instead of on every frame.
	firstFlip bool

	// pointer/keyboard state accumulated between SYN_REPORT events
	accX, accY       float64
	scrollX, scrollY float64
	absX, absY       float64
	absDirty         bool
	held             map[uint16]bool
}

// scanoutBuf is one dumb buffer plus the KMS framebuffer object that wraps it.
type scanoutBuf struct {
	dumb *drm.Dumb
	fb   uint32
}

// OpenDRM brings up a KMS connector and returns a ready-to-present backend.
// It takes DRM master, which fails when another compositor already owns the
// device: the error explains that rather than leaving a bare EACCES.
func OpenDRM(in Input, cfg DRMConfig) (*DRM, error) {
	cards, err := cardNodes(cfg.Device)
	if err != nil {
		return nil, err
	}
	var tried []string
	var lastErr error
	for _, path := range cards {
		b, err := openDRMCard(in, cfg, path)
		if err == nil {
			return b, nil
		}
		var fatal fatalError
		if errors.As(err, &fatal) {
			return nil, fatal.err
		}
		tried = append(tried, path)
		lastErr = err
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("drm: no card node tried")
	}
	return nil, fmt.Errorf("drm: no usable card in %s: %w", strings.Join(tried, ", "), lastErr)
}

// fatalError marks a failure that should stop card autodetection: another card
// node cannot do better (for example the requested connector exists but is
// disconnected).
type fatalError struct{ err error }

func (e fatalError) Error() string { return e.err.Error() }
func (e fatalError) Unwrap() error { return e.err }

// fatal wraps err so OpenDRM stops trying further card nodes.
func fatal(err error) error { return fatalError{err} }

func openDRMCard(in Input, cfg DRMConfig, path string) (*DRM, error) {
	dev, err := drm.Open(path)
	if err != nil {
		return nil, err
	}
	// b is created once the connector, CRTC and plane are known; from then on
	// the deferred cleanup tears the whole pipeline down on any error, so a
	// half-built output never leaks framebuffers or stays DRM master.
	var b *DRM
	ok := false
	defer func() {
		if ok {
			return
		}
		if b != nil {
			b.Close()
			return
		}
		_ = dev.Close()
	}()

	// Atomic modesetting is opt-in per file descriptor. Without it the kernel
	// reports CRTC MODE_ID/ACTIVE and the plane FB_ID/CRTC_ID/SRC_* properties
	// as absent, which surfaces as "object N has no MODE_ID property" during
	// the mode set. Universal planes is what makes every plane enumerable.
	if err := dev.EnableAtomic(); err != nil {
		return nil, fmt.Errorf("drm: %s does not support atomic modesetting: %w", path, err)
	}
	if !dev.DumbBufferSupported() {
		return nil, fmt.Errorf("drm: %s does not support dumb buffers", path)
	}
	res, err := dev.Resources()
	if err != nil {
		return nil, err
	}
	if len(res.CRTCs) == 0 {
		return nil, fmt.Errorf("drm: %s exposes no CRTCs", path)
	}

	conn, err := findConnector(dev, res, cfg.Connector)
	if err != nil {
		return nil, err
	}
	mode, ok := drm.SelectMode(conn.Modes, cfg.Width, cfg.Height, cfg.RefreshMHz)
	if !ok {
		return nil, fatal(fmt.Errorf("drm: %s %s: no mode matches %s (%d modes offered)",
			path, conn.Name(), requestedMode(cfg), len(conn.Modes)))
	}
	if mode.Width() <= 0 || mode.Height() <= 0 {
		return nil, fatal(fmt.Errorf("drm: %s %s: selected an empty mode", path, conn.Name()))
	}

	crtcID, err := selectCRTC(dev, res, conn)
	if err != nil {
		return nil, fatal(err)
	}
	plane, err := selectPlane(dev, res, crtcID)
	if err != nil {
		return nil, fatal(err)
	}
	crtc, err := dev.CRTC(crtcID)
	if err != nil {
		return nil, fatal(err)
	}

	b = &DRM{
		dev: dev, conn: conn, crtc: crtc, plane: plane, mode: mode,
		w: mode.Width(), h: mode.Height(),
		in: in, queued: -1, epfd: -1, logger: cfg.Logf,
	}

	if err := b.allocateBuffers(cfg.TripleBuffering); err != nil {
		return nil, err
	}
	// Master is only needed for the mode set, but taking it now means the
	// atomic commit below cannot be raced by another client.
	if err := dev.SetMaster(); err != nil {
		if drm.IsNotMaster(err) {
			return nil, fatal(fmt.Errorf("drm: %s is already mastered by another process "+
				"(Lumine is probably running inside a live session; DRM needs a free VT)", path))
		}
		return nil, fatal(err)
	}
	if err := b.modeset(); err != nil {
		return nil, fatal(err)
	}
	if err := dev.SetNonblock(true); err != nil {
		return nil, err
	}
	if err := b.setupPolling(); err != nil {
		return nil, err
	}
	ok = true
	return b, nil
}

// allocateBuffers carves out the scanout ring: two dumb buffers, or three when
// triple buffering is requested.
func (b *DRM) allocateBuffers(triple bool) error {
	count := 2
	if triple {
		count = 3
	}
	for i := 0; i < count; i++ {
		dumb, err := b.dev.CreateDumb(uint32(b.w), uint32(b.h), 32)
		if err != nil {
			return err
		}
		fb, err := b.dev.AddFBFor(dumb, uint32(b.w), uint32(b.h), 32, 24)
		if err != nil {
			_ = dumb.Destroy()
			return err
		}
		b.bufs = append(b.bufs, &scanoutBuf{dumb: dumb, fb: fb})
	}
	return nil
}

// modeset programs the connector/CRTC/plane with the chosen mode using one
// atomic commit. A TEST_ONLY commit runs first so a rejected mode fails
// cleanly instead of taking the display down.
func (b *DRM) modeset() error {
	blob, err := b.dev.CreatePropertyBlob(b.mode.ModeBlobBytes())
	if err != nil {
		return fmt.Errorf("drm: mode blob: %w", err)
	}
	atom := drm.NewAtom()
	sets := []struct {
		obj   uint32
		props map[string]uint32
		name  string
		value uint64
	}{
		{b.crtc.ID, b.crtc.Props, "MODE_ID", uint64(blob)},
		{b.crtc.ID, b.crtc.Props, "ACTIVE", 1},
		{b.conn.ID, b.conn.Props, "CRTC_ID", uint64(b.crtc.ID)},
		{b.plane.ID, b.plane.Props, "CRTC_ID", uint64(b.crtc.ID)},
		{b.plane.ID, b.plane.Props, "FB_ID", uint64(b.bufs[0].fb)},
		{b.plane.ID, b.plane.Props, "SRC_X", 0},
		{b.plane.ID, b.plane.Props, "SRC_Y", 0},
		{b.plane.ID, b.plane.Props, "SRC_W", uint64(b.w) << 16},
		{b.plane.ID, b.plane.Props, "SRC_H", uint64(b.h) << 16},
		{b.plane.ID, b.plane.Props, "CRTC_X", 0},
		{b.plane.ID, b.plane.Props, "CRTC_Y", 0},
		{b.plane.ID, b.plane.Props, "CRTC_W", uint64(b.w)},
		{b.plane.ID, b.plane.Props, "CRTC_H", uint64(b.h)},
	}
	for _, s := range sets {
		id, ok := s.props[s.name]
		if !ok {
			return fmt.Errorf("drm: object %d has no %q property (atomic modesetting unsupported by this driver?)",
				s.obj, s.name)
		}
		atom.Set(s.obj, id, s.value)
	}
	if err := b.dev.TestCommit(atom); err != nil {
		return fmt.Errorf("drm: mode test failed for %s %dx%d@%dmHz: %w",
			b.conn.Name(), b.w, b.h, b.mode.RefreshMHz(), err)
	}
	if err := b.dev.Commit(atom, drm.AtomicAllowModeset, 0); err != nil {
		return fmt.Errorf("drm: modeset failed: %w", err)
	}
	b.scanout, b.queued = 0, -1
	b.logf("%s: modeset %dx%d@%dmHz on %s (crtc %d, plane %d)",
		b.dev.Path(), b.w, b.h, b.mode.RefreshMHz(), b.conn.Name(), b.crtc.ID, b.plane.ID)
	return nil
}

// Present copies the finished frame into a free dumb buffer and schedules a
// page flip. When a flip is still in flight the frame is dropped: at 60 Hz the
// completion event has almost always been drained by then.
func (b *DRM) Present(fb *render.Framebuffer) error {
	if fb == nil {
		return nil
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed || b.err != nil || len(b.bufs) == 0 {
		return nil
	}
	if fb.Width != b.w || fb.Height != b.h {
		return nil
	}
	// Reap a completion that landed since the last poll. The event loop does
	// this too, but doing it here keeps the ring from wedging for good if a
	// wakeup is ever missed.
	b.drainFlips()
	if b.queued >= 0 {
		return nil
	}
	idx := b.nextBuffer()
	if idx < 0 {
		return nil
	}
	copyScanout(b.bufs[idx].dumb, fb)
	b.flipSeq++
	b.queued = idx
	if err := b.dev.PageFlip(b.crtc.ID, b.bufs[idx].fb, b.flipSeq); err != nil {
		b.queued = -1
		return err
	}
	return nil
}

// nextBuffer returns a scanout buffer that is neither on screen nor in flight.
func (b *DRM) nextBuffer() int {
	for i := range b.bufs {
		if i != b.scanout && i != b.queued {
			return i
		}
	}
	return -1
}

// load performs the first buffer upload before the mode set.
func copyScanout(d *drm.Dumb, fb *render.Framebuffer) {
	row := fb.Width * 4
	if row > int(d.Pitch) {
		row = int(d.Pitch)
	}
	for y := 0; y < fb.Height; y++ {
		dst := int(d.Pitch) * y
		if dst+row > len(d.Map) {
			return
		}
		src := y * fb.Stride
		if src+row > len(fb.Pix) {
			return
		}
		copy(d.Map[dst:dst+row], fb.Pix[src:src+row])
	}
}

// Drain consumes every pending DRM and input event without blocking. It is
// called from the main poll loop when FD (the epoll descriptor) is readable.
func (b *DRM) Drain() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.closed || b.epfd < 0 {
		return nil
	}
	var evs [16]unix.EpollEvent
	for {
		n, err := unix.EpollWait(b.epfd, evs[:], 0)
		if err != nil {
			if err == syscall.EINTR {
				continue
			}
			return err
		}
		if n <= 0 {
			return nil
		}
		for i := 0; i < n; i++ {
			fd := int(evs[i].Fd)
			if fd == b.dev.FD() {
				b.drainFlips()
				continue
			}
			b.drainInput(fd)
		}
	}
}

// drainFlips reads queued page-flip completions, recycling the buffer that
// just left the screen.
func (b *DRM) drainFlips() {
	for {
		ev, err := b.dev.ReadEvent()
		if err != nil {
			if !errors.Is(err, unix.EAGAIN) {
				b.fail(err)
			}
			return
		}
		if ev.Type == drm.EventFlipComplete && b.queued >= 0 && ev.UserData == b.flipSeq {
			b.scanout, b.queued = b.queued, -1
			if !b.firstFlip {
				b.firstFlip = true
				b.logf("%s: first page flip complete", b.dev.Path())
			}
		}
	}
}

// fail records the first fatal backend error so Closed reports it.
func (b *DRM) fail(err error) {
	if b.err == nil {
		b.err = err
	}
}

// logf emits a progress line when the caller supplied a logger.
func (b *DRM) logf(format string, args ...any) {
	if b.logger == nil {
		return
	}
	b.logger("drm: "+format, args...)
}

// Size returns the active mode's resolution.
func (b *DRM) Size() (int, int) { return b.w, b.h }

// FD returns the epoll descriptor covering the DRM node and every input device.
func (b *DRM) FD() int { return b.epfd }

// Closed reports whether the backend was closed or hit a fatal error.
func (b *DRM) Closed() bool {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.closed || b.err != nil
}

// OutputName is the KMS connector name, e.g. "eDP-1".
func (b *DRM) OutputName() string { return b.conn.Name() }

// PhysicalSizeMM returns the connector's physical size from its EDID.
func (b *DRM) PhysicalSizeMM() (int32, int32) {
	return int32(b.conn.MMWidth), int32(b.conn.MMHeight)
}

// RefreshMHz returns the active mode's refresh rate in millihertz.
func (b *DRM) RefreshMHz() int32 { return b.mode.RefreshMHz() }

// Device returns the card node path, useful for logging.
func (b *DRM) Device() string { return b.dev.Path() }

// Close tears the output down: it disables the CRTC, removes every framebuffer
// and dumb buffer, releases DRM master and closes all fds.
func (b *DRM) Close() {
	b.mu.Lock()
	if b.closed {
		b.mu.Unlock()
		return
	}
	b.closed = true
	dev, bufs := b.dev, b.bufs
	inputs, epfd := b.inputs, b.epfd
	b.bufs, b.inputs, b.epfd = nil, nil, -1
	b.mu.Unlock()

	for _, d := range inputs {
		_ = d.dev.Close()
	}
	if epfd >= 0 {
		_ = unix.Close(epfd)
	}
	if dev == nil {
		return
	}
	cleanupAtom := drm.NewAtom()
	for _, s := range []struct {
		obj   uint32
		props map[string]uint32
		name  string
	}{
		{b.crtc.ID, b.crtc.Props, "MODE_ID"},
		{b.crtc.ID, b.crtc.Props, "ACTIVE"},
		{b.conn.ID, b.conn.Props, "CRTC_ID"},
		{b.plane.ID, b.plane.Props, "FB_ID"},
		{b.plane.ID, b.plane.Props, "CRTC_ID"},
	} {
		if id, ok := s.props[s.name]; ok {
			cleanupAtom.Set(s.obj, id, 0)
		}
	}
	if cleanupAtom.Len() > 0 {
		_ = dev.Commit(cleanupAtom, drm.AtomicAllowModeset, 0)
	}
	for _, buf := range bufs {
		_ = dev.RmFB(buf.fb)
		_ = buf.dumb.Destroy()
	}
	_ = dev.Close()
}

// setupPolling creates the epoll set and opens every keyboard/pointer device.
func (b *DRM) setupPolling() error {
	epfd, err := unix.EpollCreate1(unix.EPOLL_CLOEXEC)
	if err != nil {
		return fmt.Errorf("drm: epoll_create1: %w", err)
	}
	b.epfd = epfd
	if err := b.watch(b.dev.FD()); err != nil {
		return err
	}
	for _, path := range evdev.List() {
		d, err := evdev.Open(path)
		if err != nil {
			continue
		}
		if !d.IsPointer() && !d.IsKeyboard() && !d.IsButton() {
			_ = d.Close()
			continue
		}
		if err := b.watch(d.FD()); err != nil {
			_ = d.Close()
			continue
		}
		b.inputs = append(b.inputs, newInputDevice(d))
	}
	return nil
}

func (b *DRM) watch(fd int) error {
	ev := unix.EpollEvent{Events: unix.EPOLLIN, Fd: int32(fd)}
	if err := unix.EpollCtl(b.epfd, unix.EPOLL_CTL_ADD, fd, &ev); err != nil {
		return fmt.Errorf("drm: epoll add fd %d: %w", fd, err)
	}
	return nil
}

// ---- device and object selection ----

func cardNodes(explicit string) ([]string, error) {
	if explicit != "" {
		return []string{explicit}, nil
	}
	if env := os.Getenv("LUMINE_DRM_DEVICE"); env != "" {
		return []string{env}, nil
	}
	if env := os.Getenv("WLR_DRM_DEVICES"); env != "" {
		if first := strings.Split(env, ":")[0]; first != "" {
			return []string{first}, nil
		}
	}
	nodes, _ := filepath.Glob("/dev/dri/card*")
	sort.Slice(nodes, func(i, j int) bool { return cardNumber(nodes[i]) < cardNumber(nodes[j]) })
	if len(nodes) == 0 {
		return nil, fmt.Errorf("drm: no /dev/dri/card* nodes (run Lumine from a VT, not inside a Wayland session)")
	}
	return nodes, nil
}

func cardNumber(path string) int {
	n, err := strconv.Atoi(strings.TrimPrefix(filepath.Base(path), "card"))
	if err != nil {
		return 1 << 30
	}
	return n
}

// findConnector picks a connected connector, honouring the requested name.
func findConnector(dev *drm.Device, res *drm.Resources, want string) (*drm.Connector, error) {
	var firstConnected *drm.Connector
	for _, id := range res.Connectors {
		c, err := dev.Connector(id)
		if err != nil {
			continue
		}
		if want != "" && !strings.EqualFold(c.Name(), want) {
			continue
		}
		if !c.Connected() || len(c.Modes) == 0 {
			if want != "" {
				return nil, fatal(fmt.Errorf("drm: connector %s is %s", c.Name(), connectionWord(c)))
			}
			continue
		}
		if want != "" {
			return c, nil
		}
		if firstConnected == nil {
			firstConnected = c
		}
	}
	if want != "" {
		return nil, fmt.Errorf("drm: connector %s is not present on this device", want)
	}
	if firstConnected == nil {
		return nil, fmt.Errorf("drm: no connected connector with modes")
	}
	return firstConnected, nil
}

func connectionWord(c *drm.Connector) string {
	if len(c.Modes) > 0 {
		return "connected but reports no modes"
	}
	return "not connected"
}

// selectCRTC picks the CRTC that should drive the connector, preferring the one
// the kernel already bound so a hand-off does not reshuffle the pipeline.
func selectCRTC(dev *drm.Device, res *drm.Resources, conn *drm.Connector) (uint32, error) {
	if conn.EncoderID != 0 {
		if enc, err := dev.Encoder(conn.EncoderID); err == nil && enc.CRTCID != 0 {
			return enc.CRTCID, nil
		}
	}
	for _, eid := range conn.Encoders {
		enc, err := dev.Encoder(eid)
		if err != nil {
			continue
		}
		if enc.CRTCID != 0 {
			return enc.CRTCID, nil
		}
	}
	for _, eid := range conn.Encoders {
		enc, err := dev.Encoder(eid)
		if err != nil || enc.PossibleCRTCs == 0 {
			continue
		}
		for _, id := range res.CRTCs {
			if crtcMaskHas(res, id, enc.PossibleCRTCs) {
				return id, nil
			}
		}
	}
	return 0, fmt.Errorf("drm: connector %s has no usable CRTC", conn.Name())
}

// selectPlane picks the primary plane that can drive crtcID with XRGB8888.
func selectPlane(dev *drm.Device, res *drm.Resources, crtcID uint32) (*drm.Plane, error) {
	ids, err := dev.PlaneResources()
	if err != nil {
		return nil, err
	}
	var fallback *drm.Plane
	for _, id := range ids {
		p, err := dev.Plane(id)
		if err != nil {
			continue
		}
		if !crtcMaskHas(res, crtcID, p.PossibleCRTCs) {
			continue
		}
		if !p.SupportsFormat(drm.FourccXRGB8888) {
			continue
		}
		if planeIsPrimary(p) {
			return p, nil
		}
		if fallback == nil {
			fallback = p
		}
	}
	if fallback != nil {
		return fallback, nil
	}
	return nil, fmt.Errorf("drm: no XRGB8888 plane can drive CRTC %d", crtcID)
}

// planeTypePrimary is DRM_PLANE_TYPE_PRIMARY.
const planeTypePrimary uint64 = 1

func planeIsPrimary(p *drm.Plane) bool {
	if _, ok := p.Prop("type"); !ok {
		return false
	}
	return p.PropValues["type"] == planeTypePrimary
}

// crtcMaskHas reports whether an encoder's PossibleCRTCs bitmask covers the
// CRTC with the given id. The mask is indexed by position in the resource CRTC
// list, not by CRTC id.
func crtcMaskHas(res *drm.Resources, id, mask uint32) bool {
	for i, c := range res.CRTCs {
		if c == id && i < 32 {
			return mask&(1<<uint(i)) != 0
		}
	}
	return false
}

func requestedMode(cfg DRMConfig) string {
	switch {
	case cfg.Width > 0 && cfg.Height > 0 && cfg.RefreshMHz > 0:
		return fmt.Sprintf("%dx%d@%dmHz", cfg.Width, cfg.Height, cfg.RefreshMHz)
	case cfg.Width > 0 && cfg.Height > 0:
		return fmt.Sprintf("%dx%d", cfg.Width, cfg.Height)
	default:
		return "the connector's modes"
	}
}
