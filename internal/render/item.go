package render

import (
	"github.com/Team-Quaver/lumine/internal/shm"
)

// DrawItem is one textured quad in output coordinates. The compositor builds
// the list back-to-front; renderers only blit.
type DrawItem struct {
	Image *shm.Image

	X, Y, W, H float64

	// Frame asks the renderer to paint server-side decorations around the
	// quad (title bar + border). Clients that draw their own decorations
	// (CSD) simply leave it false, which is how CSD and SSD coexist.
	Frame   bool
	Title   string
	AppID   string
	Active  bool
	Opacity float64 // 0..1, 1 = opaque
}

// Renderer is implemented by both the Vulkan and the software renderer. Every
// implementation draws into the framebuffer it is given, so the output path
// (nested shm buffer, DRM dumb buffer, PNG dump) stays independent of the
// renderer.
type Renderer interface {
	Name() string
	Draw(fb *Framebuffer, items []DrawItem) error
	SetColorSpace(enabled bool, tf, primaries string, maxLuma uint32)
}

// Rect is a clipped integer rectangle.
type Rect struct{ X, Y, W, H int }

// IntRect converts a draw item's geometry to integer pixels.
func (it DrawItem) IntRect() Rect {
	return Rect{X: int(it.X + 0.5), Y: int(it.Y + 0.5), W: int(it.W + 0.5), H: int(it.H + 0.5)}
}

// Theme is the compositor's decoration palette.
type Theme struct {
	Background       uint32
	Decoration       uint32
	DecorationActive uint32
	Border           uint32
	BorderActive     uint32
	Cursor           uint32
}

// DefaultTheme is a dark palette used when the configuration does not set one.
func DefaultTheme() Theme {
	return Theme{
		Background:       0xff101014,
		Decoration:       0xff262630,
		DecorationActive: 0xff3a3a5c,
		Border:           0xff33333d,
		BorderActive:     0xff8f8fd0,
		Cursor:           0xfff2f2f2,
	}
}

// colorState is the negotiated color management state. Renderers use it to
// pick a transfer function / primaries when they can, and to expose the state
// to clients through wp_color_manager.
type colorState struct {
	Enabled   bool
	TF        string
	Primaries string
	MaxLuma   uint32
}
