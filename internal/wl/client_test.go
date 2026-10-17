package wl

import (
	"os"
	"testing"
	"time"
)

type globals struct {
	names map[string]uint32
}

func (g *globals) Global(o *WlRegistry, name uint32, interface_ string, version uint32) {
	g.names[interface_] = name
}
func (g *globals) GlobalRemove(o *WlRegistry, name uint32) {}

// TestDialRealCompositor talks to whatever compositor WAYLAND_DISPLAY points
// at, which exercises the pure-Go client path (request marshalling, event
// dispatch, round trips) against a reference implementation.
func TestDialRealCompositor(t *testing.T) {
	name := os.Getenv("WAYLAND_DISPLAY")
	if name == "" {
		t.Skip("no WAYLAND_DISPLAY")
	}
	c, err := Dial(SocketPath(name))
	if err != nil {
		t.Skipf("dial %s: %v", name, err)
	}
	defer c.Close()
	if err := c.RoundTrip(); err != nil {
		t.Fatalf("initial round trip: %v", err)
	}
	g := &globals{names: map[string]uint32{}}
	reg, err := c.DisplayGetRegistry(g)
	if err != nil {
		t.Fatal(err)
	}
	if err := c.RoundTrip(); err != nil {
		t.Fatalf("registry round trip: %v", err)
	}
	if len(g.names) == 0 {
		t.Fatal("no globals advertised")
	}
	for _, want := range []string{"wl_compositor", "wl_shm", "wl_seat", "xdg_wm_base"} {
		if _, ok := g.names[want]; !ok {
			t.Errorf("global %s missing", want)
		}
	}
	_ = reg

	// Bind wl_compositor and create a surface to exercise object args.
	comp := NewWlCompositor(c, c.AllocID(), 4, nil)
	if err := reg.ReqBind(g.names["wl_compositor"], "wl_compositor", 4, &comp.Object); err != nil {
		t.Fatal(err)
	}
	surf := NewWlSurface(c, c.AllocID(), 4, nil)
	if err := comp.ReqCreateSurface(&surf.Object); err != nil {
		t.Fatal(err)
	}
	if err := c.Flush(); err != nil {
		t.Fatal(err)
	}
	done := make(chan error, 1)
	go func() { done <- c.RoundTrip() }()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("post-surface round trip: %v", err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("round trip timed out")
	}
}
