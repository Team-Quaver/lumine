// Package drm is a minimal, pure-Go direct rendering manager (DRM) / kernel
// mode setting (KMS) client. It speaks the kernel ioctl ABI directly through
// golang.org/x/sys/unix, with no libdrm and no CGO, mirroring how the rest of
// Lumine avoids native libraries.
//
// The package covers exactly what a compositor needs to bring up one output:
// device capability probing, resource/connector/CRTC/plane enumeration,
// property lookup (including blobs), dumb-buffer allocation, framebuffer
// creation (legacy ADDFB and modifier-aware ADDFB2), atomic commits and page
// flips. It is deliberately independent of Vulkan and of the compositor so it
// can be unit tested without a display.
//
// A note on the ioctl request numbers: every constant below is derived from
// the same _IOWR(type, nr, size) encoding the kernel uses, so the size field
// always matches the Go struct passed to the kernel. drm_test.go pins those
// values to the numbers from <drm/drm.h> to catch accidental drift.
package drm

import (
	"errors"
	"fmt"
	"unsafe"

	"golang.org/x/sys/unix"
)

// ioctl direction bits, matching <asm-generic/ioctl.h>.
const (
	iocNone  = 0
	iocWrite = 1
	iocRead  = 2
)

// drmIoctlBase is the ioctl "type" byte the DRM subsystem reserves ('d').
const drmIoctlBase = 'd'

// ioc encodes an _IOC(dir, type, nr, size) request.
func ioc(dir, typ, nr, size uintptr) uintptr {
	return dir<<30 | size<<16 | typ<<8 | nr
}

// io encodes a DRM_IO(nr) request (no payload).
func io(nr uintptr) uintptr { return ioc(iocNone, drmIoctlBase, nr, 0) }

// iowr encodes a DRM_IOWR(nr, T) request for a payload of the given size.
func iowr(nr, size uintptr) uintptr { return ioc(iocRead|iocWrite, drmIoctlBase, nr, size) }

// iow encodes a DRM_IOW(nr, T) request for a write-only payload.
func iow(nr, size uintptr) uintptr { return ioc(iocWrite, drmIoctlBase, nr, size) }

// Request numbers used by the compositor. The numeric values are asserted in
// drm_test.go against <drm/drm.h>.
var (
	reqGetCap         = iowr(0x0c, unsafe.Sizeof(getCap{}))
	reqSetClientCap   = iow(0x0d, unsafe.Sizeof(setClientCap{}))
	reqSetMaster      = io(0x1e)
	reqDropMaster     = io(0x1f)
	reqPrimeToFD      = iowr(0x2d, unsafe.Sizeof(primeHandle{}))
	reqPrimeToHandle  = iowr(0x2e, unsafe.Sizeof(primeHandle{}))
	reqGemClose       = io(0x09)
	reqGetResources   = iowr(0xa0, unsafe.Sizeof(cardRes{}))
	reqGetCRTC        = iowr(0xa1, unsafe.Sizeof(crtcInfo{}))
	reqGetEncoder     = iowr(0xa6, unsafe.Sizeof(encoderInfo{}))
	reqGetConnector   = iowr(0xa7, unsafe.Sizeof(connectorInfo{}))
	reqGetProperty    = iowr(0xaa, unsafe.Sizeof(propertyInfo{}))
	reqGetPropertyBl  = iowr(0xac, unsafe.Sizeof(propertyBlob{}))
	reqAddFB          = iowr(0xae, unsafe.Sizeof(fbCmd{}))
	reqRmFB           = iowr(0xaf, unsafe.Sizeof(uint32(0)))
	reqPageFlip       = iowr(0xb0, unsafe.Sizeof(pageFlip{}))
	reqCreateDumb     = iowr(0xb2, unsafe.Sizeof(createDumb{}))
	reqMapDumb        = iowr(0xb3, unsafe.Sizeof(mapDumb{}))
	reqDestroyDumb    = iowr(0xb4, unsafe.Sizeof(uint32(0)))
	reqGetPlaneRes    = iowr(0xb5, unsafe.Sizeof(planeRes{}))
	reqGetPlane       = iowr(0xb6, unsafe.Sizeof(planeInfo{}))
	reqAddFB2         = iowr(0xb8, unsafe.Sizeof(fbCmd2{}))
	reqObjGetProps    = iowr(0xb9, unsafe.Sizeof(objGetProps{}))
	reqObjSetProp     = iowr(0xba, unsafe.Sizeof(objSetProp{}))
	reqAtomic         = iowr(0xbc, unsafe.Sizeof(atomicReq{}))
	reqCreatePropBlob = iowr(0xbd, unsafe.Sizeof(createBlob{}))
)

// DRM_CAP_* capability ids.
const (
	CapDumbBuffer uint64 = 0x2
	CapPrime      uint64 = 0x7 // DRM_CAP_PRIME
)

// DRM_CLIENT_CAP_* capability ids for DRM_IOCTL_SET_CLIENT_CAP. The kernel
// hides the atomic-only properties (CRTC MODE_ID/ACTIVE, plane FB_ID/CRTC_ID/
// SRC_*) until DRM_CLIENT_CAP_ATOMIC is set, and only exposes the full plane
// list once DRM_CLIENT_CAP_UNIVERSAL_PLANES is set.
const (
	ClientCapStereo3D        uint64 = 1
	ClientCapUniversalPlanes uint64 = 2
	ClientCapAtomic          uint64 = 3
	ClientCapAspectRatio     uint64 = 4
)

// PRIME capability bits (the value returned for DRM_CAP_PRIME).
const (
	PrimeImport uint64 = 1 << 0
	PrimeExport uint64 = 1 << 1
)

// DRM_MODE_OBJECT_* type tags for object property ioctls.
const (
	ObjCRTC      uint32 = 0xcccccccc
	ObjConnector uint32 = 0xc0c0c0c0
	ObjEncoder   uint32 = 0xe0e0e0e0
	ObjPlane     uint32 = 0xeeeeeeee
)

// DRM_MODE_* enumerations used to interpret connector and mode data.
const (
	ModeConnected    = 1
	ModeDisconnected = 2
	ModeUnknown      = 3

	ModeTypePreferred = 1 << 3
)

// ioctl runs one ioctl on fd, passing arg as the request argument.
func ioctl(fd int, req uintptr, arg unsafe.Pointer) error {
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), req, uintptr(arg))
	if errno != 0 {
		return errno
	}
	return nil
}

// Device is an open /dev/dri/cardN (or renderD128) node.
type Device struct {
	fd     int
	path   string
	master bool
}

// Open opens a DRM node read-write. Callers that need to change modes must
// call SetMaster after opening.
func Open(path string) (*Device, error) {
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("drm: open %s: %w", path, err)
	}
	return &Device{fd: fd, path: path}, nil
}

// FD returns the underlying file descriptor for polling.
func (d *Device) FD() int { return d.fd }

// Path returns the device node path.
func (d *Device) Path() string { return d.path }

// IsMaster reports whether this device currently holds DRM master.
func (d *Device) IsMaster() bool { return d.master }

// SetMaster becomes the DRM master for this device. It fails with EBUSY when
// another process (a running compositor or the logind session owner) holds
// the master, which is the signal that Lumine must not take over the screen.
func (d *Device) SetMaster() error {
	if d.master {
		return nil
	}
	if err := ioctl(d.fd, reqSetMaster, nil); err != nil {
		return fmt.Errorf("drm: set master on %s: %w", d.path, err)
	}
	d.master = true
	return nil
}

// DropMaster releases DRM master, returning the device to normal mode.
func (d *Device) DropMaster() error {
	if !d.master {
		return nil
	}
	err := ioctl(d.fd, reqDropMaster, nil)
	d.master = false
	if err != nil {
		return fmt.Errorf("drm: drop master on %s: %w", d.path, err)
	}
	return nil
}

// GetCap reads a DRM_CAP_* capability.
func (d *Device) GetCap(cap uint64) (uint64, error) {
	c := getCap{Capability: cap}
	if err := ioctl(d.fd, reqGetCap, unsafe.Pointer(&c)); err != nil {
		return 0, fmt.Errorf("drm: get cap %#x: %w", cap, err)
	}
	return c.Value, nil
}

// SetClientCap opts this file descriptor into a DRM_CLIENT_CAP_* capability.
// The value must be 0 or 1; the ioctl is rejected otherwise.
func (d *Device) SetClientCap(cap, value uint64) error {
	c := setClientCap{Capability: cap, Value: value}
	if err := ioctl(d.fd, reqSetClientCap, unsafe.Pointer(&c)); err != nil {
		return fmt.Errorf("drm: set client cap %d: %w", cap, err)
	}
	return nil
}

// EnableAtomic requests the capabilities atomic modesetting needs:
// universal planes (so every plane and its properties are enumerated) and
// atomic (so CRTCs expose MODE_ID/ACTIVE and planes expose FB_ID/CRTC_ID/
// SRC_*). It must be called before enumerating resources; without it the
// kernel reports the atomic properties as absent and the mode set fails.
func (d *Device) EnableAtomic() error {
	if err := d.SetClientCap(ClientCapUniversalPlanes, 1); err != nil {
		return err
	}
	return d.SetClientCap(ClientCapAtomic, 1)
}

// DumbBufferSupported reports whether the device advertises dumb-buffer
// allocation, the baseline requirement for the fallback scanout path.
func (d *Device) DumbBufferSupported() bool {
	v, err := d.GetCap(CapDumbBuffer)
	return err == nil && v == 1
}

// CanExportDmaBuf reports whether the device supports exporting GEM handles
// as dma-buf file descriptors.
func (d *Device) CanExportDmaBuf() bool {
	v, err := d.GetCap(CapPrime)
	return err == nil && v&PrimeExport != 0
}

// Close releases the device, dropping master first if it was held.
func (d *Device) Close() error {
	if d.fd < 0 {
		return nil
	}
	if d.master {
		_ = d.DropMaster()
	}
	err := unix.Close(d.fd)
	d.fd = -1
	return err
}

// IsNotMaster reports whether err is the EBUSY/EACCES failure the kernel
// returns when a process that is not the session master tries to modeset.
func IsNotMaster(err error) bool {
	return errors.Is(err, unix.EACCES) || errors.Is(err, unix.EBUSY)
}

// ---- ioctl payloads ----
//
// These structs mirror <drm/drm_mode.h> byte for byte. Go's natural alignment
// matches the kernel's for every type here (uint64 fields are 8-aligned), and
// drm_test.go asserts sizeof against the expected ioctl size field.

type getCap struct {
	Capability uint64
	Value      uint64
}

// setClientCap is struct drm_set_client_cap, shared by any capability.
type setClientCap struct {
	Capability uint64
	Value      uint64
}

type primeHandle struct {
	Handle uint32
	Flags  uint32
	FD     int32
	Pad    uint32
}

// cardRes is struct drm_mode_card_res.
type cardRes struct {
	FBIDPtr        uint64
	CRTCIDPtr      uint64
	ConnectorIDPtr uint64
	EncoderIDPtr   uint64
	CountFBs       uint32
	CountCRTCs     uint32
	CountConns     uint32
	CountEncoders  uint32
	MinWidth       uint32
	MaxWidth       uint32
	MinHeight      uint32
	MaxHeight      uint32
}

// modeInfo is struct drm_mode_modeinfo: a 68-byte packed mode description.
type modeInfo struct {
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
	Name       [32]byte
}

// crtcInfo is struct drm_mode_crtc (used for GETCRTC).
type crtcInfo struct {
	SetConnectorsPtr uint64
	CountConnectors  uint32
	CRTCID           uint32
	FBID             uint32
	X                uint32
	Y                uint32
	GammaSize        uint32
	ModeValid        uint32
	Mode             modeInfo
}

// encoderInfo is struct drm_mode_get_encoder.
type encoderInfo struct {
	EncoderID      uint32
	EncoderType    uint32
	CRTCID         uint32
	PossibleCRTCs  uint32
	PossibleClones uint32
}

// connectorInfo is struct drm_mode_get_connector.
type connectorInfo struct {
	EncodersPtr   uint64
	ModesPtr      uint64
	PropsPtr      uint64
	PropValuesPtr uint64
	CountModes    uint32
	CountProps    uint32
	CountEncoders uint32
	EncoderID     uint32
	ConnectorID   uint32
	ConnectorType uint32
	TypeID        uint32
	Connection    uint32
	MMWidth       uint32
	MMHeight      uint32
	Subpixel      uint32
	Pad           uint32
}

// propertyInfo is struct drm_mode_get_property.
type propertyInfo struct {
	ValuesPtr     uint64
	EnumBlobPtr   uint64
	PropID        uint32
	Flags         uint32
	Name          [32]byte
	CountValues   uint32
	CountEnumBlob uint32
}

// propertyBlob is struct drm_mode_get_blob.
type propertyBlob struct {
	BlobID uint32
	Length uint32
	Data   uint64
}

// objGetProps is struct drm_mode_obj_get_properties.
type objGetProps struct {
	PropsPtr      uint64
	PropValuesPtr uint64
	CountProps    uint32
	ObjectID      uint32
	ObjType       uint32
}

// createBlob is struct drm_mode_create_blob.
type createBlob struct {
	Data   uint64
	Length uint32
	BlobID uint32
}

// planeRes is struct drm_mode_get_plane_res.
type planeRes struct {
	PlaneIDPtr  uint64
	CountPlanes uint32
}

// planeInfo is struct drm_mode_get_plane.
type planeInfo struct {
	PlaneID          uint32
	CRTCID           uint32
	FBID             uint32
	PossibleCRTCs    uint32
	GammaSize        uint32
	CountFormatTypes uint32
	FormatTypePtr    uint64
}

// createDumb is struct drm_mode_create_dumb.
type createDumb struct {
	Height uint32
	Width  uint32
	Bpp    uint32
	Flags  uint32
	Handle uint32
	Pitch  uint32
	Size   uint64
}

// mapDumb is struct drm_mode_map_dumb.
type mapDumb struct {
	Handle uint32
	Pad    uint32
	Offset uint64
}

// fbCmd is the legacy struct drm_mode_fb_cmd.
type fbCmd struct {
	FBID   uint32
	Width  uint32
	Height uint32
	Pitch  uint32
	Bpp    uint32
	Depth  uint32
	Handle uint32
}

// fbCmd2 is struct drm_mode_fb_cmd2 (modifier-aware ADDFB2).
type fbCmd2 struct {
	FBID        uint32
	Width       uint32
	Height      uint32
	PixelFormat uint32
	Flags       uint32
	Handles     [4]uint32
	Pitches     [4]uint32
	Offsets     [4]uint32
	Modifier    [4]uint64
}

// objSetProp is struct drm_mode_obj_set_property.
type objSetProp struct {
	Value    uint64
	PropID   uint32
	ObjectID uint32
	ObjType  uint32
}

// atomicReq is struct drm_mode_atomic.
type atomicReq struct {
	Flags         uint32
	CountObjs     uint32
	ObjsPtr       uint64
	CountPropsPtr uint64
	PropsPtr      uint64
	PropValuesPtr uint64
	Reserved      uint64
	UserData      uint64
}

// pageFlip is struct drm_mode_crtc_page_flip.
type pageFlip struct {
	CRTCID   uint32
	FBID     uint32
	Flags    uint32
	Reserved uint32
	UserData uint64
}
