// Package config loads Lumine's window-manager configuration and watches it
// for changes. The on-disk format is an INI-like ".conf" file, normally at
// $XDG_CONFIG_HOME/lumine-wm/config.conf. Hot reload is inotify-based: edits
// fire the Reload hook and the compositor re-reads and re-applies the new
// values without restarting clients.
package config

import (
	"encoding/binary"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"

	"golang.org/x/sys/unix"
)

// ColorManagement is one of the [Display].color-management values.
type ColorManagement string

const (
	ColorManagementNone ColorManagement = "none" // no profile
	ColorManagementEDID ColorManagement = "edid" // use the monitor's EDID
	ColorManagementICC  ColorManagement = "icc"  // use icc-file
)

// Display describes one output, written as a [Display] / [Display-1] /
// [Display-2] section. Sections are kept in file order, which is also the
// priority order: the earlier a section appears, the higher its priority, and
// the first one is the main output unless another is marked with main-monitor.
type Display struct {
	Section string // section name, e.g. "Display" or "Display-1"

	Monitor    string // connector/output name, e.g. eDP-1 ("" = any)
	Resolution string // "1920x1080" or "" for the backend default
	Width      int    // parsed from Resolution, 0 when unset
	Height     int    // parsed from Resolution, 0 when unset

	// RefreshRate keeps the raw text ("highest" or a number); RefreshMHz is
	// the parsed value in mHz (0 means "let the backend decide the highest").
	RefreshRate string
	RefreshMHz  int32

	ColorManagement ColorManagement
	ICCFile         string

	// MainMonitor is the raw main-monitor value. Main is true when this
	// display is explicitly the primary one (main-monitor = true, or its
	// monitor name matches the main-monitor string).
	MainMonitor string
	Main        bool

	HDR             string // raw value: "", "true"/"on", "false"/"off"
	HDREnabled      bool
	TripleBuffering bool
}

// Config is the full on-disk configuration.
type Config struct {
	Layout struct {
		Mode        string  // "tile" | "stack"
		Gap         float64 // px between windows
		ColumnRatio float64 // column width as a fraction of the output width
	}

	Input struct {
		XkbLayout string // advertised keymap layout name (us layout used)
		Repeat    struct {
			Delay int // ms
			Rate  int // chars/sec
		}
	}

	Color struct {
		Enabled   bool   // expose wp_color_management
		TFName    string // "srgb" | "pq" | "hlg"
		Primaries string // "srgb" | "bt2020" | "dci-p3"
		MaxLuma   uint32 // nits, HDR output
	}

	Decor struct {
		Mode string // "auto" | "ssd" | "csd" — server pref, clients may override
	}

	Bindings map[string]string // "super+left": "focus-left"

	// Displays are the [Display*] sections in priority order.
	Displays []Display

	// AutoStart is the list of exec-once commands from [auto-start].
	AutoStart []string

	// Environment is the [wm-environment] table flattened into ordered
	// "KEY=VALUE" entries, applied to the compositor and everything it spawns.
	Environment []string
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

// DefaultPath returns the config path Lumine uses when none is given:
// $XDG_CONFIG_HOME/lumine-wm/config.conf, falling back to ~/.config.
func DefaultPath() string {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "lumine-wm.conf"
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "lumine-wm", "config.conf")
}

// PrimaryDisplay returns the display that should be treated as the main
// output: the one explicitly marked main, otherwise the first section (which,
// by the priority rule, is the highest-priority one). It returns nil when no
// display is configured.
func (c *Config) PrimaryDisplay() *Display {
	for i := range c.Displays {
		if c.Displays[i].Main {
			return &c.Displays[i]
		}
	}
	if len(c.Displays) > 0 {
		return &c.Displays[0]
	}
	return nil
}

// DisplayFor returns the display section configured for the given monitor
// name, or nil when none matches.
func (c *Config) DisplayFor(monitor string) *Display {
	for i := range c.Displays {
		if c.Displays[i].Monitor != "" && strings.EqualFold(c.Displays[i].Monitor, monitor) {
			return &c.Displays[i]
		}
	}
	return nil
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

// Path returns the file the store is watching.
func (s *Store) Path() string { return s.path }

// Load reads path, falling back to defaults for missing fields. A missing file
// is not an error: it means "all defaults".
func Load(path string) (*Config, error) {
	cfg := Default()
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return cfg, nil
		}
		return nil, err
	}
	doc, err := parseINI(string(data))
	if err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	if err := cfg.apply(doc); err != nil {
		return nil, fmt.Errorf("config %s: %w", path, err)
	}
	cfg.normalize()
	return cfg, nil
}

// apply folds a parsed document into cfg. Only the sections and keys that are
// present are touched, so unspecified values keep their defaults.
func (c *Config) apply(doc *ini) error {
	for _, sec := range doc.sections {
		name := strings.ToLower(sec.name)
		switch {
		case name == "layout":
			if v, ok := sec.get("mode"); ok {
				c.Layout.Mode = v
			}
			if v, ok := sec.first("gap"); ok {
				if f, err := strconv.ParseFloat(v, 64); err == nil {
					c.Layout.Gap = f
				} else {
					return sec.err("gap", v, err)
				}
			}
			if v, ok := sec.first("column-ratio", "column_ratio"); ok {
				if f, err := strconv.ParseFloat(v, 64); err == nil {
					c.Layout.ColumnRatio = f
				} else {
					return sec.err("column-ratio", v, err)
				}
			}
		case name == "input":
			if v, ok := sec.first("xkb-layout", "xkb_layout"); ok {
				c.Input.XkbLayout = v
			}
		case name == "input.repeat":
			if v, ok := sec.get("delay"); ok {
				n, err := strconv.Atoi(v)
				if err != nil {
					return sec.err("delay", v, err)
				}
				c.Input.Repeat.Delay = n
			}
			if v, ok := sec.get("rate"); ok {
				n, err := strconv.Atoi(v)
				if err != nil {
					return sec.err("rate", v, err)
				}
				c.Input.Repeat.Rate = n
			}
		case name == "color":
			if v, ok := sec.get("enabled"); ok {
				c.Color.Enabled = parseBool(v)
			}
			if v, ok := sec.first("transfer", "tf"); ok {
				c.Color.TFName = strings.ToLower(v)
			}
			if v, ok := sec.get("primaries"); ok {
				c.Color.Primaries = strings.ToLower(v)
			}
			if v, ok := sec.first("max-luma", "max_luma"); ok {
				n, err := strconv.ParseUint(v, 10, 32)
				if err != nil {
					return sec.err("max-luma", v, err)
				}
				c.Color.MaxLuma = uint32(n)
			}
		case name == "decor":
			if v, ok := sec.get("mode"); ok {
				c.Decor.Mode = v
			}
		case name == "keybinding" || name == "bindings":
			for _, p := range sec.pairs {
				c.Bindings[p.key] = p.value
			}
		case name == "auto-start" || name == "autostart":
			c.AutoStart = append(c.AutoStart, sec.all("exec-once")...)
		case name == "wm-environment":
			// Environment variables are case-sensitive on Linux, so keep the
			// key exactly as written (only the lookup key is lower-cased).
			for _, p := range sec.pairs {
				c.Environment = append(c.Environment, p.raw+"="+p.value)
			}
		case isDisplaySection(name):
			d, err := parseDisplay(sec)
			if err != nil {
				return err
			}
			c.Displays = append(c.Displays, d)
		default:
			return fmt.Errorf("unknown section [%s]", sec.name)
		}
	}
	return nil
}

// isDisplaySection reports whether a section name is a display entry:
// [Display], [Display-1], [Display-2], ... Only a separator after the prefix
// counts, so a stray [displays] is reported as unknown instead of quietly
// being read as a monitor.
func isDisplaySection(name string) bool {
	const prefix = "display"
	if name == prefix {
		return true
	}
	if !strings.HasPrefix(name, prefix) {
		return false
	}
	switch name[len(prefix)] {
	case '-', '_', '.', ' ', ':', '/':
		return true
	}
	return false
}

// normalize clamps values to supported choices so a typo cannot wedge the
// compositor.
func (c *Config) normalize() {
	if c.Layout.Mode != "tile" && c.Layout.Mode != "stack" {
		c.Layout.Mode = "tile"
	}
	switch c.Decor.Mode {
	case "auto", "ssd", "csd":
	default:
		c.Decor.Mode = "auto"
	}
	switch c.Color.TFName {
	case "srgb", "pq", "hlg", "linear":
	default:
		c.Color.TFName = "srgb"
	}
	switch c.Color.Primaries {
	case "srgb", "bt2020", "dci-p3":
	default:
		c.Color.Primaries = "srgb"
	}
}

// parseDisplay maps one [Display] section onto a Display.
func parseDisplay(sec *iniSection) (Display, error) {
	d := Display{Section: sec.name}

	if v, ok := sec.first("monitor", "output"); ok {
		d.Monitor = v
	}

	if v, ok := sec.first("resolution", "mode"); ok && v != "" && !strings.EqualFold(v, "auto") {
		w, h, err := parseResolution(v)
		if err != nil {
			return d, sec.err("resolution", v, err)
		}
		d.Resolution, d.Width, d.Height = v, w, h
	}

	if v, ok := sec.first("refresh-rate", "refresh_rate", "refreshrate", "refresh"); ok {
		d.RefreshRate = v
		if !isAuto(v) {
			hz, err := strconv.ParseFloat(strings.TrimSuffix(strings.ToLower(v), "hz"), 64)
			if err != nil {
				return d, sec.err("refresh-rate", v, err)
			}
			d.RefreshMHz = int32(hz * 1000)
		}
	}

	if v, ok := sec.first("color-management", "color-managment", "color_management"); ok {
		switch strings.ToLower(v) {
		case "", "none", "off", "false":
			d.ColorManagement = ColorManagementNone
		case "edid":
			d.ColorManagement = ColorManagementEDID
		case "icc":
			d.ColorManagement = ColorManagementICC
		default:
			return d, fmt.Errorf("line %d: color-management = %q, want icc | edid | none", sec.lineOf("color-management", "color-managment", "color_management"), v)
		}
	}

	if v, ok := sec.first("icc-file", "icc_file"); ok {
		d.ICCFile = v
	}

	if v, ok := sec.first("main-monitor", "main_monitor", "main"); ok {
		d.MainMonitor = v
		switch strings.ToLower(v) {
		case "true", "yes", "on", "1":
			d.Main = true
		case "false", "no", "off", "0", "":
			// Nothing: the first section stays the implicit main output.
		default:
			// A name: treat it as "this monitor is the main one" when it names
			// this section's own monitor.
			d.Main = d.Monitor != "" && strings.EqualFold(v, d.Monitor)
		}
	}

	if v, ok := sec.first("hdr"); ok {
		d.HDR = v
		switch strings.ToLower(v) {
		case "true", "yes", "on", "1", "hdr":
			d.HDREnabled = true
		default:
			d.HDREnabled = false
		}
	}

	if v, ok := sec.first("triple-buffering", "triple_buffering", "triplebuffering"); ok {
		d.TripleBuffering = parseBool(v)
	}

	return d, nil
}

// parseResolution parses "1920x1080". "×" and "X" are accepted too.
func parseResolution(v string) (int, int, error) {
	s := strings.ReplaceAll(strings.ToLower(v), "×", "x")
	parts := strings.SplitN(s, "x", 2)
	if len(parts) != 2 {
		return 0, 0, fmt.Errorf("want WIDTHxHEIGHT")
	}
	w, err := strconv.Atoi(strings.TrimSpace(parts[0]))
	if err != nil || w <= 0 {
		return 0, 0, fmt.Errorf("bad width")
	}
	h, err := strconv.Atoi(strings.TrimSpace(parts[1]))
	if err != nil || h <= 0 {
		return 0, 0, fmt.Errorf("bad height")
	}
	return w, h, nil
}

// isAuto reports whether a refresh-rate value means "pick the highest".
func isAuto(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "", "auto", "highest", "max", "maximum":
		return true
	}
	return false
}

// parseBool accepts the usual hand-written truthy/falsey spellings.
func parseBool(v string) bool {
	switch strings.ToLower(strings.TrimSpace(v)) {
	case "true", "yes", "on", "1", "enabled", "enable":
		return true
	}
	return false
}

func (s *iniSection) err(key, value string, cause error) error {
	return fmt.Errorf("line %d: %s = %q: %v", s.lineOf(key), key, value, cause)
}

func (s *iniSection) lineOf(keys ...string) int {
	want := make(map[string]bool, len(keys))
	for _, k := range keys {
		want[strings.ToLower(k)] = true
	}
	for _, p := range s.pairs {
		if want[p.key] {
			return p.line
		}
	}
	return 0
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
