package render

import "math"

// Color management.
//
// The compositor negotiates wp_color_manager_v1 state per output and stores it
// on the renderer with SetColorSpace. MVP implements the output side of that
// pipeline: the composited frame is decoded from sRGB, scaled to the output's
// peak luminance and re-encoded with the negotiated transfer function. That is
// a real (if simple) CMS stage shared by both renderers, so pixels stay
// identical no matter which one draws them. Per-surface image descriptions,
// ICC profiles and a 3D LUT are deliberately left for later.

// referenceWhite is the luminance (nits) that a linear signal value of 1.0
// represents in the compositor's working space.
const referenceWhite = 100.0

// background is the clear colour for the current color state. With color
// management disabled the compositor falls back to a neutral dark backdrop.
func (cs colorState) background(theme Theme) uint32 {
	if cs.Enabled {
		return theme.Background
	}
	return 0xff0b0b0e
}

// applyColorTransform rewrites the composited framebuffer in place. It is a
// no-op for sRGB output (the working space), so the common path costs nothing.
func applyColorTransform(fb *Framebuffer, cs colorState) {
	if !cs.Enabled || fb == nil {
		return
	}
	tf := cs.TF
	if tf == "" || tf == "srgb" {
		return
	}
	peak := float64(cs.MaxLuma)
	if peak < referenceWhite {
		peak = referenceWhite
	}
	// Work on whole pixels: B, G, R, X in little-endian XRGB8888.
	for i := 0; i+3 < len(fb.Pix); i += 4 {
		lr := srgbToLinear(float64(fb.Pix[i+2]) / 255)
		lg := srgbToLinear(float64(fb.Pix[i+1]) / 255)
		lb := srgbToLinear(float64(fb.Pix[i]) / 255)
		switch tf {
		case "pq", "st2084", "smpte2084":
			fb.Pix[i+2] = to8(pqEncode(lr * peak))
			fb.Pix[i+1] = to8(pqEncode(lg * peak))
			fb.Pix[i] = to8(pqEncode(lb * peak))
		case "hlg", "arib-std-b67":
			fb.Pix[i+2] = to8(hlgEncode(lr))
			fb.Pix[i+1] = to8(hlgEncode(lg))
			fb.Pix[i] = to8(hlgEncode(lb))
		case "linear":
			fb.Pix[i+2], fb.Pix[i+1], fb.Pix[i] = to8(lr), to8(lg), to8(lb)
		default:
			return
		}
	}
}

func to8(v float64) byte {
	v = v*255 + 0.5
	if v <= 0 {
		return 0
	}
	if v >= 255 {
		return 255
	}
	return byte(v)
}

// srgbToLinear is the sRGB EOTF (encoded -> linear).
func srgbToLinear(v float64) float64 {
	if v <= 0.04045 {
		return v / 12.92
	}
	return math.Pow((v+0.055)/1.055, 2.4)
}

// pqEncode is the SMPTE ST 2084 (PQ) EOTF inverse: absolute luminance in nits
// (0..10000) to code value 0..1.
func pqEncode(nits float64) float64 {
	const (
		m1 = 0.1593017578125
		m2 = 78.84375
		c1 = 0.8359375
		c2 = 18.8515625
		c3 = 18.6875
	)
	n := nits / 10000.0
	if n <= 0 {
		return 0
	}
	if n > 1 {
		n = 1
	}
	np := math.Pow(n, m1)
	return math.Pow((c1+c2*np)/(1+c3*np), m2)
}

// hlgEncode is the ARIB STD-B67 (HLG) OETF for scene linear 0..1.
func hlgEncode(l float64) float64 {
	const (
		a = 0.17883277
		b = 0.28466892
		c = 0.55991073
	)
	if l <= 0 {
		return 0
	}
	if l <= 1.0/12.0 {
		return math.Sqrt(3 * l)
	}
	return a*math.Log(12*l-b) + c
}
