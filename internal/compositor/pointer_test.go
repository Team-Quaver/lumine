package compositor_test

import (
	"path/filepath"
	"testing"

	"github.com/Team-Quaver/lumine/internal/compositor"
	"github.com/Team-Quaver/lumine/internal/config"
	"github.com/Team-Quaver/lumine/internal/wl"
)

// TestPointerStartsCentredAndTracksMotion pins the contract the backends rely
// on to composite the software cursor: a fresh output parks the pointer in the
// middle, motion is relative and the position is clamped to the output.
func TestPointerStartsCentredAndTracksMotion(t *testing.T) {
	dir := t.TempDir()
	t.Setenv("XDG_RUNTIME_DIR", dir)

	store, err := config.NewStore(filepath.Join(dir, "config.conf"))
	if err != nil {
		t.Fatal(err)
	}
	srv, err := wl.NewServer(filepath.Join(dir, "lumine-pointer"))
	if err != nil {
		t.Fatal(err)
	}
	comp := compositor.New(srv, store)
	comp.AddOutput(&compositor.Output{Name: "test-0", W: 800, H: 600, Refresh: 60_000, Renderer: &captureRenderer{}})

	if x, y := comp.Pointer(); x != 400 || y != 300 {
		t.Fatalf("initial pointer = (%d,%d), want the output centre (400,300)", x, y)
	}
	comp.Motion(10, -20)
	if x, y := comp.Pointer(); x != 410 || y != 280 {
		t.Fatalf("pointer after motion = (%d,%d), want (410,280)", x, y)
	}
	comp.MotionAbs(-100, 5000)
	if x, y := comp.Pointer(); x != 0 || y != 599 {
		t.Fatalf("pointer after clamp = (%d,%d), want (0,599)", x, y)
	}
}
