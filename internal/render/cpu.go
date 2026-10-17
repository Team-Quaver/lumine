package render

// CPU is the software renderer. It is the fallback when no Vulkan device is
// usable, the headless/test renderer, and the reference implementation the
// Vulkan path is checked against.
type CPU struct {
	Theme Theme
	cs    colorState
}

func NewCPU(theme Theme) *CPU { return &CPU{Theme: theme} }

func (r *CPU) Name() string { return "cpu" }

func (r *CPU) SetColorSpace(enabled bool, tf, primaries string, maxLuma uint32) {
	r.cs = colorState{Enabled: enabled, TF: tf, Primaries: primaries, MaxLuma: maxLuma}
}

func (r *CPU) Draw(fb *Framebuffer, items []DrawItem) error {
	fb.Fill(r.cs.background(r.Theme))
	for _, it := range items {
		rect := it.IntRect()
		if it.Frame {
			r.drawTitleBar(fb, rect, it.Active)
		}
		alpha := uint16(0xffff)
		if it.Opacity > 0 && it.Opacity < 1 {
			alpha = uint16(it.Opacity * 0xffff)
		}
		fb.ScaleFromImage(it.Image, rect.X, rect.Y, rect.W, rect.H, alpha)
		if it.Frame {
			r.drawOutline(fb, rect, it.Active)
		}
	}
	applyColorTransform(fb, r.cs)
	return nil
}

const (
	titleBarHeight = 28
	borderWidth    = 1
)

// drawTitleBar paints the SSD title bar and close affordance. The bar is drawn
// just above the content rect; tile mode reserves exactly this much space
// (layout.Tree.DecorH) above a decorated window, so the bar never overlaps the
// window's own pixels.
func (r *CPU) drawTitleBar(fb *Framebuffer, rect Rect, active bool) {
	bar := r.Theme.Decoration
	if active {
		bar = r.Theme.DecorationActive
	}
	fb.FillRect(rect.X, rect.Y-titleBarHeight, rect.W, titleBarHeight, bar)
	fb.FillRect(rect.X+rect.W-20, rect.Y-titleBarHeight+8, 12, 12, 0xffe06060)
}

// drawOutline paints the one-pixel border on top of the content, so CSD
// clients (which leave Frame false) and SSD clients look distinct.
func (r *CPU) drawOutline(fb *Framebuffer, rect Rect, active bool) {
	border := r.Theme.Border
	if active {
		border = r.Theme.BorderActive
	}
	fb.FillRect(rect.X, rect.Y, rect.W, borderWidth, border)
	fb.FillRect(rect.X, rect.Y+rect.H-borderWidth, rect.W, borderWidth, border)
	fb.FillRect(rect.X, rect.Y, borderWidth, rect.H, border)
	fb.FillRect(rect.X+rect.W-borderWidth, rect.Y, borderWidth, rect.H, border)
}
