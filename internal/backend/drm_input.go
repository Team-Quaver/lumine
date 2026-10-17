package backend

import (
	"errors"
	"os"
	"strconv"

	"github.com/Team-Quaver/lumine/internal/evdev"
	"github.com/Team-Quaver/lumine/internal/wl"
	"golang.org/x/sys/unix"
)

// Relative-delta scale for a raw mouse, and the surface-unit distance one wheel
// notch scrolls. Neither applies pointer acceleration: Lumine forwards motion
// verbatim, which keeps the mapping predictable across devices.
const (
	pointerScale = 1.0
	wheelStep    = 15.0
)

// inputDevice is one opened evdev node plus the capability flags the event
// translation needs. The flags are read at open time so the routing logic is
// testable without a real file descriptor.
type inputDevice struct {
	dev  *evdev.Device
	axes map[uint16]evdev.AbsInfo
	dead bool

	keyboardDevice bool
	buttonDevice   bool
	wheelHiRes     bool
}

func newInputDevice(d *evdev.Device) *inputDevice {
	dev := &inputDevice{
		dev:            d,
		axes:           map[uint16]evdev.AbsInfo{},
		keyboardDevice: d.IsKeyboard(),
		buttonDevice:   d.IsButton(),
		wheelHiRes:     d.HasRel(evdev.RelWheelHiRes) || d.HasRel(evdev.RelHWheelHiRes),
	}
	for _, code := range []uint16{evdev.AbsX, evdev.AbsY, evdev.AbsMtPositionX, evdev.AbsMtPositionY} {
		if info, ok := d.AbsInfo(code); ok {
			dev.axes[code] = info
		}
	}
	return dev
}

// isPointerButton reports whether an EV_KEY code in the BTN_* range is a
// clickable mouse button (BTN_LEFT .. BTN_TASK). BTN_TOOL_* and friends sit
// just above this range and describe tool state, not clicks.
func isPointerButton(code uint32) bool {
	return code >= evdev.BtnLeft && code <= evdev.BtnTask
}

// traceInput mirrors raw evdev events to stderr when LUMINE_DEBUG_INPUT is
// set. On a bare VT there is no journal and no other reader, so this is the
// only way to confirm "the kernel sees my mouse" while debugging a session.
func traceInput(d *inputDevice, e evdev.Event) {
	if os.Getenv("LUMINE_DEBUG_INPUT") == "" {
		return
	}
	name := "?"
	if d != nil && d.dev != nil {
		name = d.dev.Name()
	}
	line := "input: " + name +
		" type=" + strconv.Itoa(int(e.Type)) +
		" code=" + strconv.Itoa(int(e.Code)) +
		" value=" + strconv.Itoa(int(e.Value)) +
		" time=" + strconv.FormatUint(uint64(e.Time), 10) + "\n"
	_, _ = unix.Write(2, []byte(line))
}

// drainInput reads every queued event from whichever input device owns fd and
// translates it. A device that reports a hard error is unregistered; the rest
// of the session keeps running.
func (b *DRM) drainInput(fd int) {
	for _, d := range b.inputs {
		if d.dead || d.dev.FD() != fd {
			continue
		}
		events, err := d.dev.Read()
		if err != nil {
			if !errors.Is(err, unix.EAGAIN) {
				d.dead = true
				_ = unix.EpollCtl(b.epfd, unix.EPOLL_CTL_DEL, fd, nil)
			}
			return
		}
		for _, e := range events {
			traceInput(d, e)
			b.handleEvent(d, e)
		}
		return
	}
}

func (b *DRM) handleEvent(d *inputDevice, e evdev.Event) {
	switch e.Type {
	case evdev.EvKey:
		b.handleKey(d, e.Time, e.Code, e.Value != 0)
	case evdev.EvRel:
		b.handleRel(d, e.Code, e.Value)
	case evdev.EvAbs:
		b.handleAbs(d, e.Code, e.Value)
	case evdev.EvSyn:
		switch e.Code {
		case evdev.SynReport:
			b.flushPointer(e.Time)
		case evdev.SynDropped:
			// Events were lost; drop the half-built report rather than applying
			// a bogus delta. The kernel follows SYN_DROPPED with the device's
			// current ABS/KEY state, which reseeds the accumulators normally.
			b.accX, b.accY, b.scrollX, b.scrollY, b.absDirty = 0, 0, 0, 0, false
		}
	}
}

// handleKey routes an EV_KEY event to the keyboard or the pointer. The split
// mirrors the evdev layout (keyboard keys live below BTN_MISC, buttons at or
// above it) with two refinements:
//
//   - only BTN_LEFT..BTN_TASK become wl_pointer.button events. BTN_TOOL_*
//     (finger/pen proximity on touchpads and tablets) would otherwise fire
//     phantom clicks on every touch.
//   - a device that cannot report mouse buttons at all (a plain keyboard, a
//     media remote) keeps its high KEY_* codes as keyboard input, because
//     those codes (KEY_OK = 0x160 …) sit inside the BTN_* range without
//     being buttons.
func (b *DRM) handleKey(d *inputDevice, time uint32, code uint16, pressed bool) {
	if b.in == nil {
		return
	}
	if code >= evdev.BtnMisc {
		onPointerDevice := d == nil || d.buttonDevice
		if onPointerDevice && isPointerButton(uint32(code)) {
			b.in.Button(time, uint32(code), pressed)
			return
		}
		if onPointerDevice {
			return // tool-state or unsupported button code
		}
		// falls through: keyboard-device codes that merely look like buttons
	}
	b.in.Key(time, uint32(code), pressed)
	b.updateMods(code, pressed)
}

// updateMods recomputes the modifier state from the set of held modifier keys
// and hands it to the compositor, which forwards it to the focused client.
func (b *DRM) updateMods(code uint16, pressed bool) {
	switch code {
	case evdev.KeyLeftShift, evdev.KeyRightShift,
		evdev.KeyLeftCtrl, evdev.KeyRightCtrl,
		evdev.KeyLeftAlt, evdev.KeyRightAlt,
		evdev.KeyLeftMeta, evdev.KeyRightMeta:
	default:
		return
	}
	if b.held == nil {
		b.held = map[uint16]bool{}
	}
	if pressed {
		b.held[code] = true
	} else {
		delete(b.held, code)
	}
	b.in.SetMods(wl.Mods{
		Shift: b.held[evdev.KeyLeftShift] || b.held[evdev.KeyRightShift],
		Ctrl:  b.held[evdev.KeyLeftCtrl] || b.held[evdev.KeyRightCtrl],
		Alt:   b.held[evdev.KeyLeftAlt] || b.held[evdev.KeyRightAlt],
		Super: b.held[evdev.KeyLeftMeta] || b.held[evdev.KeyRightMeta],
	})
}

// handleRel accumulates relative motion and wheel steps. Modern mice report
// each notch twice (REL_WHEEL plus a 120-unit REL_WHEEL_HI_RES); honouring
// both would scroll exactly twice as fast, so on hi-res capable devices only
// the fine axis is used.
func (b *DRM) handleRel(d *inputDevice, code uint16, value int32) {
	hiRes := d != nil && d.wheelHiRes
	switch code {
	case evdev.RelX:
		b.accX += float64(value) * pointerScale
	case evdev.RelY:
		b.accY += float64(value) * pointerScale
	case evdev.RelWheel:
		if !hiRes {
			b.scrollY += float64(value) * wheelStep
		}
	case evdev.RelHWheel:
		if !hiRes {
			b.scrollX += float64(value) * wheelStep
		}
	case evdev.RelWheelHiRes:
		b.scrollY += float64(value) / 120.0 * wheelStep
	case evdev.RelHWheelHiRes:
		b.scrollX += float64(value) / 120.0 * wheelStep
	}
}

func (b *DRM) handleAbs(d *inputDevice, code uint16, value int32) {
	info, ok := d.axes[code]
	if !ok || info.Maximum <= info.Minimum {
		return
	}
	switch code {
	case evdev.AbsX, evdev.AbsMtPositionX:
		b.absX = scaleAxis(info, value, b.w)
		b.absDirty = true
	case evdev.AbsY, evdev.AbsMtPositionY:
		b.absY = scaleAxis(info, value, b.h)
		b.absDirty = true
	}
}

// scaleAxis maps a device coordinate onto [0, length) using the axis range the
// kernel reported.
func scaleAxis(info evdev.AbsInfo, value int32, length int) float64 {
	if length <= 0 {
		return 0
	}
	span := float64(info.Maximum - info.Minimum)
	if span <= 0 {
		return 0
	}
	v := float64(value-info.Minimum) / span
	if v < 0 {
		v = 0
	} else if v > 1 {
		v = 1
	}
	return v * float64(length)
}

// flushPointer applies the motion and scroll accumulated since the last
// SYN_REPORT as one batch, so a relative X/Y pair becomes a single move.
func (b *DRM) flushPointer(time uint32) {
	if b.in == nil {
		return
	}
	if b.accX != 0 || b.accY != 0 {
		b.in.Motion(time, b.accX, b.accY)
		b.accX, b.accY = 0, 0
	}
	if b.absDirty {
		b.in.MotionAbs(time, b.absX, b.absY)
		b.absDirty = false
	}
	if b.scrollX != 0 || b.scrollY != 0 {
		b.in.Axis(time, b.scrollX, b.scrollY)
		b.scrollX, b.scrollY = 0, 0
	}
}
