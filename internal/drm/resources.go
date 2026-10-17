package drm

import (
	"fmt"
	"runtime"
	"unsafe"
)

// Resources is the top-level object list from DRM_IOCTL_MODE_GETRESOURCES.
type Resources struct {
	FBs        []uint32
	CRTCs      []uint32
	Connectors []uint32
	Encoders   []uint32
	MinWidth   uint32
	MaxWidth   uint32
	MinHeight  uint32
	MaxHeight  uint32
}

// Mode is one entry of a connector's mode list.
type Mode struct {
	Clock      uint32
	HDisplay   uint16
	HSyncStart uint16
	HSyncEnd   uint16
	HTotal     uint16
	HSkew      uint16
	VDisplay   uint16
	VSyncStart uint16
	VSyncEnd   uint16
	VTotal     uint16
	VScan      uint16
	VRefresh   uint32
	Flags      uint32
	Type       uint32
	Name       string
}

// Preferred reports whether the mode carries DRM_MODE_TYPE_PREFERRED.
func (m Mode) Preferred() bool { return m.Type&ModeTypePreferred != 0 }

// Width and Height return the visible resolution.
func (m Mode) Width() int  { return int(m.HDisplay) }
func (m Mode) Height() int { return int(m.VDisplay) }

// RefreshMHz is the mode refresh rate in millihertz.
func (m Mode) RefreshMHz() int32 { return int32(m.VRefresh) * 1000 }

// Connector describes one output connector and its modes/properties.
type Connector struct {
	ID         uint32
	Type       uint32
	TypeID     uint32
	Connection uint32
	MMWidth    uint32
	MMHeight   uint32
	Subpixel   uint32
	EncoderID  uint32
	Encoders   []uint32
	Modes      []Mode

	Props      map[string]uint32
	PropValues map[string]uint64
}

// Name renders the kernel connector name, e.g. "eDP-1" or "HDMI-A-1".
func (c *Connector) Name() string {
	base := connectorTypeName(c.Type)
	if base == "" {
		return fmt.Sprintf("unknown-%d-%d", c.Type, c.TypeID)
	}
	return fmt.Sprintf("%s-%d", base, c.TypeID)
}

// Connected reports whether a sink is attached.
func (c *Connector) Connected() bool { return c.Connection == ModeConnected }

// Prop returns the property id for name and whether it exists.
func (c *Connector) Prop(name string) (uint32, bool) {
	id, ok := c.Props[name]
	return id, ok
}

func connectorTypeName(t uint32) string {
	switch t {
	case 1:
		return "VGA"
	case 2:
		return "DVI-I"
	case 3:
		return "DVI-D"
	case 4:
		return "DVI-A"
	case 5:
		return "Composite"
	case 6:
		return "SVIDEO"
	case 7:
		return "LVDS"
	case 8:
		return "Component"
	case 9:
		return "DIN"
	case 10:
		return "DP"
	case 11:
		return "HDMI-A"
	case 12:
		return "HDMI-B"
	case 13:
		return "TV"
	case 14:
		return "eDP"
	case 15:
		return "Virtual"
	case 16:
		return "DSI"
	case 17:
		return "DPI"
	case 18:
		return "Writeback"
	case 19:
		return "SPI"
	case 20:
		return "USB"
	}
	return ""
}

// Encoder describes one display encoder.
type Encoder struct {
	ID             uint32
	Type           uint32
	CRTCID         uint32
	PossibleCRTCs  uint32
	PossibleClones uint32
}

// CRTC describes one display controller and its currently programmed mode.
type CRTC struct {
	ID         uint32
	FBID       uint32
	X, Y       uint32
	GammaSize  uint32
	ModeValid  bool
	Mode       Mode
	Connectors []uint32

	Props      map[string]uint32
	PropValues map[string]uint64
}

// Prop returns the property id for name and whether it exists.
func (c *CRTC) Prop(name string) (uint32, bool) {
	id, ok := c.Props[name]
	return id, ok
}

// Plane describes one display plane.
type Plane struct {
	ID            uint32
	CRTCID        uint32
	FBID          uint32
	PossibleCRTCs uint32
	GammaSize     uint32
	Formats       []uint32
	Props         map[string]uint32
	PropValues    map[string]uint64
}

// Prop returns the property id for name and whether it exists.
func (p *Plane) Prop(name string) (uint32, bool) {
	id, ok := p.Props[name]
	return id, ok
}

// Property describes one KMS object property.
type Property struct {
	ID     uint32
	Name   string
	Flags  uint32
	Values []uint64
}

// Resources enumerates the device's framebuffers, CRTCs, connectors and
// encoders using the two-phase "count then fetch" protocol of GETRESOURCES.
func (d *Device) Resources() (*Resources, error) {
	var res cardRes
	if err := ioctl(d.fd, reqGetResources, unsafe.Pointer(&res)); err != nil {
		return nil, fmt.Errorf("drm: getresources: %w", err)
	}
	fbs := make([]uint32, res.CountFBs)
	crtcs := make([]uint32, res.CountCRTCs)
	conns := make([]uint32, res.CountConns)
	encs := make([]uint32, res.CountEncoders)
	if len(fbs) > 0 {
		res.FBIDPtr = u64ptr(unsafe.Pointer(&fbs[0]))
	}
	if len(crtcs) > 0 {
		res.CRTCIDPtr = u64ptr(unsafe.Pointer(&crtcs[0]))
	}
	if len(conns) > 0 {
		res.ConnectorIDPtr = u64ptr(unsafe.Pointer(&conns[0]))
	}
	if len(encs) > 0 {
		res.EncoderIDPtr = u64ptr(unsafe.Pointer(&encs[0]))
	}
	if err := ioctl(d.fd, reqGetResources, unsafe.Pointer(&res)); err != nil {
		return nil, fmt.Errorf("drm: getresources(fill): %w", err)
	}
	runtime.KeepAlive(fbs)
	runtime.KeepAlive(crtcs)
	runtime.KeepAlive(conns)
	runtime.KeepAlive(encs)
	out := &Resources{
		FBs:        fbs[:res.CountFBs],
		CRTCs:      crtcs[:res.CountCRTCs],
		Connectors: conns[:res.CountConns],
		Encoders:   encs[:res.CountEncoders],
		MinWidth:   res.MinWidth,
		MaxWidth:   res.MaxWidth,
		MinHeight:  res.MinHeight,
		MaxHeight:  res.MaxHeight,
	}
	return out, nil
}

// Connector fetches a connector's current state, mode list and properties.
func (d *Device) Connector(id uint32) (*Connector, error) {
	var ci connectorInfo
	ci.ConnectorID = id
	if err := ioctl(d.fd, reqGetConnector, unsafe.Pointer(&ci)); err != nil {
		return nil, fmt.Errorf("drm: getconnector %d: %w", id, err)
	}
	modes := make([]modeInfo, ci.CountModes)
	encs := make([]uint32, ci.CountEncoders)
	props := make([]uint32, ci.CountProps)
	vals := make([]uint64, ci.CountProps)
	if len(modes) > 0 {
		ci.ModesPtr = u64ptr(unsafe.Pointer(&modes[0]))
	}
	if len(encs) > 0 {
		ci.EncodersPtr = u64ptr(unsafe.Pointer(&encs[0]))
	}
	if len(props) > 0 {
		ci.PropsPtr = u64ptr(unsafe.Pointer(&props[0]))
		ci.PropValuesPtr = u64ptr(unsafe.Pointer(&vals[0]))
	}
	if err := ioctl(d.fd, reqGetConnector, unsafe.Pointer(&ci)); err != nil {
		return nil, fmt.Errorf("drm: getconnector(fill) %d: %w", id, err)
	}
	runtime.KeepAlive(modes)
	runtime.KeepAlive(encs)
	runtime.KeepAlive(props)
	runtime.KeepAlive(vals)

	c := &Connector{
		ID:         ci.ConnectorID,
		Type:       ci.ConnectorType,
		TypeID:     ci.TypeID,
		Connection: ci.Connection,
		MMWidth:    ci.MMWidth,
		MMHeight:   ci.MMHeight,
		Subpixel:   ci.Subpixel,
		EncoderID:  ci.EncoderID,
		Encoders:   encs[:ci.CountEncoders],
		Modes:      make([]Mode, 0, len(modes)),
		Props:      map[string]uint32{},
		PropValues: map[string]uint64{},
	}
	for _, m := range modes[:ci.CountModes] {
		c.Modes = append(c.Modes, modeFromNative(m))
	}
	for i := uint32(0); i < ci.CountProps && i < uint32(len(props)); i++ {
		name, err := d.propertyName(props[i])
		if err != nil {
			continue
		}
		c.Props[name] = props[i]
		c.PropValues[name] = vals[i]
	}
	return c, nil
}

// Encoder fetches one encoder.
func (d *Device) Encoder(id uint32) (*Encoder, error) {
	ei := encoderInfo{EncoderID: id}
	if err := ioctl(d.fd, reqGetEncoder, unsafe.Pointer(&ei)); err != nil {
		return nil, fmt.Errorf("drm: getencoder %d: %w", id, err)
	}
	return &Encoder{
		ID:             ei.EncoderID,
		Type:           ei.EncoderType,
		CRTCID:         ei.CRTCID,
		PossibleCRTCs:  ei.PossibleCRTCs,
		PossibleClones: ei.PossibleClones,
	}, nil
}

// CRTC fetches a CRTC's current mode plus its (unrelated) property set.
func (d *Device) CRTC(id uint32) (*CRTC, error) {
	var ci crtcInfo
	ci.CRTCID = id
	if err := ioctl(d.fd, reqGetCRTC, unsafe.Pointer(&ci)); err != nil {
		return nil, fmt.Errorf("drm: getcrtc %d: %w", id, err)
	}
	c := &CRTC{
		ID:        ci.CRTCID,
		FBID:      ci.FBID,
		X:         ci.X,
		Y:         ci.Y,
		GammaSize: ci.GammaSize,
		ModeValid: ci.ModeValid != 0,
		Mode:      modeFromNative(ci.Mode),
	}
	if ci.ModeValid != 0 {
		// The connector list requires a second call with a pointer; for
		// bring-up we only need the CRTC id and properties, so we skip it.
	}
	props, values, err := d.ObjectProperties(id, ObjCRTC)
	if err != nil {
		return nil, err
	}
	c.Props, c.PropValues = props, values
	return c, nil
}

// PlaneResources lists the device's plane object ids.
func (d *Device) PlaneResources() ([]uint32, error) {
	var pr planeRes
	if err := ioctl(d.fd, reqGetPlaneRes, unsafe.Pointer(&pr)); err != nil {
		return nil, fmt.Errorf("drm: getplaneresources: %w", err)
	}
	ids := make([]uint32, pr.CountPlanes)
	if len(ids) > 0 {
		pr.PlaneIDPtr = u64ptr(unsafe.Pointer(&ids[0]))
	}
	if err := ioctl(d.fd, reqGetPlaneRes, unsafe.Pointer(&pr)); err != nil {
		return nil, fmt.Errorf("drm: getplaneresources(fill): %w", err)
	}
	runtime.KeepAlive(ids)
	return ids[:pr.CountPlanes], nil
}

// Plane fetches one plane, including its supported pixel formats.
func (d *Device) Plane(id uint32) (*Plane, error) {
	var pi planeInfo
	pi.PlaneID = id
	if err := ioctl(d.fd, reqGetPlane, unsafe.Pointer(&pi)); err != nil {
		return nil, fmt.Errorf("drm: getplane %d: %w", id, err)
	}
	formats := make([]uint32, pi.CountFormatTypes)
	if len(formats) > 0 {
		pi.FormatTypePtr = u64ptr(unsafe.Pointer(&formats[0]))
	}
	if err := ioctl(d.fd, reqGetPlane, unsafe.Pointer(&pi)); err != nil {
		return nil, fmt.Errorf("drm: getplane(fill) %d: %w", id, err)
	}
	runtime.KeepAlive(formats)
	p := &Plane{
		ID:            pi.PlaneID,
		CRTCID:        pi.CRTCID,
		FBID:          pi.FBID,
		PossibleCRTCs: pi.PossibleCRTCs,
		GammaSize:     pi.GammaSize,
		Formats:       formats[:pi.CountFormatTypes],
	}
	props, values, err := d.ObjectProperties(id, ObjPlane)
	if err != nil {
		return nil, err
	}
	p.Props, p.PropValues = props, values
	return p, nil
}

// SupportsFormat reports whether the plane lists a DRM fourcc.
func (p *Plane) SupportsFormat(fourcc uint32) bool {
	for _, f := range p.Formats {
		if f == fourcc {
			return true
		}
	}
	return false
}

// ObjectProperties enumerates the property ids and current values of any KMS
// object (CRTC, connector, plane, ...) via DRM_IOCTL_MODE_OBJ_GETPROPERTIES.
func (d *Device) ObjectProperties(id, objType uint32) (map[string]uint32, map[string]uint64, error) {
	var op objGetProps
	op.ObjectID = id
	op.ObjType = objType
	if err := ioctl(d.fd, reqObjGetProps, unsafe.Pointer(&op)); err != nil {
		return nil, nil, fmt.Errorf("drm: objgetproperties %d: %w", id, err)
	}
	props := make([]uint32, op.CountProps)
	vals := make([]uint64, op.CountProps)
	if len(props) > 0 {
		op.PropsPtr = u64ptr(unsafe.Pointer(&props[0]))
		op.PropValuesPtr = u64ptr(unsafe.Pointer(&vals[0]))
	}
	if err := ioctl(d.fd, reqObjGetProps, unsafe.Pointer(&op)); err != nil {
		return nil, nil, fmt.Errorf("drm: objgetproperties(fill) %d: %w", id, err)
	}
	runtime.KeepAlive(props)
	runtime.KeepAlive(vals)
	names := map[string]uint32{}
	values := map[string]uint64{}
	for i := uint32(0); i < op.CountProps && i < uint32(len(props)); i++ {
		name, err := d.propertyName(props[i])
		if err != nil {
			continue
		}
		names[name] = props[i]
		values[name] = vals[i]
	}
	return names, values, nil
}

// Property fetches a property's name, flags and range values.
func (d *Device) Property(id uint32) (*Property, error) {
	var pi propertyInfo
	pi.PropID = id
	if err := ioctl(d.fd, reqGetProperty, unsafe.Pointer(&pi)); err != nil {
		return nil, fmt.Errorf("drm: getproperty %d: %w", id, err)
	}
	return &Property{ID: pi.PropID, Name: cString(pi.Name[:]), Flags: pi.Flags}, nil
}

// propertyName resolves a property id to its name.
func (d *Device) propertyName(id uint32) (string, error) {
	p, err := d.Property(id)
	if err != nil {
		return "", err
	}
	return p.Name, nil
}

// PropertyBlob reads a blob property's payload.
func (d *Device) PropertyBlob(id uint32) ([]byte, error) {
	var pb propertyBlob
	pb.BlobID = id
	if err := ioctl(d.fd, reqGetPropertyBl, unsafe.Pointer(&pb)); err != nil {
		return nil, fmt.Errorf("drm: getblob %d: %w", id, err)
	}
	if pb.Length == 0 {
		return nil, nil
	}
	data := make([]byte, pb.Length)
	pb.Data = u64ptr(unsafe.Pointer(&data[0]))
	if err := ioctl(d.fd, reqGetPropertyBl, unsafe.Pointer(&pb)); err != nil {
		return nil, fmt.Errorf("drm: getblob(fill) %d: %w", id, err)
	}
	runtime.KeepAlive(data)
	return data[:pb.Length], nil
}

// CreatePropertyBlob uploads an opaque byte blob and returns its property id.
// Atomic mode setting uses a blob to carry the selected CRTC mode.
func (d *Device) CreatePropertyBlob(data []byte) (uint32, error) {
	cb := createBlob{Length: uint32(len(data))}
	if len(data) > 0 {
		cb.Data = u64ptr(unsafe.Pointer(&data[0]))
	}
	if err := ioctl(d.fd, reqCreatePropBlob, unsafe.Pointer(&cb)); err != nil {
		return 0, fmt.Errorf("drm: createblob: %w", err)
	}
	runtime.KeepAlive(data)
	return cb.BlobID, nil
}

// modeFromNative converts a kernel mode description to Mode.
func modeFromNative(m modeInfo) Mode {
	return Mode{
		Clock: m.Clock, HDisplay: m.HDisplay, HSyncStart: m.HSyncStart,
		HSyncEnd: m.HSyncEnd, HTotal: m.HTotal, HSkew: m.HSkew,
		VDisplay: m.VDisplay, VSyncStart: m.VSyncStart, VSyncEnd: m.VSyncEnd,
		VTotal: m.VTotal, VScan: m.VScan, VRefresh: m.VRefresh,
		Flags: m.Flags, Type: m.Type, Name: cString(m.Name[:]),
	}
}

// Native re-encodes a Mode for the mode blob written to the kernel. The name
// is fixed-width in the kernel ABI, so the string is truncated to 32 bytes.
func (m Mode) Native() modeInfo {
	out := modeInfo{
		Clock: m.Clock, HDisplay: m.HDisplay, HSyncStart: m.HSyncStart,
		HSyncEnd: m.HSyncEnd, HTotal: m.HTotal, HSkew: m.HSkew,
		VDisplay: m.VDisplay, VSyncStart: m.VSyncStart, VSyncEnd: m.VSyncEnd,
		VTotal: m.VTotal, VScan: m.VScan, VRefresh: m.VRefresh,
		Flags: m.Flags, Type: m.Type,
	}
	copy(out.Name[:], m.Name)
	return out
}

// ModeBlobBytes returns the byte payload for DRM_IOCTL_MODE_CREATEPROPBLOB
// from a Mode, ready to hand to CreatePropertyBlob.
func (m Mode) ModeBlobBytes() []byte {
	native := m.Native()
	return unsafe.Slice((*byte)(unsafe.Pointer(&native)), unsafe.Sizeof(native))
}

// cString trims a NUL-terminated fixed-width byte array.
func cString(b []byte) string {
	for i, c := range b {
		if c == 0 {
			return string(b[:i])
		}
	}
	return string(b)
}

// u64ptr converts a pointer to the 64-bit integer the kernel ABI uses for
// user pointers in the mode-setting structs (__u64), independent of the
// host's native uintptr width.
func u64ptr(p unsafe.Pointer) uint64 { return uint64(uintptr(p)) }
