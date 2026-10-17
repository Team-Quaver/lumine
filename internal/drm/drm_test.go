package drm

import (
	"testing"
	"unsafe"
)

// TestRequestNumbers pins every ioctl request to the value the kernel derives
// from <drm/drm.h>. A mismatch means the Go struct passed to the kernel has
// the wrong size, which would silently corrupt an ioctl argument.
func TestRequestNumbers(t *testing.T) {
	cases := []struct {
		name string
		got  uintptr
		want uintptr
	}{
		{"GET_CAP", reqGetCap, 0xc010640c},
		{"SET_MASTER", reqSetMaster, 0x641e},
		{"DROP_MASTER", reqDropMaster, 0x641f},
		{"GEM_CLOSE", reqGemClose, 0x6409},
		{"PRIME_HANDLE_TO_FD", reqPrimeToFD, 0xc010642d},
		{"PRIME_FD_TO_HANDLE", reqPrimeToHandle, 0xc010642e},
		{"MODE_GETRESOURCES", reqGetResources, 0xc04064a0},
		{"MODE_GETCRTC", reqGetCRTC, 0xc06864a1},
		{"MODE_GETENCODER", reqGetEncoder, 0xc01464a6},
		{"MODE_GETCONNECTOR", reqGetConnector, 0xc05064a7},
		{"MODE_GETPROPERTY", reqGetProperty, 0xc04064aa},
		{"MODE_GETPROPBLOB", reqGetPropertyBl, 0xc01064ac},
		{"MODE_ADDFB", reqAddFB, 0xc01c64ae},
		{"MODE_RMFB", reqRmFB, 0xc00464af},
		{"MODE_PAGE_FLIP", reqPageFlip, 0xc01864b0},
		{"MODE_CREATE_DUMB", reqCreateDumb, 0xc02064b2},
		{"MODE_MAP_DUMB", reqMapDumb, 0xc01064b3},
		{"MODE_DESTROY_DUMB", reqDestroyDumb, 0xc00464b4},
		{"MODE_GETPLANERESOURCES", reqGetPlaneRes, 0xc01064b5},
		{"MODE_GETPLANE", reqGetPlane, 0xc02064b6},
		{"MODE_ADDFB2", reqAddFB2, 0xc06864b8},
		{"MODE_OBJ_GETPROPERTIES", reqObjGetProps, 0xc02064b9},
		{"MODE_OBJ_SETPROPERTY", reqObjSetProp, 0xc01864ba},
		{"MODE_ATOMIC", reqAtomic, 0xc03864bc},
		{"MODE_CREATEPROPBLOB", reqCreatePropBlob, 0xc01064bd},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("%s request = %#x, want %#x", c.name, c.got, c.want)
		}
	}
}

// TestStructSizes checks the size field of every request against the size of
// the Go struct that is passed to the kernel, so the two can never drift.
func TestStructSizes(t *testing.T) {
	cases := []struct {
		name string
		got  uintptr
		want uintptr
	}{
		{"getCap", unsafe.Sizeof(getCap{}), 16},
		{"primeHandle", unsafe.Sizeof(primeHandle{}), 16},
		{"cardRes", unsafe.Sizeof(cardRes{}), 64},
		{"modeInfo", unsafe.Sizeof(modeInfo{}), 68},
		{"crtcInfo", unsafe.Sizeof(crtcInfo{}), 104},
		{"encoderInfo", unsafe.Sizeof(encoderInfo{}), 20},
		{"connectorInfo", unsafe.Sizeof(connectorInfo{}), 80},
		{"propertyInfo", unsafe.Sizeof(propertyInfo{}), 64},
		{"propertyBlob", unsafe.Sizeof(propertyBlob{}), 16},
		{"objGetProps", unsafe.Sizeof(objGetProps{}), 32},
		{"createBlob", unsafe.Sizeof(createBlob{}), 16},
		{"planeRes", unsafe.Sizeof(planeRes{}), 16},
		{"planeInfo", unsafe.Sizeof(planeInfo{}), 32},
		{"createDumb", unsafe.Sizeof(createDumb{}), 32},
		{"mapDumb", unsafe.Sizeof(mapDumb{}), 16},
		{"fbCmd", unsafe.Sizeof(fbCmd{}), 28},
		{"fbCmd2", unsafe.Sizeof(fbCmd2{}), 104},
		{"objSetProp", unsafe.Sizeof(objSetProp{}), 24},
		{"atomicReq", unsafe.Sizeof(atomicReq{}), 56},
		{"pageFlip", unsafe.Sizeof(pageFlip{}), 24},
	}
	for _, c := range cases {
		if c.got != c.want {
			t.Errorf("sizeof(%s) = %d, want %d", c.name, c.got, c.want)
		}
	}
}

// TestIOCEncoding spot-checks the _IOC macro itself against its documented
// bit layout.
func TestIOCEncoding(t *testing.T) {
	if got := ioc(iocNone, 'd', 0x1e, 0); got != 0x641e {
		t.Errorf("io(0x1e) = %#x", got)
	}
	if got := iowr(0x2d, 16); got != 0xc010642d {
		t.Errorf("iowr(0x2d,16) = %#x", got)
	}
}

// TestConnectorName checks the connector-name rendering against the kernel's
// drm_connector_enum_list names.
func TestConnectorName(t *testing.T) {
	cases := []struct {
		typ, id uint32
		want    string
	}{
		{14, 1, "eDP-1"},
		{10, 3, "DP-3"},
		{11, 1, "HDMI-A-1"},
		{1, 1, "VGA-1"},
		{3, 2, "DVI-D-2"},
		{16, 1, "DSI-1"},
	}
	for _, c := range cases {
		got := (&Connector{Type: c.typ, TypeID: c.id}).Name()
		if got != c.want {
			t.Errorf("connector(%d,%d).Name() = %q, want %q", c.typ, c.id, got, c.want)
		}
	}
}

// TestAtomFlatten verifies the parallel-array layout the atomic ioctl needs:
// objects in insertion order, each carrying a contiguous run of properties.
func TestAtomFlatten(t *testing.T) {
	a := NewAtom()
	a.Set(10, 100, 1)
	a.Set(10, 101, 2)
	a.Set(20, 200, 3)
	a.Set(10, 101, 9) // replace, not append
	if a.Len() != 2 {
		t.Fatalf("Len() = %d, want 2", a.Len())
	}
	counts, props, values := a.flatten()
	wantCounts := []uint32{2, 1}
	wantProps := []uint32{100, 101, 200}
	wantValues := []uint64{1, 9, 3}
	if !equalU32(counts, wantCounts) {
		t.Errorf("counts = %v, want %v", counts, wantCounts)
	}
	if !equalU32(props, wantProps) {
		t.Errorf("props = %v, want %v", props, wantProps)
	}
	if !equalU64(values, wantValues) {
		t.Errorf("values = %v, want %v", values, wantValues)
	}
}

func equalU32(a, b []uint32) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func equalU64(a, b []uint64) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// TestSelectMode exercises the resolution/refresh matching rules.
func TestSelectMode(t *testing.T) {
	modes := []Mode{
		{HDisplay: 1920, VDisplay: 1080, VRefresh: 60, Type: ModeTypePreferred},
		{HDisplay: 1920, VDisplay: 1080, VRefresh: 144},
		{HDisplay: 2560, VDisplay: 1440, VRefresh: 60},
	}
	if m, ok := SelectMode(modes, 0, 0, 0); !ok || !m.Preferred() {
		t.Errorf("preferred mode not chosen: %+v ok=%v", m, ok)
	}
	if m, ok := SelectMode(modes, 1920, 1080, 0); !ok || m.VRefresh != 144 {
		t.Errorf("highest refresh for 1080p = %+v", m)
	}
	if m, ok := SelectMode(modes, 1920, 1080, 60000); !ok || m.VRefresh != 60 {
		t.Errorf("60Hz for 1080p = %+v", m)
	}
	if _, ok := SelectMode(modes, 800, 600, 0); ok {
		t.Errorf("unexpected mode match for 800x600")
	}
	if m, ok := SelectMode([]Mode{{HDisplay: 1024, VDisplay: 768, VRefresh: 60}}, 0, 0, 0); !ok || m.HDisplay != 1024 {
		t.Errorf("fallback mode = %+v ok=%v", m, ok)
	}
}
