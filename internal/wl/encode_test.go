package wl

import (
	"encoding/binary"
	"testing"
)

// objectMsg is a synthetic message with a single object argument, mirroring
// events such as wl_keyboard.enter or wl_pointer.enter.
func objectMsg() *Message {
	return &Message{Name: "enter", Args: []Arg{{Type: ArgObject, Iface: "wl_surface"}}}
}

func encodeObject(t *testing.T, a any) uint32 {
	t.Helper()
	w, err := encodeArgs("wl_test", objectMsg(), []any{a})
	if err != nil {
		t.Fatalf("encodeArgs(%T): %v", a, err)
	}
	return binary.LittleEndian.Uint32(w.buf)
}

// TestEncodeObjectArgs guards that generated send helpers may pass typed
// resource pointers (e.g. *WlSurface) as object arguments. Missing this made
// wl_keyboard.enter fail to encode, so clients never learned they had focus.
func TestEncodeObjectArgs(t *testing.T) {
	obj := &Object{ID: 42}
	if got := encodeObject(t, obj); got != 42 {
		t.Fatalf("*Object id = %d, want 42", got)
	}

	surf := &WlSurface{}
	surf.ID = 7
	if got := encodeObject(t, surf); got != 7 {
		t.Fatalf("*WlSurface id = %d, want 7", got)
	}

	if got := encodeObject(t, (*WlSurface)(nil)); got != 0 {
		t.Fatalf("typed nil *WlSurface id = %d, want 0", got)
	}
	if got := encodeObject(t, nil); got != 0 {
		t.Fatalf("nil id = %d, want 0", got)
	}
	if got := encodeObject(t, uint32(9)); got != 9 {
		t.Fatalf("uint32 id = %d, want 9", got)
	}

	if _, err := encodeArgs("wl_test", objectMsg(), []any{struct{}{}}); err == nil {
		t.Fatal("non-object argument should be rejected")
	}
}
