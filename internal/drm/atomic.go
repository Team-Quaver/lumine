package drm

import (
	"encoding/binary"
	"errors"
	"fmt"
	"runtime"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Atomic commit and page-flip flags from <drm/drm_mode.h>.
const (
	AtomicTestOnly     uint32 = 0x0100
	AtomicNonblock     uint32 = 0x0200
	AtomicAllowModeset uint32 = 0x0400

	PageFlipEvent uint32 = 0x01
	PageFlipAsync uint32 = 0x02
)

// DRM event types delivered on the device fd.
const (
	EventVblank       uint32 = 0x01
	EventFlipComplete uint32 = 0x02
)

// eventSize is sizeof(struct drm_event_vblank): drm_event base (8) + user_data
// (8) + tv_sec/tv_usec/sequence/crtc_id (16).
const eventSize = 32

// Atom builds a DRM_IOCTL_MODE_ATOMIC request. Properties are grouped by the
// object they belong to, because the ioctl encodes objects with a parallel
// array of per-object property counts.
type Atom struct {
	objs   []uint32
	perObj map[uint32][]propVal
}

type propVal struct {
	prop  uint32
	value uint64
}

// NewAtom returns an empty property set.
func NewAtom() *Atom {
	return &Atom{perObj: map[uint32][]propVal{}}
}

// Set adds or replaces a property value on objID.
func (a *Atom) Set(objID, propID uint32, value uint64) {
	ps, ok := a.perObj[objID]
	if !ok {
		a.objs = append(a.objs, objID)
	}
	for i := range ps {
		if ps[i].prop == propID {
			ps[i].value = value
			a.perObj[objID] = ps
			return
		}
	}
	a.perObj[objID] = append(ps, propVal{prop: propID, value: value})
}

// Len reports how many objects carry at least one property.
func (a *Atom) Len() int { return len(a.objs) }

// flatten lays the per-object property lists out in the parallel arrays the
// atomic ioctl expects.
func (a *Atom) flatten() (counts, props []uint32, values []uint64) {
	for _, obj := range a.objs {
		ps := a.perObj[obj]
		counts = append(counts, uint32(len(ps)))
		for _, p := range ps {
			props = append(props, p.prop)
			values = append(values, p.value)
		}
	}
	return counts, props, values
}

// Commit applies the property set atomically. flags is a combination of the
// Atomic* constants; userData is echoed back in the resulting completion
// event when PageFlipEvent is set.
func (d *Device) Commit(a *Atom, flags uint32, userData uint64) error {
	if a == nil || a.Len() == 0 {
		return fmt.Errorf("drm: empty atomic commit")
	}
	counts, props, values := a.flatten()
	req := atomicReq{
		Flags:         flags,
		CountObjs:     uint32(len(a.objs)),
		ObjsPtr:       u64ptr(unsafe.Pointer(&a.objs[0])),
		CountPropsPtr: u64ptr(unsafe.Pointer(&counts[0])),
		PropsPtr:      u64ptr(unsafe.Pointer(&props[0])),
		PropValuesPtr: u64ptr(unsafe.Pointer(&values[0])),
		UserData:      userData,
	}
	if err := ioctl(d.fd, reqAtomic, unsafe.Pointer(&req)); err != nil {
		return fmt.Errorf("drm: atomic commit: %w", err)
	}
	runtime.KeepAlive(a.objs)
	runtime.KeepAlive(counts)
	runtime.KeepAlive(props)
	runtime.KeepAlive(values)
	return nil
}

// TestCommit validates an atomic request without touching hardware
// (DRM_MODE_ATOMIC_TEST_ONLY). It is how the backend checks a mode before
// committing, so a bad mode fails cleanly rather than blanking the screen.
func (d *Device) TestCommit(a *Atom) error {
	return d.Commit(a, AtomicTestOnly|AtomicAllowModeset, 0)
}

// PageFlip programs a CRTC's primary plane with fbID and asks the kernel to
// deliver a DRM_EVENT_FLIP_COMPLETE when the flip lands. userData is returned
// verbatim in the event so the caller can recycle the right buffer.
//
// This uses a legacy page flip rather than an atomic commit: it is accepted on
// every KMS driver (including NVIDIA's) and, because the CRTC is already
// modeset by the initial atomic commit, only the scanout address changes.
func (d *Device) PageFlip(crtcID, fbID uint32, userData uint64) error {
	pf := pageFlip{CRTCID: crtcID, FBID: fbID, Flags: PageFlipEvent, UserData: userData}
	if err := ioctl(d.fd, reqPageFlip, unsafe.Pointer(&pf)); err != nil {
		return fmt.Errorf("drm: page flip crtc=%d fb=%d: %w", crtcID, fbID, err)
	}
	return nil
}

// Event is a decoded drm_event_vblank.
type Event struct {
	Type     uint32
	UserData uint64
	Sec      uint32
	Usec     uint32
	Sequence uint32
	CRTCID   uint32
}

// ReadEvent reads one queued DRM event. It returns unix.EAGAIN when the fd is
// non-blocking and nothing is pending, and io.EOF when the device closed.
func (d *Device) ReadEvent() (Event, error) {
	var buf [eventSize]byte
	n, err := unix.Read(d.fd, buf[:])
	if err != nil {
		return Event{}, err
	}
	if n < eventSize {
		if n == 0 {
			return Event{}, errors.New("drm: device closed")
		}
		// A short read means the buffer was drained between events; retry the
		// remainder once before giving up.
		for n < eventSize {
			m, err := unix.Read(d.fd, buf[n:])
			if err != nil {
				return Event{}, err
			}
			if m == 0 {
				return Event{}, errors.New("drm: device closed")
			}
			n += m
		}
	}
	return Event{
		Type:     binary.LittleEndian.Uint32(buf[0:4]),
		UserData: binary.LittleEndian.Uint64(buf[8:16]),
		Sec:      binary.LittleEndian.Uint32(buf[16:20]),
		Usec:     binary.LittleEndian.Uint32(buf[20:24]),
		Sequence: binary.LittleEndian.Uint32(buf[24:28]),
		CRTCID:   binary.LittleEndian.Uint32(buf[28:32]),
	}, nil
}

// SetNonblock toggles O_NONBLOCK on the device fd so the event loop can drain
// events without ever blocking on a read.
func (d *Device) SetNonblock(on bool) error {
	flags, err := unix.FcntlInt(uintptr(d.fd), unix.F_GETFL, 0)
	if err != nil {
		return err
	}
	if on {
		flags |= unix.O_NONBLOCK
	} else {
		flags &^= unix.O_NONBLOCK
	}
	_, err = unix.FcntlInt(uintptr(d.fd), unix.F_SETFL, flags)
	return err
}
