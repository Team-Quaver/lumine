// Package evdev reads Linux input events straight from /dev/input/event*
// devices. It is the input half of the bare-metal DRM backend: no libinput, no
// libevdev, no CGO, just the evdev ioctl ABI through golang.org/x/sys/unix.
//
// The package is deliberately small. It exposes raw (type, code, value)
// events plus the device capabilities a compositor needs to route them
// (keyboard keys, pointer buttons, relative axes and absolute axes). The
// translation into compositor actions lives in the backend so this package
// stays testable without a compositor.
//
// A note on the ioctl request numbers: every constant below is derived from
// the same _IOC(dir, type, nr, size) encoding the kernel uses for
// <linux/input.h>, so the size field always matches the Go struct passed to
// the kernel. evdev_test.go pins those values.
package evdev

import (
	"fmt"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"unsafe"

	"golang.org/x/sys/unix"
)

// ioctl direction bits, matching <asm-generic/ioctl.h>.
const (
	iocNone  = 0
	iocWrite = 1
	iocRead  = 2
)

// evdevIoctlBase is the ioctl "type" byte the input subsystem reserves ('E').
const evdevIoctlBase = 'E'

// ioc encodes an _IOC(dir, type, nr, size) request.
func ioc(dir, typ, nr, size uintptr) uintptr {
	return dir<<30 | size<<16 | typ<<8 | nr
}

// reqGetName reads the device name; the length in the size field is the caller
// supplied buffer size.
func reqGetName(size uintptr) uintptr { return ioc(iocRead, evdevIoctlBase, 0x06, size) }

// reqGetBit reads the capability bitmap for one event type
// (EVIOCGBIT(ev, len)).
func reqGetBit(ev, size uintptr) uintptr { return ioc(iocRead, evdevIoctlBase, 0x20+ev, size) }

// reqGetAbs reads the value/limits of one absolute axis (EVIOCGABS(abs)).
func reqGetAbs(abs uintptr) uintptr {
	return ioc(iocRead, evdevIoctlBase, 0x40+abs, unsafe.Sizeof(absInfo{}))
}

// reqGrab is EVIOCGRAB: exclusive access to the device.
func reqGrab() uintptr { return ioc(iocWrite, evdevIoctlBase, 0x90, unsafe.Sizeof(int32(0))) }

// Event types from <linux/input-event-codes.h>.
const (
	EvSyn = 0x00 // EV_SYN
	EvKey = 0x01 // EV_KEY
	EvRel = 0x02 // EV_REL
	EvAbs = 0x03 // EV_ABS
)

// Event codes used by the pointer/keyboard translation.
const (
	SynReport  = 0x00 // SYN_REPORT
	SynDropped = 0x03 // SYN_DROPPED

	RelX           = 0x00 // REL_X
	RelY           = 0x01 // REL_Y
	RelHWheel      = 0x06 // REL_HWHEEL
	RelWheel       = 0x08 // REL_WHEEL
	RelHWheelHiRes = 0x0b // REL_HWHEEL_HI_RES
	RelWheelHiRes  = 0x0c // REL_WHEEL_HI_RES

	AbsX           = 0x00 // ABS_X
	AbsY           = 0x01 // ABS_Y
	AbsMtPositionX = 0x35 // ABS_MT_POSITION_X
	AbsMtPositionY = 0x36 // ABS_MT_POSITION_Y
)

// Key codes. Keyboard keys live below BtnMisc (0x100); everything from there
// up is a button (BTN_*), which is exactly the split a compositor needs to
// route EV_KEY events to wl_keyboard or wl_pointer.
const (
	KeyMax  = 0xff
	BtnMisc = 0x100

	BtnLeft    = 0x110
	BtnRight   = 0x111
	BtnMiddle  = 0x112
	BtnSide    = 0x113
	BtnExtra   = 0x114
	BtnForward = 0x115
	BtnBack    = 0x116
	BtnTask    = 0x117

	BtnToolPen    = 0x142 // BTN_TOOL_* codes mark tool presence on a
	BtnToolRubber = 0x146 // pointing device; they are not clickable
	BtnToolBrush  = 0x148 // buttons and must not be forwarded as
	BtnToolFinger = 0x14a // wl_pointer.button events.
	BtnToolDouble = 0x14b
	BtnToolTriple = 0x14c
	BtnToolQuad   = 0x14d
	BtnToolQuint  = 0x14e

	KeyLeftCtrl   = 29
	KeyRightCtrl  = 97
	KeyLeftShift  = 42
	KeyRightShift = 54
	KeyLeftAlt    = 56
	KeyRightAlt   = 100
	KeyLeftMeta   = 125
	KeyRightMeta  = 126
)

// bitBytes is a generous capability bitmap size: the largest bitmap the kernel
// exposes here is KEY_MAX/8+1 = 96 bytes.
const bitBytes = 96

// eventSize is sizeof(struct input_event) on this platform: struct timeval
// (16 bytes on 64-bit Linux) plus __u16 type, __u16 code and __s32 value.
var eventSize = unsafe.Sizeof(inputEvent{})

// inputEvent mirrors struct input_event. unix.Timeval keeps the timeval layout
// matching the host (8 bytes on 32-bit, 16 on 64-bit) so the struct size is
// right without a build tag.
type inputEvent struct {
	Time  unix.Timeval
	Type  uint16
	Code  uint16
	Value int32
}

// Event is one decoded input event.
type Event struct {
	// Time is the kernel timestamp in milliseconds since CLOCK_MONOTONIC
	// start, truncated to 32 bits exactly like Wayland event times.
	Time  uint32
	Type  uint16
	Code  uint16
	Value int32
}

// AbsInfo mirrors struct input_absinfo: the current value and range of one
// absolute axis, used to scale a device's coordinates onto an output.
type AbsInfo struct {
	Value      int32
	Minimum    int32
	Maximum    int32
	Fuzz       int32
	Flat       int32
	Resolution int32
}

type absInfo struct {
	Value      int32
	Minimum    int32
	Maximum    int32
	Fuzz       int32
	Flat       int32
	Resolution int32
}

// Device is one open /dev/input/eventN node.
type Device struct {
	fd   int
	path string
	name string

	keyBits []byte
	relBits []byte
	absBits []byte

	// carry holds a trailing partial event when a read returned a byte count
	// that is not a whole number of input_event structs. The kernel always
	// returns whole events, so this only guards against surprises.
	carry []byte

	grabbed bool
}

// ioctl runs one ioctl on fd, passing arg as the request argument.
func ioctl(fd int, req uintptr, arg unsafe.Pointer) error {
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), req, uintptr(arg))
	if errno != 0 {
		return errno
	}
	return nil
}

// Open opens an evdev node non-blocking and reads its capabilities. The node
// is opened O_RDWR because the kernel rejects EVIOCGRAB (exclusive grab, what
// a bare-metal compositor needs) on a read-only descriptor; machines where
// O_RDWR is not permitted fall back to O_RDONLY and simply cannot grab.
func Open(path string) (*Device, error) {
	fd, err := unix.Open(path, unix.O_RDWR|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	if err != nil {
		fd, err = unix.Open(path, unix.O_RDONLY|unix.O_NONBLOCK|unix.O_CLOEXEC, 0)
	}
	if err != nil {
		return nil, fmt.Errorf("evdev: open %s: %w", path, err)
	}
	d := &Device{fd: fd, path: path}
	if err := d.identify(); err != nil {
		unix.Close(fd)
		return nil, err
	}
	return d, nil
}

func (d *Device) identify() error {
	var name [256]byte
	if err := ioctl(d.fd, reqGetName(uintptr(len(name))), unsafe.Pointer(&name[0])); err != nil {
		// A device without a name is still usable; keep going.
		d.name = ""
	} else {
		d.name = cString(name[:])
	}
	d.keyBits = d.bitmap(EvKey)
	d.relBits = d.bitmap(EvRel)
	d.absBits = d.bitmap(EvAbs)
	return nil
}

// bitmap fetches one EVIOCGBIT bitmap, returning nil when the device has no
// capabilities of that type.
func (d *Device) bitmap(ev uint16) []byte {
	buf := make([]byte, bitBytes)
	if err := ioctl(d.fd, reqGetBit(uintptr(ev), uintptr(len(buf))), unsafe.Pointer(&buf[0])); err != nil {
		return nil
	}
	return buf
}

// FD returns the device file descriptor for polling.
func (d *Device) FD() int { return d.fd }

// Path returns the device node path.
func (d *Device) Path() string { return d.path }

// Name returns the kernel device name, e.g. "AT Translated Set 2 keyboard".
func (d *Device) Name() string { return d.name }

// IsPointer reports whether the device can move a pointer: it advertises
// relative axes (a mouse) or absolute position axes (a touchpad or tablet).
func (d *Device) IsPointer() bool {
	return hasAny(d.relBits, RelX, RelY) || hasAny(d.absBits, AbsX, AbsY, AbsMtPositionX, AbsMtPositionY)
}

// IsKeyboard reports whether the device is a keyboard: it can report the
// letter-key range rather than only buttons.
func (d *Device) IsKeyboard() bool {
	return hasBit(d.keyBits, 30 /* KEY_A */) || hasBit(d.keyBits, 44 /* KEY_Z */) || hasBit(d.keyBits, 57 /* KEY_SPACE */)
}

// IsButton reports whether the device can report pointer buttons.
func (d *Device) IsButton() bool {
	return hasAny(d.keyBits, BtnLeft, BtnRight, BtnMiddle)
}

// IsTouchpad reports whether the device looks like a touchpad rather than a
// mouse or a drawing tablet: multitouch/absolute axes with BTN_TOOL_FINGER
// and no relative axes. Touchpads need gesture interpretation (tap, two-
// finger scroll, kinetic coasting) that Lumine does not implement yet, so
// the backend treats this classification as "skip unless the user opts in":
// mapping the raw ABS_MT position onto the screen would teleport the cursor
// and fire phantom clicks on every touch.
func (d *Device) IsTouchpad() bool {
	if hasAny(d.relBits, RelX, RelY) {
		return false
	}
	hasAbs := hasAny(d.absBits, AbsX, AbsY, AbsMtPositionX, AbsMtPositionY)
	return hasAbs && hasAny(d.keyBits, BtnToolFinger)
}

// HasKey reports whether the device advertises an EV_KEY code.
func (d *Device) HasKey(code uint16) bool { return hasBit(d.keyBits, code) }

// HasRel reports whether the device advertises an EV_REL code.
func (d *Device) HasRel(code uint16) bool { return hasBit(d.relBits, code) }

// HasAbs reports whether the device advertises an EV_ABS code.
func (d *Device) HasAbs(code uint16) bool { return hasBit(d.absBits, code) }

// AbsInfo returns the range of one absolute axis, or false when the device does
// not report it.
func (d *Device) AbsInfo(code uint16) (AbsInfo, bool) {
	if !d.HasAbs(code) {
		return AbsInfo{}, false
	}
	var raw absInfo
	if err := ioctl(d.fd, reqGetAbs(uintptr(code)), unsafe.Pointer(&raw)); err != nil {
		return AbsInfo{}, false
	}
	return AbsInfo{
		Value: raw.Value, Minimum: raw.Minimum, Maximum: raw.Maximum,
		Fuzz: raw.Fuzz, Flat: raw.Flat, Resolution: raw.Resolution,
	}, true
}

// Grab takes exclusive access to the device so no other client (the host
// compositor, the kernel VT) sees its events. It is the caller's job to Grab
// only devices Lumine owns.
func (d *Device) Grab() error {
	v := int32(1)
	if err := ioctl(d.fd, reqGrab(), unsafe.Pointer(&v)); err != nil {
		return fmt.Errorf("evdev: grab %s: %w", d.path, err)
	}
	d.grabbed = true
	return nil
}

// Read drains every complete event currently queued and returns them. It
// returns unix.EAGAIN when the non-blocking fd has nothing pending.
func (d *Device) Read() ([]Event, error) {
	var buf [64 * 24]byte
	n, err := unix.Read(d.fd, buf[:])
	if n <= 0 {
		if err == nil {
			return nil, unix.EAGAIN
		}
		return nil, err
	}
	raw := buf[:n]
	if len(d.carry) > 0 {
		raw = append(append([]byte(nil), d.carry...), raw...)
		d.carry = nil
	}
	whole := len(raw) / int(eventSize)
	if rem := len(raw) % int(eventSize); rem != 0 {
		d.carry = append([]byte(nil), raw[len(raw)-rem:]...)
		raw = raw[:len(raw)-rem]
	}
	out := make([]Event, 0, whole)
	for i := 0; i < whole; i++ {
		out = append(out, decode(raw[i*int(eventSize):]))
	}
	return out, nil
}

// Close releases the device.
func (d *Device) Close() error {
	if d.fd < 0 {
		return nil
	}
	err := unix.Close(d.fd)
	d.fd = -1
	return err
}

// decode reinterprets one complete input_event. Reinterpreting the mirror
// struct (rather than indexing fixed byte offsets) keeps the field offsets
// correct on both 32- and 64-bit hosts, where struct timeval differs.
func decode(b []byte) Event {
	raw := (*inputEvent)(unsafe.Pointer(&b[0]))
	ms := int64(raw.Time.Sec)*1000 + int64(raw.Time.Usec)/1000
	return Event{Time: uint32(ms), Type: raw.Type, Code: raw.Code, Value: raw.Value}
}

// List returns the /dev/input/event* nodes present, ordered by their event
// number so "event2" sorts before "event10".
func List() []string {
	matches, _ := filepath.Glob("/dev/input/event*")
	sort.Slice(matches, func(i, j int) bool {
		a, b := eventNumber(matches[i]), eventNumber(matches[j])
		if a != b {
			return a < b
		}
		return matches[i] < matches[j]
	})
	return matches
}

func eventNumber(path string) int {
	base := filepath.Base(path)
	n, err := strconv.Atoi(strings.TrimPrefix(base, "event"))
	if err != nil {
		return 1 << 30
	}
	return n
}

func hasBit(bits []byte, code uint16) bool {
	i := int(code) / 8
	if i >= len(bits) {
		return false
	}
	return bits[i]&(1<<(uint(code)%8)) != 0
}

func hasAny(bits []byte, codes ...uint16) bool {
	for _, c := range codes {
		if hasBit(bits, c) {
			return true
		}
	}
	return false
}

func cString(b []byte) string {
	for i, c := range b {
		if c == 0 {
			return string(b[:i])
		}
	}
	return string(b)
}
