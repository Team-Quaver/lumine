package backend

import "github.com/Team-Quaver/lumine/internal/render"

// Output is one presentation target the entry point drives. Two backends
// implement it today:
//
//   - *Backend, the nested Wayland client that renders into a host compositor's
//     window (a wl_shm buffer per frame); and
//   - *DRM, the bare-metal kernel mode-setting backend that scans out DRM dumb
//     buffers through the Vulkan-rendered frame.
//
// The main loop only needs these six methods, so it multiplexes either backend
// without knowing which is which.
type Output interface {
	// Size returns the current presentation size in pixels.
	Size() (int, int)
	// FD returns the file descriptor to poll for backend events. For DRM this
	// is an epoll fd covering the card node and every input device.
	FD() int
	// Present copies a finished frame into the backend's scanout target and
	// schedules it for display.
	Present(fb *render.Framebuffer) error
	// Drain consumes pending backend events after FD becomes readable. It must
	// never block: callers invoke it from a poll loop.
	Drain() error
	// Closed reports that the backend can no longer present.
	Closed() bool
	// Close releases the backend and every resource it owns.
	Close()
}

// Describer is implemented by backends that know the physical output behind
// them. The nested and headless backends have no fixed connector, so they do
// not implement it; the DRM backend reports the KMS connector instead.
type Describer interface {
	// OutputName is the connector name, e.g. "eDP-1".
	OutputName() string
	// PhysicalSizeMM is the connector's physical size in millimetres, or
	// (0, 0) when the EDID did not report one.
	PhysicalSizeMM() (int32, int32)
	// RefreshMHz is the active mode's refresh rate in millihertz.
	RefreshMHz() int32
}
