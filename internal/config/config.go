// Package config loads Lumine's TOML configuration and watches it for
// changes. Hot reload is inotify-based: edits fire the Reload hook, the
// compositor re-reads and re-applies without restarting clients.
package config

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"

	"github.com/BurntSushi/toml"
	"golang.org/x/sys/unix"
)

// Config is the full on-disk configuration.
type Config struct {
	Layout struct {
		Mode        string  `toml:"mode"`         // "tile" | "stack"
		Gap         float64 `toml:"gap"`          // px between windows
		ColumnRatio float64 `toml:"column_ratio"` // column width as fraction of output width
	} `toml:"layout"`

	Input struct {
		XkbLayout string `toml:"xkb_layout"` // advertised keymap name (us layout used)
		Repeat    struct {
			Delay int `toml:"delay"` // ms
			Rate  int `toml:"rate"`  // chars/sec
		} `toml:"repeat"`
	} `toml:"input"`

	Color struct {
		Enabled   bool   `toml:"enabled"`   // expose wp_color_management
		TFName    string `toml:"transfer"`  // "srgb" | "pq" | "hlg"
		Primaries string `toml:"primaries"` // "srgb" | "bt2020" | "dci-p3"
		MaxLuma   uint32 `toml:"max_luma"`  // nits, HDR output
	} `toml:"color"`

	Decor struct {
		Mode string `toml:"mode"` // "auto" | "ssd" | "csd" — server pref, clients may override
	} `toml:"decor"`

	Bindings map[string]string `toml:"bindings"` // "super+left": "focus-left"
}

// Default returns the built-in configuration.
func Default() *Config {
	c := &Config{Bindings: map[string]string{
		"super+left":        "focus-left",
		"super+right":       "focus-right",
		"super+up":          "focus-up",
		"super+down":        "focus-down",
		"super+shift+left":  "scroll-left",
		"super+shift+right": "scroll-right",
		"super+t":           "mode-toggle",
		"super+q":           "close",
		"super+shift+j":     "move-left",
		"super+shift+k":     "move-right",
		"super+return":      "spawn-terminal",
	}}
	c.Layout.Mode = "tile"
	c.Layout.Gap = 8
	c.Layout.ColumnRatio = 0.5
	c.Input.XkbLayout = "us"
	c.Input.Repeat.Delay = 300
	c.Input.Repeat.Rate = 25
	c.Color.Enabled = true
	c.Color.TFName = "srgb"
	c.Color.Primaries = "srgb"
	c.Color.MaxLuma = 400
	c.Decor.Mode = "auto"
	return c
}

// Store owns the active config and its file watcher.
type Store struct {
	mu   sync.Mutex
	path string
	cfg  *Config

	Reload func(cfg *Config)

	ifd   int
	dirW  int // watch descriptor for the config directory
	fileW int // watch descriptor for the config file itself (-1 when unset)
	base  string
}

// Load reads path, falling back to defaults for missing fields.
func Load(path string) (*Config, error) {
	cfg := Default()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return nil, err
	}
	if _, err := toml.Decode(string(data), cfg); err != nil {
		return nil, fmt.Errorf("config: %w", err)
	}
	if cfg.Layout.Mode != "tile" && cfg.Layout.Mode != "stack" {
		cfg.Layout.Mode = "tile"
	}
	return cfg, nil
}

// NewStore loads path and starts watching it.
func NewStore(path string) (*Store, error) {
	cfg, err := Load(path)
	if err != nil {
		return nil, err
	}
	s := &Store{path: path, cfg: cfg, ifd: -1, dirW: -1, fileW: -1}
	if err := s.watch(); err != nil {
		return s, nil // config still usable without hot reload
	}
	return s, nil
}

// watch arms the inotify descriptors. Two watches are needed because editors
// save in two different ways:
//
//   - the directory watch catches create/rename/delete of the config entry, so
//     atomic "write temp + rename over" saves (vim, helix, most editors) are
//     seen;
//   - the file watch catches in-place writes and truncations ("> file",
//     "tee", "echo >>"), which produce no directory event at all.
func (s *Store) watch() error {
	ifd, err := unix.InotifyInit1(unix.IN_CLOEXEC | unix.IN_NONBLOCK)
	if err != nil {
		return err
	}
	abs, _ := filepath.Abs(s.path)
	dir := filepath.Dir(abs)
	base := filepath.Base(abs)
	dirW, err := unix.InotifyAddWatch(ifd, dir, unix.IN_CREATE|unix.IN_MOVED_TO|unix.IN_DELETE)
	if err != nil {
		unix.Close(ifd)
		return err
	}
	s.ifd, s.dirW, s.base = ifd, dirW, base
	s.watchFile()
	return nil
}

// watchFile (re)arms a watch on the config file itself. inotify_add_watch is
// idempotent for a given inode, so calling it again after a rename-based save
// just moves the watch to the new inode. A missing file is not an error: the
// directory watch will arm the file watch once it appears.
func (s *Store) watchFile() {
	if s.ifd < 0 {
		return
	}
	abs, err := filepath.Abs(s.path)
	if err != nil {
		return
	}
	wd, err := unix.InotifyAddWatch(s.ifd, abs,
		unix.IN_MODIFY|unix.IN_CLOSE_WRITE|unix.IN_ATTRIB|unix.IN_DELETE_SELF|unix.IN_MOVE_SELF)
	if err != nil {
		s.fileW = -1
		return
	}
	s.fileW = wd
}

// Config returns the active configuration snapshot.
func (s *Store) Config() *Config {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.cfg
}

// ReloadNow re-reads the file and notifies subscribers.
func (s *Store) ReloadNow() error {
	cfg, err := Load(s.path)
	if err != nil {
		return err
	}
	s.mu.Lock()
	s.cfg = cfg
	s.mu.Unlock()
	if s.Reload != nil {
		s.Reload(cfg)
	}
	return nil
}

// FD exposes the inotify fd for integration into an event loop.
func (s *Store) FD() int { return s.ifd }

// OnReadable consumes inotify events; call when s.FD() is readable. It
// reloads only for events that concern the config file, so unrelated churn in
// the config directory does not trigger spurious reloads.
func (s *Store) OnReadable() {
	buf := make([]byte, 8192)
	reload := false
	for {
		n, err := unix.Read(s.ifd, buf)
		if n <= 0 || err != nil {
			break
		}
		for off := 0; off+unix.SizeofInotifyEvent <= n; {
			wd := int(int32(binary.LittleEndian.Uint32(buf[off:])))
			mask := binary.LittleEndian.Uint32(buf[off+4:])
			nameLen := int(binary.LittleEndian.Uint32(buf[off+12:]))
			var name string
			if nameLen > 0 && off+unix.SizeofInotifyEvent+nameLen <= n {
				raw := buf[off+unix.SizeofInotifyEvent : off+unix.SizeofInotifyEvent+nameLen]
				name = strings.TrimRight(string(raw), "\x00")
			}
			off += unix.SizeofInotifyEvent + nameLen

			switch {
			case wd == s.dirW:
				// Only react to our own entry. A rename-based save also means
				// the file inode changed, so re-arm the file watch.
				if name == s.base {
					reload = true
					s.watchFile()
				}
			case wd == s.fileW:
				if mask&unix.IN_IGNORED != 0 {
					s.fileW = -1
				} else {
					reload = true
				}
			}
		}
		if n < len(buf) {
			break
		}
	}
	if reload {
		_ = s.ReloadNow()
	}
}

// Close stops the watcher.
func (s *Store) Close() {
	if s.ifd >= 0 {
		unix.Close(s.ifd)
		s.ifd = -1
	}
}
