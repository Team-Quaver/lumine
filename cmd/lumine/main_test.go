package main

import (
	"os"
	"testing"

	"github.com/Team-Quaver/lumine/internal/config"
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

func TestApplyEnvironmentSetsVariables(t *testing.T) {
	env := []string{"LUMINE_TEST_THEME=Adwaita", "LUMINE_TEST_SCALE=2"}
	t.Cleanup(func() {
		os.Unsetenv("LUMINE_TEST_THEME")
		os.Unsetenv("LUMINE_TEST_SCALE")
	})
	applyEnvironment(env)
	if got := os.Getenv("LUMINE_TEST_THEME"); got != "Adwaita" {
		t.Errorf("LUMINE_TEST_THEME = %q, want Adwaita", got)
	}
	if got := os.Getenv("LUMINE_TEST_SCALE"); got != "2" {
		t.Errorf("LUMINE_TEST_SCALE = %q, want 2", got)
	}
}

func TestOutputNameAndRefreshFallBackToDefaults(t *testing.T) {
	if got := outputName(nil); got != "lumine-nested" {
		t.Errorf("outputName(nil) = %q", got)
	}
	if got := refreshMHz(nil); got != 60000 {
		t.Errorf("refreshMHz(nil) = %d, want 60000", got)
	}

	d := &config.Display{Monitor: "e-DP1", RefreshRate: "highest"}
	if got := outputName(d); got != "e-DP1" {
		t.Errorf("outputName = %q, want e-DP1", got)
	}
	if got := refreshMHz(d); got != 60000 {
		t.Errorf("refreshMHz(highest) = %d, want 60000", got)
	}
	d.RefreshMHz = 144000
	if got := refreshMHz(d); got != 144000 {
		t.Errorf("refreshMHz(144Hz) = %d, want 144000", got)
	}
}

func TestFoldDisplayHDR(t *testing.T) {
	cfg := config.Default()
	cfg.Displays = []config.Display{{Section: "Display", HDREnabled: true}}
	foldDisplayHDR(cfg)
	if !cfg.Color.Enabled || cfg.Color.TFName != "pq" || cfg.Color.Primaries != "bt2020" {
		t.Fatalf("HDR mapping = enabled=%v tf=%q primaries=%q", cfg.Color.Enabled, cfg.Color.TFName, cfg.Color.Primaries)
	}

	// An explicit [color] choice wins over the display default.
	cfg2 := config.Default()
	cfg2.Color.TFName = "hlg"
	cfg2.Color.Primaries = "dci-p3"
	cfg2.Displays = []config.Display{{Section: "Display", HDREnabled: true}}
	foldDisplayHDR(cfg2)
	if cfg2.Color.TFName != "hlg" || cfg2.Color.Primaries != "dci-p3" {
		t.Fatalf("explicit color overridden: tf=%q primaries=%q", cfg2.Color.TFName, cfg2.Color.Primaries)
	}

	// No display / HDR off leaves color untouched.
	cfg3 := config.Default()
	cfg3.Displays = []config.Display{{Section: "Display"}}
	foldDisplayHDR(cfg3)
	if cfg3.Color.TFName != "srgb" || cfg3.Color.Primaries != "srgb" {
		t.Fatalf("non-HDR display changed color: tf=%q primaries=%q", cfg3.Color.TFName, cfg3.Color.Primaries)
	}
}
