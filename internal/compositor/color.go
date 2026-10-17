package compositor

import (
	"fmt"
	"hash/fnv"

	"github.com/Team-Quaver/lumine/internal/config"
	"github.com/Team-Quaver/lumine/internal/wl"
	"golang.org/x/sys/unix"
)

// Color management (wp_color_manager_v1).
//
// Lumine implements the output side of the colour pipeline in the renderer
// (render.SetColorSpace) and serves the protocol side here: clients can create
// parametric / scRGB / BT.2100 image descriptions, assign one to a surface and
// query the preferred description for an output. The description is stored on
// the surface so compositing decisions (and future ICC/LUT work) can see it.
//
// Scope note: pixel-accurate per-surface conversion (ICC profiles, 3D LUTs) is
// not implemented yet; the named transfer function / primaries are tracked as
// metadata and the output transform is applied at present time.

// imageDesc is the compositor's model of a wp_image_description_v1. Only the
// named transfer function / primaries plus the luminance range are kept; a
// description that needs features Lumine does not support is failed at
// creation time instead of being silently accepted.
type imageDesc struct {
	tf        string // "srgb" | "pq" | "hlg" | "linear"
	primaries string // "srgb" | "bt2020" | "dci-p3"
	minLum    uint32
	maxLum    uint32
	refLum    uint32
}

// tfName maps a wp_color_manager_v1 transfer_function enum to the renderer's
// label. An empty result means "unsupported".
func tfName(tf uint32) string {
	switch tf {
	case wl.WpColorManagerV1TransferFunctionSrgb, wl.WpColorManagerV1TransferFunctionExtSrgb:
		return "srgb"
	case wl.WpColorManagerV1TransferFunctionSt2084Pq:
		return "pq"
	case wl.WpColorManagerV1TransferFunctionHlg:
		return "hlg"
	case wl.WpColorManagerV1TransferFunctionExtLinear, wl.WpColorManagerV1TransferFunctionSt428:
		return "linear"
	default:
		return ""
	}
}

// primariesName maps a wp_color_manager_v1 primaries enum to the renderer's
// label. An empty result means "unsupported".
func primariesName(p uint32) string {
	switch p {
	case wl.WpColorManagerV1PrimariesSrgb:
		return "srgb"
	case wl.WpColorManagerV1PrimariesBt2020:
		return "bt2020"
	case wl.WpColorManagerV1PrimariesDciP3:
		return "dci-p3"
	default:
		return ""
	}
}

// identity returns a stable non-zero identity for a description. Descriptions
// with identical content share an identity, which is what the protocol
// expects.
func (d imageDesc) identity() uint64 {
	h := fnv.New64a()
	fmt.Fprintf(h, "%s|%s|%d|%d|%d", d.tf, d.primaries, d.minLum, d.maxLum, d.refLum)
	v := h.Sum64()
	if v == 0 {
		v = 1
	}
	return v
}

// preferredDesc is the description Lumine advertises for its outputs.
func preferredDesc(cfg *config.Config) imageDesc {
	d := imageDesc{tf: "srgb", primaries: "srgb", refLum: 100}
	if cfg != nil {
		if n := tfNameFromConfig(cfg.Color.TFName); n != "" {
			d.tf = n
		}
		if n := primariesFromConfig(cfg.Color.Primaries); n != "" {
			d.primaries = n
		}
		if cfg.Color.MaxLuma > 0 {
			d.maxLum = cfg.Color.MaxLuma
		}
	}
	if d.maxLum < d.refLum {
		d.maxLum = d.refLum
	}
	return d
}

func tfNameFromConfig(s string) string {
	switch s {
	case "srgb", "pq", "hlg", "linear":
		return s
	case "st2084", "smpte2084":
		return "pq"
	case "arib-std-b67":
		return "hlg"
	default:
		return ""
	}
}

func primariesFromConfig(s string) string {
	switch s {
	case "srgb", "bt2020", "dci-p3":
		return s
	default:
		return ""
	}
}

// emitDesc wires a freshly allocated wp_image_description_v1 to its content
// and marks it ready so the client stops waiting.
func emitDesc(id *wl.WpImageDescriptionV1, d imageDesc) {
	id.Impl = &descHandler{desc: d}
	if id.Version >= 2 {
		v := d.identity()
		id.Ready2(uint32(v>>32), uint32(v))
		return
	}
	id.Ready(uint32(d.identity()))
}

type descHandler struct{ desc imageDesc }

func (h *descHandler) Destroy(o *wl.WpImageDescriptionV1) {}
func (h *descHandler) GetInformation(o *wl.WpImageDescriptionV1, info *wl.WpImageDescriptionInfoV1) {
	if h.desc.tf != "" {
		if tf, ok := tfEnum(h.desc.tf); ok {
			info.TfNamed(tf)
		}
	}
	if h.desc.primaries != "" {
		if p, ok := primariesEnum(h.desc.primaries); ok {
			info.PrimariesNamed(p)
		}
	}
	info.Luminances(h.desc.minLum, h.desc.maxLum, h.desc.refLum)
	info.Done()
	info.Object.Destroy() // done is a destructor event
}

func tfEnum(name string) (uint32, bool) {
	switch name {
	case "srgb":
		return wl.WpColorManagerV1TransferFunctionSrgb, true
	case "pq":
		return wl.WpColorManagerV1TransferFunctionSt2084Pq, true
	case "hlg":
		return wl.WpColorManagerV1TransferFunctionHlg, true
	case "linear":
		return wl.WpColorManagerV1TransferFunctionExtLinear, true
	default:
		return 0, false
	}
}

func primariesEnum(name string) (uint32, bool) {
	switch name {
	case "srgb":
		return wl.WpColorManagerV1PrimariesSrgb, true
	case "bt2020":
		return wl.WpColorManagerV1PrimariesBt2020, true
	case "dci-p3":
		return wl.WpColorManagerV1PrimariesDciP3, true
	default:
		return 0, false
	}
}

// ---- wp_color_manager_v1 ----

type colorManagerHandler struct{ c *Compositor }

func (h *colorManagerHandler) Destroy(o *wl.WpColorManagerV1) {}

func (h *colorManagerHandler) GetOutput(o *wl.WpColorManagerV1, id *wl.WpColorManagementOutputV1, output *wl.WlOutput) {
	id.Impl = &colorOutputHandler{c: h.c, cfg: h.c.Store.Config()}
	_ = output
}

func (h *colorManagerHandler) GetSurface(o *wl.WpColorManagerV1, id *wl.WpColorManagementSurfaceV1, surface *wl.WlSurface) {
	id.Impl = &colorSurfaceHandler{c: h.c, surface: surface}
}

func (h *colorManagerHandler) GetSurfaceFeedback(o *wl.WpColorManagerV1, id *wl.WpColorManagementSurfaceFeedbackV1, surface *wl.WlSurface) {
	id.Impl = &colorFeedbackHandler{c: h.c}
	_ = surface
}

func (h *colorManagerHandler) CreateIccCreator(o *wl.WpColorManagerV1, id *wl.WpImageDescriptionCreatorIccV1) {
	id.Impl = &iccCreatorHandler{}
}

func (h *colorManagerHandler) CreateParametricCreator(o *wl.WpColorManagerV1, id *wl.WpImageDescriptionCreatorParamsV1) {
	id.Impl = &paramCreatorHandler{}
}

func (h *colorManagerHandler) CreateWindowsScrgb(o *wl.WpColorManagerV1, id *wl.WpImageDescriptionV1) {
	emitDesc(id, imageDesc{tf: "linear", primaries: "srgb", refLum: 80, maxLum: 80})
}

func (h *colorManagerHandler) GetImageDescription(o *wl.WpColorManagerV1, id *wl.WpImageDescriptionV1, ref *wl.WpImageDescriptionReferenceV1) {
	// References are never handed out by Lumine, so this cannot be resolved.
	id.Failed(wl.WpImageDescriptionV1ErrorNoInformation, "image description references are not supported")
}

func (h *colorManagerHandler) CreateWindowsBt2100(o *wl.WpColorManagerV1, id *wl.WpImageDescriptionV1) {
	emitDesc(id, imageDesc{tf: "pq", primaries: "bt2020", refLum: 100, maxLum: 1000})
}

// paramCreatorHandler accumulates a parametric description and materialises it
// on create.
type paramCreatorHandler struct {
	tf          uint32
	hasTF       bool
	primaries   uint32
	hasPrim     bool
	minLum      uint32
	maxLum      uint32
	refLum      uint32
	hasLum      bool
	unsupported bool
}

func (h *paramCreatorHandler) SetTfNamed(o *wl.WpImageDescriptionCreatorParamsV1, tf uint32) {
	h.tf, h.hasTF = tf, true
}
func (h *paramCreatorHandler) SetTfPower(o *wl.WpImageDescriptionCreatorParamsV1, eexp uint32) {
	h.unsupported = true // set_tf_power is not advertised
}
func (h *paramCreatorHandler) SetPrimariesNamed(o *wl.WpImageDescriptionCreatorParamsV1, p uint32) {
	h.primaries, h.hasPrim = p, true
}
func (h *paramCreatorHandler) SetPrimaries(o *wl.WpImageDescriptionCreatorParamsV1, rX, rY, gX, gY, bX, bY, wX, wY int32) {
	h.unsupported = true // explicit chromaticities are not admitted
}
func (h *paramCreatorHandler) SetLuminances(o *wl.WpImageDescriptionCreatorParamsV1, minLum, maxLum, refLum uint32) {
	h.minLum, h.maxLum, h.refLum, h.hasLum = minLum, maxLum, refLum, true
}
func (h *paramCreatorHandler) SetMasteringDisplayPrimaries(o *wl.WpImageDescriptionCreatorParamsV1, rX, rY, gX, gY, bX, bY, wX, wY int32) {
}
func (h *paramCreatorHandler) SetMasteringLuminance(o *wl.WpImageDescriptionCreatorParamsV1, minLum, maxLum uint32) {
}
func (h *paramCreatorHandler) SetMaxCll(o *wl.WpImageDescriptionCreatorParamsV1, maxCll uint32)   {}
func (h *paramCreatorHandler) SetMaxFall(o *wl.WpImageDescriptionCreatorParamsV1, maxFall uint32) {}

func (h *paramCreatorHandler) Create(o *wl.WpImageDescriptionCreatorParamsV1, id *wl.WpImageDescriptionV1) {
	d, err := h.build()
	if err != nil {
		id.Failed(wl.WpImageDescriptionV1CauseUnsupported, err.Error())
		return
	}
	emitDesc(id, d)
}

// build materialises the accumulated state into an imageDesc, or reports why
// the description cannot be produced with the features Lumine advertises.
func (h *paramCreatorHandler) build() (imageDesc, error) {
	if h.unsupported {
		return imageDesc{}, fmt.Errorf("unsupported parametric image description feature")
	}
	tf := "srgb"
	if h.hasTF {
		tf = tfName(h.tf)
		if tf == "" {
			return imageDesc{}, fmt.Errorf("unsupported transfer function")
		}
	}
	prim := "srgb"
	if h.hasPrim {
		prim = primariesName(h.primaries)
		if prim == "" {
			return imageDesc{}, fmt.Errorf("unsupported primaries")
		}
	}
	d := imageDesc{tf: tf, primaries: prim, refLum: 100}
	if h.hasLum {
		d.minLum, d.maxLum, d.refLum = h.minLum, h.maxLum, h.refLum
	}
	if d.maxLum == 0 {
		d.maxLum = d.refLum
	}
	return d, nil
}

// iccCreatorHandler accepts the protocol calls but declines: ICC parsing is
// outside the MVP, and the feature is not advertised, so a conforming client
// never reaches here.
type iccCreatorHandler struct{}

func (h *iccCreatorHandler) SetIccFile(o *wl.WpImageDescriptionCreatorIccV1, iccProfile int, offset, length uint32) {
	if iccProfile >= 0 {
		unix.Close(iccProfile)
	}
}
func (h *iccCreatorHandler) Create(o *wl.WpImageDescriptionCreatorIccV1, id *wl.WpImageDescriptionV1) {
	id.Failed(wl.WpImageDescriptionV1CauseUnsupported, "ICC profiles are not supported")
}

// ---- wp_color_management_output_v1 ----

type colorOutputHandler struct {
	c   *Compositor
	cfg *config.Config
}

func (h *colorOutputHandler) Destroy(o *wl.WpColorManagementOutputV1) {}
func (h *colorOutputHandler) GetImageDescription(o *wl.WpColorManagementOutputV1, id *wl.WpImageDescriptionV1) {
	emitDesc(id, preferredDesc(h.cfg))
}

// ---- wp_color_management_surface_v1 ----

type colorSurfaceHandler struct {
	c       *Compositor
	surface *wl.WlSurface
}

func (h *colorSurfaceHandler) Destroy(o *wl.WpColorManagementSurfaceV1) {}
func (h *colorSurfaceHandler) SetImageDescription(o *wl.WpColorManagementSurfaceV1, desc *wl.WpImageDescriptionV1, renderIntent uint32) {
	h.c.mu.Lock()
	defer h.c.mu.Unlock()
	s := h.c.surfaces[h.surface]
	if s == nil {
		return
	}
	if dh, ok := desc.Impl.(*descHandler); ok {
		d := dh.desc
		s.srcColor = &d
	}
}
func (h *colorSurfaceHandler) UnsetImageDescription(o *wl.WpColorManagementSurfaceV1) {
	h.c.mu.Lock()
	defer h.c.mu.Unlock()
	if s := h.c.surfaces[h.surface]; s != nil {
		s.srcColor = nil
	}
}

// ---- wp_color_management_surface_feedback_v1 ----

type colorFeedbackHandler struct{ c *Compositor }

func (h *colorFeedbackHandler) Destroy(o *wl.WpColorManagementSurfaceFeedbackV1) {}
func (h *colorFeedbackHandler) GetPreferred(o *wl.WpColorManagementSurfaceFeedbackV1, id *wl.WpImageDescriptionV1) {
	emitDesc(id, preferredDesc(h.c.Store.Config()))
}
func (h *colorFeedbackHandler) GetPreferredParametric(o *wl.WpColorManagementSurfaceFeedbackV1, id *wl.WpImageDescriptionV1) {
	emitDesc(id, preferredDesc(h.c.Store.Config()))
}
