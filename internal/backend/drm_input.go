package backend

import (
	"errors"

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

// inputDevice is one opened evdev node plus the absolute-axis ranges needed to
// scale its coordinates onto the output.
type inputDevice struct {
	dev  *evdev.Device
	axes map[uint16]evdev.AbsInfo
	dead bool
}

func newInputDevice(d *evdev.Device) *inputDevice {
	dev := &inputDevice{dev: d, axes: map[uint16]evdev.AbsInfo{}}
	for _, code := range []uint16{evdev.AbsX, evdev.AbsY, evdev.AbsMtPositionX, evdev.AbsMtPositionY} {
		if info, ok := d.AbsInfo(code); ok {
			dev.axes[code] = info
		}
	}
	return dev
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
			b.handleEvent(d, e)
		}
		return
	}
}

func (b *DRM) handleEvent(d *inputDevice, e evdev.Event) {
	switch e.Type {
	case evdev.EvKey:
		b.handleKey(d, e.Code, e.Value != 0)
	case evdev.EvRel:
		b.handleRel(e.Code, e.Value)
	case evdev.EvAbs:
		b.handleAbs(d, e.Code, e.Value)
	case evdev.EvSyn:
		switch e.Code {
		case evdev.SynReport:
			b.flushPointer()
		case evdev.SynDropped:
			// Events were lost; drop the half-built report rather than applying
			// a bogus delta.
			b.accX, b.accY, b.scrollX, b.scrollY, b.absDirty = 0, 0, 0, 0, false
		}
	}
}

// handleKey routes an EV_KEY event to the keyboard or the pointer, mirroring the
// evdev split: keyboard keys live below BTN_MISC (0x100), everything above is a
// button. Modifier keys additionally refresh the compositor's modifier state.
func (b *DRM) handleKey(d *inputDevice, code uint16, pressed bool) {
	if b.in == nil {
		return
	}
	if code >= evdev.BtnMisc {
		b.in.Button(uint32(code), pressed)
		return
	}
	b.in.Key(uint32(code), pressed)
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

func (b *DRM) handleRel(code uint16, value int32) {
	switch code {
	case evdev.RelX:
		b.accX += float64(value) * pointerScale
	case evdev.RelY:
		b.accY += float64(value) * pointerScale
	case evdev.RelWheel:
		b.scrollY += float64(value) * wheelStep
	case evdev.RelHWheel:
		b.scrollX += float64(value) * wheelStep
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
func (b *DRM) flushPointer() {
	if b.in == nil {
		return
	}
	if b.accX != 0 || b.accY != 0 {
		b.in.Motion(b.accX, b.accY)
		b.accX, b.accY = 0, 0
	}
	if b.absDirty {
		b.in.MotionAbs(b.absX, b.absY)
		b.absDirty = false
	}
	if b.scrollX != 0 || b.scrollY != 0 {
		b.in.Axis(b.scrollX, b.scrollY)
		b.scrollX, b.scrollY = 0, 0
	}
}
