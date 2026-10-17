// Command lumine is a pure-Go Wayland compositor. This MVP entry point brings
// up the protocol server, a layout/render pipeline and one nested Wayland
// output, then drives a fixed 60 Hz frame loop.
package main

import (
	"flag"
	"fmt"
	"log"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/Team-Quaver/lumine/internal/backend"
	"github.com/Team-Quaver/lumine/internal/compositor"
	"github.com/Team-Quaver/lumine/internal/config"
	"github.com/Team-Quaver/lumine/internal/render"
	"github.com/Team-Quaver/lumine/internal/wl"
	"golang.org/x/sys/unix"
)

const frameInterval = time.Second / 60

func main() {
	var (
		socket   = flag.String("socket", defaultSocketName(), "Wayland socket name or absolute path")
		cfgPath  = flag.String("config", config.DefaultPath(), "configuration file path (.conf)")
		terminal = flag.String("terminal", defaultTerminal(), "command bound to the terminal shortcut")
		backendK = flag.String("backend", "nested", "output backend: nested | drm | headless")
		drmDev   = flag.String("drm-device", "", "DRM card node for -backend drm (default: autodetect)")
		size     = flag.String("size", "1280x720", "initial nested window size")
		renderer = flag.String("renderer", "auto", "renderer: auto | vulkan | cpu")
		logLevel = flag.Bool("verbose", false, "log renderer and backend selection")
	)
	flag.Parse()

	width, height := parseSize(*size)

	socketPath := *socket
	displayName := *socket
	if !strings.Contains(socketPath, "/") {
		runtime := runtimeDir()
		if err := os.MkdirAll(runtime, 0o700); err != nil {
			log.Fatalf("lumine: %v", err)
		}
		socketPath = filepath.Join(runtime, *socket)
	}

	store, err := config.NewStore(*cfgPath)
	if err != nil {
		log.Fatalf("lumine: config: %v", err)
	}
	defer store.Close()

	// The [wm-environment] table is exported into our own environment, so the
	// compositor, autostart commands and every spawned client inherit it.
	cfg := store.Config()
	applyEnvironment(cfg.Environment)

	srv, err := wl.NewServer(socketPath)
	if err != nil {
		log.Fatalf("lumine: listen on %s: %v", socketPath, err)
	}

	comp := compositor.New(srv, store)
	comp.SetTerminal(strings.Fields(*terminal))
	comp.Spawn = func(argv []string) { spawn(displayName, socketPath, argv) }
	store.Reload = func(c *config.Config) {
		applyEnvironment(c.Environment)
		applyConfig(comp, c)
	}
	comp.RegisterGlobals()

	// A configured [Display] decides the output's initial size and refresh
	// rate; the nested backend still lets the host window resize it later.
	// Multiple [Display-*] sections are ordered by priority, so the primary
	// one is the section view used here.
	disp := cfg.PrimaryDisplay()
	if disp != nil && disp.Width > 0 && disp.Height > 0 {
		width, height = disp.Width, disp.Height
	}

	// Three output backends share the same frame loop. "nested" renders into
	// another compositor's window, "drm" drives a bare-metal KMS connector and
	// "headless" presents nowhere (used by the control socket and tests).
	var (
		be     backend.Output
		quit   atomic.Bool
		closer = make(chan struct{})
	)
	switch *backendK {
	case "headless":
		// nothing to bring up
	case "drm":
		drmCfg := backend.DRMConfig{
			Device:          *drmDev,
			Connector:       displayMonitor(disp),
			Width:           displayWidth(disp),
			Height:          displayHeight(disp),
			RefreshMHz:      displayRefreshMHz(disp),
			TripleBuffering: disp != nil && disp.TripleBuffering,
		}
		if *logLevel {
			drmCfg.Logf = log.Printf
		}
		d, err := backend.OpenDRM(comp, drmCfg)
		if err != nil {
			log.Fatalf("lumine: %v", err)
		}
		be = d
		width, height = d.Size()
	case "nested":
		nb, err := backend.New(comp, width, height, "Lumine")
		if err != nil {
			log.Fatalf("lumine: %v", err)
		}
		nb.OnClose = func() { quit.Store(true) }
		be = nb
		width, height = nb.Size()
	default:
		log.Fatalf("lumine: unknown backend %q (want nested, drm or headless)", *backendK)
	}

	r, rendererName := newRenderer(*renderer, width, height)
	out := &OutputRenderer{renderer: r, width: width, height: height}
	// Bare-metal and headless outputs own the pointer, so Lumine draws it. A
	// nested output sits inside another compositor that already shows a cursor,
	// and compositing a second one would draw two.
	if *backendK != "nested" {
		out.pointer = comp.Pointer
	}
	if be != nil {
		out.present = be.Present
	}

	// The DRM backend knows the connector it owns, so it overrides the nested
	// defaults with the real output name and physical size.
	var (
		outName       = outputName(disp)
		mmW     int32 = 500
		mmH     int32 = 300
		refresh       = refreshMHz(disp)
	)
	if d, ok := be.(backend.Describer); ok {
		outName = d.OutputName()
		if wmm, hmm := d.PhysicalSizeMM(); wmm > 0 && hmm > 0 {
			mmW, mmH = wmm, hmm
		}
		if got := d.RefreshMHz(); got > 0 {
			refresh = got
		}
	}

	comp.AddOutput(&compositor.Output{
		Name:            outName,
		W:               width,
		H:               height,
		Refresh:         refresh,
		WidthMM:         mmW,
		HeightMM:        mmH,
		Renderer:        out,
		TripleBuffering: disp != nil && disp.TripleBuffering,
	})
	if err := comp.Listen(socketPath, "lumine"); err != nil {
		log.Fatalf("lumine: control socket: %v", err)
	}

	// Apply the initial configuration: layout tuning, per-output color space
	// (including any HDR request from [Display]) and the rest of the knobs.
	applyConfig(comp, cfg)

	// Any positional arguments are a command to launch once the compositor is
	// listening; the child is pointed at Lumine's own socket.
	if argv := flag.Args(); len(argv) > 0 {
		spawn(displayName, socketPath, argv)
	}

	// exec-once lines from [auto-start] run exactly once, after the socket is
	// accepting connections so the clients can connect immediately.
	for _, line := range cfg.AutoStart {
		if argv := strings.Fields(line); len(argv) > 0 {
			spawn(displayName, socketPath, argv)
		}
	}

	if *logLevel {
		log.Printf("lumine: display=%s socket=%s renderer=%s backend=%s config=%s",
			displayName, socketPath, rendererName, *backendK, *cfgPath)
		log.Printf("lumine: output=%s %dx%d@%dmHz hdr=%v triple-buffering=%v autostart=%d",
			outName, width, height, refresh,
			disp != nil && disp.HDREnabled, disp != nil && disp.TripleBuffering, len(cfg.AutoStart))
	}

	// Signals and the host compositor closing our window both stop the loop.
	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, syscall.SIGINT, syscall.SIGTERM)
	go func() {
		select {
		case <-sigc:
		case <-closer:
		}
		quit.Store(true)
	}()
	defer close(closer)

	run(comp, store, be, out, &quit)

	if be != nil {
		be.Close()
	}
	if c, ok := r.(interface{ Close() }); ok {
		c.Close()
	}
}

// run drives the compositor: backend events, config changes and frame ticks.
func run(comp *compositor.Compositor, store *config.Store, be backend.Output, out *OutputRenderer, quit *atomic.Bool) {
	next := time.Now()
	for !quit.Load() {
		next = next.Add(frameInterval)
		if d := time.Until(next); d > 0 {
			if err := wait(be, store, d); err != nil {
				log.Printf("lumine: backend: %v", err)
				quit.Store(true)
				break
			}
		}
		if be != nil {
			if be.Closed() {
				// The host compositor went away, or the DRM device vanished;
				// there is nothing left to present into.
				quit.Store(true)
				break
			}
			w, h := be.Size()
			out.Resize(w, h)
		}
		comp.Frame()
		if now := time.Now(); next.Before(now.Add(-frameInterval)) {
			next = now // we fell behind; resynchronise instead of spinning
		}
	}
}

// wait blocks until the frame deadline, waking early for backend input or a
// configuration change. The returned error is a fatal backend failure.
func wait(be backend.Output, store *config.Store, d time.Duration) error {
	fds := make([]unix.PollFd, 0, 2)
	if int(store.FD()) >= 0 {
		fds = append(fds, unix.PollFd{Fd: int32(store.FD()), Events: unix.POLLIN})
	}
	if be != nil {
		fds = append(fds, unix.PollFd{Fd: int32(be.FD()), Events: unix.POLLIN})
	}
	ms := int(d.Round(time.Millisecond) / time.Millisecond)
	if ms < 1 {
		ms = 1
	}
	if _, err := unix.Poll(fds, ms); err != nil {
		if err != syscall.EINTR {
			return err
		}
		return nil
	}
	for i := range fds {
		if fds[i].Revents == 0 {
			continue
		}
		switch {
		case int32(store.FD()) == fds[i].Fd:
			store.OnReadable()
		case be != nil && int32(be.FD()) == fds[i].Fd:
			if err := be.Drain(); err != nil {
				return err
			}
		}
	}
	return nil
}

// OutputRenderer adapts a render.Renderer to compositor.Renderer: it owns the
// framebuffer, converts draw items and hands the finished frame to the backend.
type OutputRenderer struct {
	renderer render.Renderer
	present  func(*render.Framebuffer) error
	// pointer reports the compositor's pointer position, which is where the
	// software cursor is stamped into the finished frame.
	pointer func() (int, int)
	fb      *render.Framebuffer
	width   int
	height  int

	lastPresentErr time.Time
}

// Resize reallocates the framebuffer for a new output size.
func (o *OutputRenderer) Resize(w, h int) {
	if w <= 0 || h <= 0 {
		return
	}
	if o.fb != nil && w == o.width && h == o.height {
		return
	}
	o.width, o.height = w, h
	o.fb = render.NewFramebuffer(w, h)
}

// Draw renders one frame and presents it.
func (o *OutputRenderer) Draw(w, h int, items []compositor.DrawItem) error {
	if w <= 0 || h <= 0 {
		return nil
	}
	o.Resize(w, h)
	if o.fb == nil {
		return nil
	}
	ritems := make([]render.DrawItem, 0, len(items))
	for _, it := range items {
		ritems = append(ritems, render.DrawItem{
			Image: it.Image,
			X:     it.X, Y: it.Y, W: it.W, H: it.H,
			Frame: it.Frame, Title: it.Title, AppID: it.AppID,
			Active: it.Activated,
		})
	}
	if err := o.renderer.Draw(o.fb, ritems); err != nil {
		return err
	}
	// The cursor is composited after the renderer so it survives regardless of
	// which renderer drew the frame and is never occluded by a full-screen
	// client surface.
	if o.pointer != nil {
		px, py := o.pointer()
		render.DrawCursor(o.fb, px, py, render.DefaultTheme().Cursor)
	}
	if o.present != nil {
		// A present failure used to be swallowed by the frame loop; surfacing
		// it once is what turns a silent black output into a diagnosable one.
		if err := o.present(o.fb); err != nil {
			o.logPresentErr(err)
			return err
		}
	}
	return nil
}

// logPresentErr reports a backend present failure at most once a second, so a
// persistent fault does not flood the log but is visible straight away.
func (o *OutputRenderer) logPresentErr(err error) {
	now := time.Now()
	if now.Sub(o.lastPresentErr) < time.Second {
		return
	}
	o.lastPresentErr = now
	log.Printf("lumine: present: %v", err)
}

// SetColorSpace forwards the negotiated color state to the renderer.
func (o *OutputRenderer) SetColorSpace(enabled bool, tf, primaries string, maxLuma uint32) {
	o.renderer.SetColorSpace(enabled, tf, primaries, maxLuma)
}

// newRenderer picks the requested renderer, falling back to software rendering
// when Vulkan is unavailable.
func newRenderer(kind string, w, h int) (render.Renderer, string) {
	theme := render.DefaultTheme()
	switch kind {
	case "cpu":
		return render.NewCPU(theme), "cpu"
	case "vulkan":
		v, err := render.NewVulkan(theme, w, h)
		if err != nil {
			log.Printf("lumine: vulkan unavailable (%v); falling back to cpu", err)
			return render.NewCPU(theme), "cpu"
		}
		return v, "vulkan"
	default:
		if v, err := render.NewVulkan(theme, w, h); err == nil {
			return v, "vulkan"
		}
		return render.NewCPU(theme), "cpu"
	}
}

// spawn launches a client with WAYLAND_DISPLAY pointed at this compositor. The
// parent's WAYLAND_DISPLAY must not leak into children, otherwise they would
// talk to the host compositor instead.
func spawn(display, socketPath string, argv []string) {
	if len(argv) == 0 {
		return
	}
	cmd := exec.Command(argv[0], argv[1:]...)
	env := make([]string, 0, len(os.Environ())+1)
	for _, kv := range os.Environ() {
		if strings.HasPrefix(kv, "WAYLAND_DISPLAY=") {
			continue
		}
		env = append(env, kv)
	}
	target := display
	if strings.Contains(socketPath, "/") {
		target = socketPath
	}
	env = append(env, "WAYLAND_DISPLAY="+target)
	cmd.Env = env
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		log.Printf("lumine: spawn %v: %v", argv, err)
	}
}

// applyEnvironment exports the [wm-environment] entries into the process
// environment. Because children inherit os.Environ, this covers autostart
// commands, the terminal shortcut and luminectl-driven spawns alike.
func applyEnvironment(env []string) {
	for _, kv := range env {
		if k, v, ok := strings.Cut(kv, "="); ok && k != "" {
			_ = os.Setenv(k, v)
		}
	}
}

// applyConfig folds a [Display] HDR request into the color settings and hands
// the result to the compositor.
func applyConfig(comp *compositor.Compositor, cfg *config.Config) {
	foldDisplayHDR(cfg)
	comp.ApplyConfig(cfg)
}

// foldDisplayHDR turns an HDR display into a PQ/BT.2020 output unless the
// [color] section already asked for something explicit.
func foldDisplayHDR(cfg *config.Config) {
	d := cfg.PrimaryDisplay()
	if d == nil || !d.HDREnabled {
		return
	}
	cfg.Color.Enabled = true
	if cfg.Color.TFName == "srgb" {
		cfg.Color.TFName = "pq"
	}
	if cfg.Color.Primaries == "srgb" {
		cfg.Color.Primaries = "bt2020"
	}
}

// displayMonitor, displayWidth, displayHeight and displayRefreshMHz read the
// primary [Display] section without forcing every caller to nil-check it.
func displayMonitor(d *config.Display) string {
	if d == nil {
		return ""
	}
	return d.Monitor
}

func displayWidth(d *config.Display) int {
	if d == nil {
		return 0
	}
	return d.Width
}

func displayHeight(d *config.Display) int {
	if d == nil {
		return 0
	}
	return d.Height
}

func displayRefreshMHz(d *config.Display) int32 {
	if d == nil {
		return 0
	}
	return d.RefreshMHz
}

// outputName picks the wl_output name: the connector from [Display].monitor,
// falling back to the built-in nested name.
func outputName(disp *config.Display) string {
	if disp != nil && disp.Monitor != "" {
		return disp.Monitor
	}
	return "lumine-nested"
}

// refreshMHz returns the configured refresh rate in mHz, defaulting to 60 Hz
// when the display asks for the highest available (or none is configured).
func refreshMHz(disp *config.Display) int32 {
	if disp != nil && disp.RefreshMHz > 0 {
		return disp.RefreshMHz
	}
	return 60_000
}

func parseSize(s string) (int, int) {
	var w, h int
	if _, err := fmt.Sscanf(s, "%dx%d", &w, &h); err != nil || w <= 0 || h <= 0 {
		return 1280, 720
	}
	return w, h
}

func runtimeDir() string {
	if d := os.Getenv("XDG_RUNTIME_DIR"); d != "" {
		return d
	}
	return fmt.Sprintf("/run/user/%d", os.Getuid())
}

func defaultSocketName() string {
	return "lumine-wayland"
}

func defaultTerminal() string {
	if t := os.Getenv("LUMINE_TERMINAL"); t != "" {
		return t
	}
	return "kitty"
}
