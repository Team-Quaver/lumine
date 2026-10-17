package main

import (
	"testing"

	"github.com/Team-Quaver/lumine/internal/render"
)

type testRenderer struct{ draws int }

func (*testRenderer) Name() string { return "test" }
func (r *testRenderer) Draw(fb *render.Framebuffer, items []render.DrawItem) error {
	r.draws++
	return nil
}
func (*testRenderer) SetColorSpace(bool, string, string, uint32) {}

func TestOutputRendererResizeAllocatesInitialFramebuffer(t *testing.T) {
	r := &testRenderer{}
	o := &OutputRenderer{renderer: r, width: 800, height: 600}
	o.Resize(800, 600)
	if o.fb == nil {
		t.Fatal("Resize must allocate the framebuffer even when dimensions match the initial size")
	}
	if o.fb.Width != 800 || o.fb.Height != 600 {
		t.Fatalf("framebuffer = %dx%d, want 800x600", o.fb.Width, o.fb.Height)
	}

	presented := false
	o.present = func(fb *render.Framebuffer) error {
		if fb != o.fb {
			t.Fatalf("presented framebuffer %p, want %p", fb, o.fb)
		}
		presented = true
		return nil
	}
	if err := o.Draw(800, 600, nil); err != nil {
		t.Fatal(err)
	}
	if !presented {
		t.Fatal("Draw did not present the framebuffer")
	}
	if r.draws != 1 {
		t.Fatalf("renderer draws = %d, want 1", r.draws)
	}
}
