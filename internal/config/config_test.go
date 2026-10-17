package config

import (
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

// TestBindingsMergeWithDefaults checks a [keybinding] section is merged on top
// of the built-in defaults rather than replacing them.
func TestBindingsMergeWithDefaults(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.conf")
	if err := os.WriteFile(p, []byte("[keybinding]\n\"super+x\" = \"close\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Bindings["super+left"] != "focus-left" {
		t.Fatalf("default binding lost: %v", cfg.Bindings)
	}
	if cfg.Bindings["super+x"] != "close" {
		t.Fatalf("custom binding missing: %v", cfg.Bindings)
	}
}

// TestLayoutModeFallback checks an invalid mode falls back to tile.
func TestLayoutModeFallback(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "config.conf")
	if err := os.WriteFile(p, []byte("[layout]\nmode = bogus\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	cfg, err := Load(p)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Layout.Mode != "tile" {
		t.Fatalf("mode = %q, want tile", cfg.Layout.Mode)
	}
}

// TestMissingFileUsesDefaults checks a missing config still yields a usable
// default configuration (hot reload is optional).
func TestMissingFileUsesDefaults(t *testing.T) {
	cfg, err := Load(filepath.Join(t.TempDir(), "nope.conf"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Layout.Gap != 8 || cfg.Layout.Mode != "tile" {
		t.Fatalf("defaults not applied: %+v", cfg.Layout)
	}
}

// TestDisplaySection exercises the [Display] mapping: connector, resolution,
// refresh rate, color management, HDR and triple buffering.
func TestDisplaySection(t *testing.T) {
	body := `
[Display] # the primary output
monitor = e-DP1
resolution = 2560x1440
refresh-Rate = highest
color-managment = icc
icc-file = "/somewhere/panel.icc"
main-monitor = ""
hdr = "false"
triple-buffering = true
`
	cfg := loadString(t, body)
	if len(cfg.Displays) != 1 {
		t.Fatalf("displays = %d, want 1", len(cfg.Displays))
	}
	d := cfg.Displays[0]
	if d.Monitor != "e-DP1" {
		t.Errorf("monitor = %q", d.Monitor)
	}
	if d.Resolution != "2560x1440" || d.Width != 2560 || d.Height != 1440 {
		t.Errorf("resolution = %q (%dx%d)", d.Resolution, d.Width, d.Height)
	}
	if d.RefreshMHz != 0 || d.RefreshRate != "highest" {
		t.Errorf("refresh = %q / %d, want highest / 0", d.RefreshRate, d.RefreshMHz)
	}
	if d.ColorManagement != ColorManagementICC {
		t.Errorf("color-management = %q", d.ColorManagement)
	}
	if d.ICCFile != "/somewhere/panel.icc" {
		t.Errorf("icc-file = %q", d.ICCFile)
	}
	if d.HDREnabled {
		t.Errorf("hdr should be off")
	}
	if !d.TripleBuffering {
		t.Errorf("triple-buffering should be on")
	}
	if cfg.PrimaryDisplay() != &cfg.Displays[0] {
		t.Errorf("first display should be the implicit main output")
	}
}

// TestDisplayPriority verifies that Display-1 / Display-2 keep file order and
// that an explicit main-monitor overrides the implicit first-wins rule.
func TestDisplayPriority(t *testing.T) {
	body := `
[Display-1]
monitor = HDMI-A-1
resolution = 3840x2160
refresh-rate = 59.94

[Display-2]
monitor = e-DP1
resolution = 1920x1080
main-monitor = true
`
	cfg := loadString(t, body)
	if len(cfg.Displays) != 2 {
		t.Fatalf("displays = %d, want 2", len(cfg.Displays))
	}
	if cfg.Displays[0].Monitor != "HDMI-A-1" || cfg.Displays[1].Monitor != "e-DP1" {
		t.Fatalf("order lost: %+v", cfg.Displays)
	}
	if got := cfg.Displays[0].RefreshMHz; got != 59940 {
		t.Errorf("59.94Hz = %d mHz, want 59940", got)
	}
	if cfg.PrimaryDisplay() != &cfg.Displays[1] {
		t.Errorf("main-monitor = true should win over file order")
	}
	if cfg.DisplayFor("e-DP1") != &cfg.Displays[1] {
		t.Errorf("DisplayFor did not match by connector name")
	}
}

// TestAutoStartAndEnvironment covers the repeated exec-once keys and the
// [wm-environment] table.
func TestAutoStartAndEnvironment(t *testing.T) {
	body := `
[auto-start]
exec-once = fcitx5 -d
exec-once = "/usr/bin/fastfetch"
# other lines are ignored

[wm-environment]
GTK_THEME = Breeze
QT_SCALE_FACTOR = 1.25
`
	cfg := loadString(t, body)
	want := []string{"fcitx5 -d", "/usr/bin/fastfetch"}
	if len(cfg.AutoStart) != len(want) {
		t.Fatalf("auto-start = %v, want %v", cfg.AutoStart, want)
	}
	for i := range want {
		if cfg.AutoStart[i] != want[i] {
			t.Fatalf("auto-start[%d] = %q, want %q", i, cfg.AutoStart[i], want[i])
		}
	}
	env := strings.Join(cfg.Environment, " ")
	if !strings.Contains(env, "GTK_THEME=Breeze") || !strings.Contains(env, "QT_SCALE_FACTOR=1.25") {
		t.Fatalf("environment = %v", cfg.Environment)
	}
}

// TestCommentsAndTypographicQuotes checks the forgiving value handling that
// makes copy-pasted examples work.
func TestCommentsAndTypographicQuotes(t *testing.T) {
	body := "[Display]\n" +
		"monitor = \u201ce-DP1\u201d ; trailing comment\n" +
		"icc-file = \"/x#y.icc\" # the '#' is inside quotes\n"
	cfg := loadString(t, body)
	d := cfg.Displays[0]
	if d.Monitor != "e-DP1" {
		t.Errorf("monitor = %q, want e-DP1", d.Monitor)
	}
	if d.ICCFile != "/x#y.icc" {
		t.Errorf("icc-file = %q", d.ICCFile)
	}
}

// TestUnknownSectionIsAnError keeps typos loud instead of silent.
func TestUnknownSectionIsAnError(t *testing.T) {
	_, err := Load(writeString(t, "[displays]\nmonitor = e-DP1\n"))
	if err == nil {
		t.Fatal("expected an error for an unknown section")
	}
}

// TestBadResolutionIsAnError checks malformed values are reported.
func TestBadResolutionIsAnError(t *testing.T) {
	_, err := Load(writeString(t, "[Display]\nresolution = 1920\n"))
	if err == nil {
		t.Fatal("expected an error for a bad resolution")
	}
}

// TestExampleConfigParses keeps the shipped example in sync with the parser:
// a doc that no longer loads is a bug users would hit first.
func TestExampleConfigParses(t *testing.T) {
	cfg, err := Load("../../config.example.conf")
	if err != nil {
		t.Fatalf("config.example.conf: %v", err)
	}
	if len(cfg.Displays) != 1 || cfg.Displays[0].Monitor != "eDP-1" {
		t.Fatalf("example display = %+v", cfg.Displays)
	}
	if len(cfg.AutoStart) != 2 {
		t.Fatalf("example auto-start = %v", cfg.AutoStart)
	}
	if cfg.Displays[0].Width != 2560 || cfg.Displays[0].Height != 1440 {
		t.Fatalf("example resolution = %dx%d", cfg.Displays[0].Width, cfg.Displays[0].Height)
	}
	if cfg.Bindings["super+t"] != "mode-toggle" {
		t.Fatalf("example keybinding not merged: %v", cfg.Bindings)
	}
}

// TestDefaultPathHonoursXDGConfigHome pins the file location.
func TestDefaultPathHonoursXDGConfigHome(t *testing.T) {
	t.Setenv("XDG_CONFIG_HOME", "/tmp/xdg")
	if got, want := DefaultPath(), "/tmp/xdg/lumine-wm/config.conf"; got != want {
		t.Fatalf("DefaultPath = %q, want %q", got, want)
	}
}

// waitReload polls the store's inotify fd until the reload hook fires.
func waitReload(t *testing.T, s *Store, ch chan *Config) *Config {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	fds := []unix.PollFd{{Fd: int32(s.FD()), Events: unix.POLLIN}}
	for {
		select {
		case c := <-ch:
			return c
		default:
		}
		ms := int(time.Until(deadline).Milliseconds())
		if ms <= 0 {
			t.Fatal("timed out waiting for config reload")
		}
		if ms > 200 {
			ms = 200
		}
		n, err := unix.Poll(fds, ms)
		if err != nil && err != syscall.EINTR {
			t.Fatalf("poll: %v", err)
		}
		if n > 0 {
			s.OnReadable()
		}
	}
}

func newWatchedStore(t *testing.T, body string) (*Store, chan *Config) {
	t.Helper()
	dir := t.TempDir()
	p := filepath.Join(dir, "config.conf")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	s, err := NewStore(p)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(s.Close)
	if s.FD() < 0 {
		t.Skip("inotify unavailable")
	}
	ch := make(chan *Config, 4)
	s.Reload = func(c *Config) { ch <- c }
	return s, ch
}

// TestHotReloadInPlaceEdit verifies an in-place rewrite (same inode, no
// rename) triggers a reload. A directory-only watch would miss this.
func TestHotReloadInPlaceEdit(t *testing.T) {
	s, ch := newWatchedStore(t, "[layout]\nmode = tile\n")
	p := s.Path()
	if err := os.WriteFile(p, []byte("[layout]\nmode = stack\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if cfg := waitReload(t, s, ch); cfg.Layout.Mode != "stack" {
		t.Fatalf("mode = %q, want stack", cfg.Layout.Mode)
	}
}

// TestHotReloadAtomicRename covers the write-temp-then-rename save that most
// editors use, and checks the file watch is re-armed on the new inode so a
// later in-place edit still reloads.
func TestHotReloadAtomicRename(t *testing.T) {
	s, ch := newWatchedStore(t, "[layout]\nmode = tile\n")
	p := s.Path()
	tmp := p + ".new"
	if err := os.WriteFile(tmp, []byte("[layout]\nmode = stack\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(tmp, p); err != nil {
		t.Fatal(err)
	}
	if cfg := waitReload(t, s, ch); cfg.Layout.Mode != "stack" {
		t.Fatalf("after rename: mode = %q, want stack", cfg.Layout.Mode)
	}
	// A subsequent in-place edit must also fire: proves watchFile followed the
	// rename to the new inode.
	if err := os.WriteFile(p, []byte("[layout]\nmode = tile\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if cfg := waitReload(t, s, ch); cfg.Layout.Mode != "tile" {
		t.Fatalf("after re-edit: mode = %q, want tile", cfg.Layout.Mode)
	}
}

func loadString(t *testing.T, body string) *Config {
	t.Helper()
	cfg, err := Load(writeString(t, body))
	if err != nil {
		t.Fatal(err)
	}
	return cfg
}

func writeString(t *testing.T, body string) string {
	t.Helper()
	p := filepath.Join(t.TempDir(), "config.conf")
	if err := os.WriteFile(p, []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
	return p
}
