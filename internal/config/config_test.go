package config

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"
)

func TestBindingsMergeWithDefaults(t *testing.T) {
	dir := t.TempDir()
	p := filepath.Join(dir, "c.toml")
	if err := os.WriteFile(p, []byte("[bindings]\n\"super+x\" = \"close\"\n"), 0o644); err != nil {
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
	p := filepath.Join(dir, "c.toml")
	if err := os.WriteFile(p, []byte("[layout]\nmode = \"bogus\"\n"), 0o644); err != nil {
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
	cfg, err := Load(filepath.Join(t.TempDir(), "nope.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.Layout.Gap != 8 || cfg.Layout.Mode != "tile" {
		t.Fatalf("defaults not applied: %+v", cfg.Layout)
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
	p := filepath.Join(dir, "config.toml")
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
	s, ch := newWatchedStore(t, "[layout]\nmode = \"tile\"\n")
	p := s.path
	if err := os.WriteFile(p, []byte("[layout]\nmode = \"stack\"\n"), 0o644); err != nil {
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
	s, ch := newWatchedStore(t, "[layout]\nmode = \"tile\"\n")
	p := s.path
	tmp := p + ".new"
	if err := os.WriteFile(tmp, []byte("[layout]\nmode = \"stack\"\n"), 0o644); err != nil {
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
	if err := os.WriteFile(p, []byte("[layout]\nmode = \"tile\"\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if cfg := waitReload(t, s, ch); cfg.Layout.Mode != "tile" {
		t.Fatalf("after re-edit: mode = %q, want tile", cfg.Layout.Mode)
	}
}
