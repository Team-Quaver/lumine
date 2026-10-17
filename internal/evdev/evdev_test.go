package evdev

import (
	"encoding/binary"
	"sort"
	"testing"
	"unsafe"
)

// TestRequestNumbers pins the EVIOC* requests to the values the kernel derives
// from <linux/input.h>. A mismatch means the size field in the _IOC encoding no
// longer matches the Go struct we hand the kernel, which would make the ioctl
// read or write the wrong number of bytes.
func TestRequestNumbers(t *testing.T) {
	if unsafe.Sizeof(absInfo{}) != 24 {
		t.Fatalf("sizeof(input_absinfo) = %d, want 24", unsafe.Sizeof(absInfo{}))
	}
	cases := []struct {
		name string
		got  uintptr
		want uintptr
	}{
		{"EVIOCGNAME(256)", reqGetName(256), 0x81004506},
		{"EVIOCGBIT(EV_KEY,96)", reqGetBit(EvKey, 96), 0x80604521},
		{"EVIOCGABS(ABS_X)", reqGetAbs(AbsX), 0x80184540},
		{"EVIOCGRAB", reqGrab(), 0x40044590},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s = %#x, want %#x", c.name, c.got, c.want)
		}
	}
}

// TestEventSize checks the decoded struct size: timeval plus type, code and
// value. timeval is 8 bytes on 32-bit and 16 on 64-bit, so the event is 16 or
// 24 bytes respectively.
func TestEventSize(t *testing.T) {
	want := unsafe.Sizeof(tv{}) + 8
	if eventSize != want {
		t.Errorf("eventSize = %d, want %d", eventSize, want)
	}
}

// tv aliases unix.Timeval's layout without importing the package here; the
// size is what matters.
type tv struct {
	Sec  int64
	Usec int64
}

// TestDecode pins the field offsets decode reads: type, code and value sit at
// the tail of input_event, after struct timeval.
func TestDecode(t *testing.T) {
	size := int(eventSize)
	b := make([]byte, size)
	binary.LittleEndian.PutUint16(b[size-8:], EvKey)
	binary.LittleEndian.PutUint16(b[size-6:], KeyLeftShift)
	binary.LittleEndian.PutUint32(b[size-4:], 1)

	got := decode(b)
	if got.Type != EvKey || got.Code != KeyLeftShift || got.Value != 1 {
		t.Fatalf("decode = %+v, want {EvKey %d 1}", got, KeyLeftShift)
	}
}

// TestDecodeRoundTrip writes through the mirror struct and reads back through
// decode to prove both use the same layout.
func TestDecodeRoundTrip(t *testing.T) {
	b := make([]byte, int(eventSize))
	raw := (*inputEvent)(unsafe.Pointer(&b[0]))
	raw.Type = EvRel
	raw.Code = RelWheel
	raw.Value = -3

	got := decode(b)
	if got.Type != EvRel || got.Code != RelWheel || got.Value != -3 {
		t.Fatalf("decode round trip = %+v", got)
	}
}

// TestHasBit exercises the capability bitmap lookup, including the bit that
// crosses a byte boundary and the out-of-range guard.
func TestHasBit(t *testing.T) {
	bits := []byte{0b0000_0001, 0b0000_1000} // bit 0 and bit 11 set
	cases := []struct {
		code uint16
		want bool
	}{
		{0, true},
		{1, false},
		{7, false},
		{8, false},
		{11, true},
		{15, false},
		{16, false}, // past the bitmap
	}
	for _, c := range cases {
		if got := hasBit(bits, c.code); got != c.want {
			t.Errorf("hasBit(bit %d) = %v, want %v", c.code, got, c.want)
		}
	}
	if hasBit(nil, 0) {
		t.Errorf("hasBit(nil, 0) = true, want false")
	}
}

func TestHasAny(t *testing.T) {
	bits := []byte{0b0000_0010} // only bit 1 set
	if !hasAny(bits, 5, 1) {
		t.Errorf("hasAny should match bit 1")
	}
	if hasAny(bits, 2, 3) {
		t.Errorf("hasAny matched unset bits")
	}
	if hasAny(nil, AbsX, AbsY) {
		t.Errorf("hasAny(nil) = true, want false")
	}
}

// TestIsPointerAndKeyboard checks the routing predicates against synthetic
// capability bitmaps, since they never touch the file descriptor.
func TestIsPointerAndKeyboard(t *testing.T) {
	mouse := &Device{relBits: bits(RelX, RelY), keyBits: bits(BtnLeft, BtnRight)}
	if !mouse.IsPointer() {
		t.Errorf("mouse should be a pointer")
	}
	if mouse.IsKeyboard() {
		t.Errorf("mouse should not be a keyboard")
	}
	if !mouse.IsButton() {
		t.Errorf("mouse should report buttons")
	}

	touchpad := &Device{absBits: bits(AbsX, AbsY), keyBits: bits(BtnLeft)}
	if !touchpad.IsPointer() {
		t.Errorf("touchpad should be a pointer")
	}
	if !touchpad.HasAbs(AbsX) || touchpad.HasAbs(AbsMtPositionX) {
		t.Errorf("touchpad abs bitmap wrong")
	}

	keyboard := &Device{keyBits: bits(30 /* KEY_A */, KeyLeftShift)}
	if !keyboard.IsKeyboard() {
		t.Errorf("keyboard should be a keyboard")
	}
	if keyboard.IsPointer() {
		t.Errorf("keyboard should not be a pointer")
	}
}

// bits builds a capability bitmap with the named codes set, sized so even the
// highest BTN_* code fits.
func bits(codes ...uint16) []byte {
	n := 8
	for _, c := range codes {
		if want := int(c)/8 + 1; want > n {
			n = want
		}
	}
	b := make([]byte, n)
	for _, c := range codes {
		b[int(c)/8] |= 1 << (uint(c) % 8)
	}
	return b
}

// TestEventNumber pins the numeric extraction used to sort nodes.
func TestEventNumber(t *testing.T) {
	cases := []struct {
		path string
		want int
	}{
		{"/dev/input/event0", 0},
		{"/dev/input/event2", 2},
		{"/dev/input/event10", 10},
		{"event7", 7},
		{"/dev/input/not-an-event", 1 << 30},
	}
	for _, c := range cases {
		if got := eventNumber(c.path); got != c.want {
			t.Errorf("eventNumber(%q) = %d, want %d", c.path, got, c.want)
		}
	}
}

// TestEventOrdering checks the sort predicate puts event2 before event10 (a
// plain string sort would not).
func TestEventOrdering(t *testing.T) {
	nodes := []string{"event10", "event2", "event1", "event20", "event3"}
	sort.Slice(nodes, func(i, j int) bool {
		a, b := eventNumber(nodes[i]), eventNumber(nodes[j])
		if a != b {
			return a < b
		}
		return nodes[i] < nodes[j]
	})
	want := []string{"event1", "event2", "event3", "event10", "event20"}
	for i := range want {
		if nodes[i] != want[i] {
			t.Fatalf("sorted = %v, want %v", nodes, want)
		}
	}
}

// TestListSorted checks the real List output is ordered as documented whenever
// the host has input nodes (skipped otherwise).
func TestListSorted(t *testing.T) {
	nodes := List()
	if len(nodes) < 2 {
		t.Skipf("not enough /dev/input/event* nodes to check ordering")
	}
	for i := 1; i < len(nodes); i++ {
		a, b := eventNumber(nodes[i-1]), eventNumber(nodes[i])
		if a > b || (a == b && nodes[i-1] > nodes[i]) {
			t.Fatalf("List not sorted: %s before %s", nodes[i-1], nodes[i])
		}
	}
}

func TestCString(t *testing.T) {
	if got := cString([]byte{'a', 'b', 0, 'c'}); got != "ab" {
		t.Errorf("cString = %q, want %q", got, "ab")
	}
	if got := cString([]byte("abc")); got != "abc" {
		t.Errorf("cString(NUL-free) = %q, want %q", got, "abc")
	}
	if got := cString([]byte{0}); got != "" {
		t.Errorf("cString(empty) = %q, want %q", got, "")
	}
}
