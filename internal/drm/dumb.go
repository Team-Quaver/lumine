package drm

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/unix"
)

// PRIME fd flags understood by DRM_IOCTL_PRIME_HANDLE_TO_FD.
const (
	PrimeCloexec = 1 << 0
	PrimeRdwr    = 1 << 1
)

// FourCC codes from <drm/drm_fourcc.h>. XRGB8888/ARGB8888 are the two the
// compositor's framebuffer layout (0xAARRGGBB, little-endian bytes B,G,R,X)
// maps onto directly.
const (
	FourccXRGB8888 uint32 = 0x34325258 // 'X','R','2','4'
	FourccARGB8888 uint32 = 0x34325241 // 'A','R','2','4'
	// FourccModLinear is DRM_FORMAT_MOD_LINEAR (modifier 0 with the "none"
	// vendor), the only modifier a dumb buffer ever carries.
	FourccModLinear uint64 = 0
	// FbModifiers is DRM_MODE_FB_MODIFIERS.
	FbModifiers uint32 = 1 << 1
)

// Dumb is a KMS dumb buffer: a linear, CPU-mappable scanout buffer.
type Dumb struct {
	Handle uint32
	Pitch  uint32
	Size   uint64
	Offset uint64

	// Map is the mmap'd pixel memory. Writes through it are what the display
	// controller scans out, which is why the software-copy present path is
	// portable across Intel, AMD and NVIDIA.
	Map []byte

	dev *Device
}

// CreateDumb allocates and maps a dumb buffer of the given size. bpp is bits
// per pixel (32 for XRGB8888).
func (d *Device) CreateDumb(width, height, bpp uint32) (*Dumb, error) {
	cd := createDumb{Width: width, Height: height, Bpp: bpp}
	if err := ioctl(d.fd, reqCreateDumb, unsafe.Pointer(&cd)); err != nil {
		return nil, fmt.Errorf("drm: create dumb %dx%d: %w", width, height, err)
	}
	md := mapDumb{Handle: cd.Handle}
	if err := ioctl(d.fd, reqMapDumb, unsafe.Pointer(&md)); err != nil {
		_ = d.destroyDumb(cd.Handle)
		return nil, fmt.Errorf("drm: map dumb: %w", err)
	}
	mem, err := unix.Mmap(d.fd, int64(md.Offset), int(cd.Size), unix.PROT_READ|unix.PROT_WRITE, unix.MAP_SHARED)
	if err != nil {
		_ = d.destroyDumb(cd.Handle)
		return nil, fmt.Errorf("drm: mmap dumb: %w", err)
	}
	return &Dumb{Handle: cd.Handle, Pitch: cd.Pitch, Size: cd.Size, Offset: md.Offset, Map: mem, dev: d}, nil
}

// ExportFD exports the buffer as a dma-buf file descriptor. The returned fd
// is owned by the caller.
func (b *Dumb) ExportFD() (int, error) {
	p := primeHandle{Handle: b.Handle, Flags: PrimeRdwr | PrimeCloexec}
	if err := ioctl(b.dev.fd, reqPrimeToFD, unsafe.Pointer(&p)); err != nil {
		return -1, fmt.Errorf("drm: prime handle->fd: %w", err)
	}
	return int(p.FD), nil
}

// AddFBFor registers a legacy (implicit-modifier) framebuffer for the given
// geometry and dumb buffer. bpp is 32 and depth 24 for XRGB8888.
func (d *Device) AddFBFor(b *Dumb, width, height, bpp, depth uint32) (uint32, error) {
	fc := fbCmd{Width: width, Height: height, Pitch: b.Pitch, Bpp: bpp, Depth: depth, Handle: b.Handle}
	if err := ioctl(d.fd, reqAddFB, unsafe.Pointer(&fc)); err != nil {
		return 0, fmt.Errorf("drm: addfb %dx%d: %w", width, height, err)
	}
	return fc.FBID, nil
}

// AddFB2 registers a framebuffer with an explicit format and modifier. It is
// used both for linear dumb buffers and for dma-bufs exported by the Vulkan
// scanout path.
func (d *Device) AddFB2(fd int, width, height, pitch, offset uint32, fourcc uint32, modifier uint64) (uint32, error) {
	// ADDFB2 takes GEM handles, so convert the dma-buf fd back into one.
	handle, err := d.fdToHandle(fd)
	if err != nil {
		return 0, err
	}
	fc := fbCmd2{
		Width: width, Height: height, PixelFormat: fourcc, Flags: FbModifiers,
		Handles: [4]uint32{handle}, Pitches: [4]uint32{pitch}, Offsets: [4]uint32{offset},
		Modifier: [4]uint64{modifier},
	}
	if err := ioctl(d.fd, reqAddFB2, unsafe.Pointer(&fc)); err != nil {
		return 0, fmt.Errorf("drm: addfb2 %dx%d: %w", width, height, err)
	}
	return fc.FBID, nil
}

// AddFB2Handle is like AddFB2 but takes an already-imported GEM handle.
func (d *Device) AddFB2Handle(handle, width, height, pitch, offset uint32, fourcc uint32, modifier uint64) (uint32, error) {
	fc := fbCmd2{
		Width: width, Height: height, PixelFormat: fourcc, Flags: FbModifiers,
		Handles: [4]uint32{handle}, Pitches: [4]uint32{pitch}, Offsets: [4]uint32{offset},
		Modifier: [4]uint64{modifier},
	}
	if err := ioctl(d.fd, reqAddFB2, unsafe.Pointer(&fc)); err != nil {
		return 0, fmt.Errorf("drm: addfb2 %dx%d: %w", width, height, err)
	}
	return fc.FBID, nil
}

// RmFB removes a framebuffer, ignoring ENOENT so teardown is idempotent.
func (d *Device) RmFB(id uint32) error {
	if id == 0 {
		return nil
	}
	v := id
	if err := ioctl(d.fd, reqRmFB, unsafe.Pointer(&v)); err != nil {
		return fmt.Errorf("drm: rmfb %d: %w", id, err)
	}
	return nil
}

// Destroy unmaps the buffer and frees the GEM object.
func (b *Dumb) Destroy() error {
	if b == nil || b.dev == nil {
		return nil
	}
	if b.Map != nil {
		_ = unix.Munmap(b.Map)
		b.Map = nil
	}
	err := b.dev.destroyDumb(b.Handle)
	b.dev = nil
	return err
}

func (d *Device) destroyDumb(handle uint32) error {
	if handle == 0 {
		return nil
	}
	h := handle
	if err := ioctl(d.fd, reqDestroyDumb, unsafe.Pointer(&h)); err != nil {
		return fmt.Errorf("drm: destroy dumb %d: %w", handle, err)
	}
	return nil
}

// fdToHandle imports a dma-buf file descriptor as a local GEM handle
// (DRM_IOCTL_PRIME_FD_TO_HANDLE).
func (d *Device) fdToHandle(fd int) (uint32, error) {
	p := primeHandle{FD: int32(fd), Flags: PrimeRdwr | PrimeCloexec}
	if err := ioctl(d.fd, reqPrimeToHandle, unsafe.Pointer(&p)); err != nil {
		return 0, fmt.Errorf("drm: prime fd->handle: %w", err)
	}
	return p.Handle, nil
}

// CloseHandle closes a GEM handle obtained from fdToHandle
// (DRM_IOCTL_GEM_CLOSE).
func (d *Device) CloseHandle(handle uint32) error {
	if handle == 0 {
		return nil
	}
	h := handle
	if err := ioctl(d.fd, reqGemClose, unsafe.Pointer(&h)); err != nil {
		return fmt.Errorf("drm: gem close %d: %w", handle, err)
	}
	return nil
}
