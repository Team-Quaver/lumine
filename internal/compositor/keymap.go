package compositor

import (
	"fmt"
	"os"

	"github.com/Team-Quaver/lumine/internal/wl"
	"golang.org/x/sys/unix"
)

func dbg(format string, args ...any) {
	if os.Getenv("LUMINE_DEBUG_KEYMAP") != "" {
		fmt.Fprintf(os.Stderr, "DEBUG "+format+"\n", args...)
	}
}

// keymapFormatXKBV1 is wl_keyboard.keymap_format.xkb_v1.
const keymapFormatXKBV1 = 1

// keymapText is the keymap handed to every client. It is written in the XKB
// text format (XKB_KEYMAP_FORMAT_TEXT_V1) and uses the standard include
// directives, so libxkbcommon expands it against the system xkb data. Lumine
// forwards evdev keycodes unchanged, which is exactly what evdev+aliases
// keycodes describe.
const keymapText = `xkb_keymap {
	xkb_keycodes  { include "evdev+aliases(qwerty)"	};
	xkb_types     { include "complete"	};
	xkb_compat    { include "complete"	};
	xkb_symbols   { include "pc+us+inet(evdev)"	};
	xkb_geometry  { include "pc(pc105)"	};
};
`

// sendKeymap creates a sealed memfd holding keymapText and passes it to the
// client through wl_keyboard.keymap. The fd is duplicated by the socket send;
// the compositor closes its own copy immediately.
func sendKeymap(kb *wl.WlKeyboard) {
	if kb == nil {
		return
	}
	data := []byte(keymapText)
	fd, err := unix.MemfdCreate("lumine-keymap", unix.MFD_CLOEXEC|unix.MFD_ALLOW_SEALING)
	if err != nil {
		dbg("memfd: %v", err)
		return
	}
	defer unix.Close(fd)
	if err := unix.Ftruncate(fd, int64(len(data))); err != nil {
		dbg("ftruncate: %v", err)
		return
	}
	if _, err := unix.Pwrite(fd, data, 0); err != nil {
		dbg("pwrite: %v", err)
		return
	}
	_, _ = unix.FcntlInt(uintptr(fd), unix.F_ADD_SEALS, unix.F_SEAL_SHRINK|unix.F_SEAL_GROW|unix.F_SEAL_WRITE)
	dbg("kd=%p conn=%p fd=%d len=%d", kb, kb.Object.Conn, fd, len(data))
	kb.Keymap(keymapFormatXKBV1, fd, uint32(len(data)))
}
