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
		cfgPath  = flag.String("config", defaultConfigPath(), "TOML configuration path")
		terminal = flag.String("terminal", defaultTerminal(), "command bound to the terminal shortcut")
		backendK = flag.String("backend", "nested", "output backend: nested | headless")
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

	srv, err := wl.NewServer(socketPath)
	if err != nil {
		log.Fatalf("lumine: listen on %s: %v", socketPath, err)
	}

	comp := compositor.New(srv, store)
	comp.SetTerminal(strings.Fields(*terminal))
	comp.Spawn = func(argv []string) { spawn(displayName, socketPath, argv) }
	store.Reload = comp.ApplyConfig
	comp.RegisterGlobals()

	// The nested backend is the MVP output: it renders into another
	// compositor's window, so Lumine can be developed and tested everywhere.
	var (
		be     *backend.Backend
		quit   atomic.Bool
		closer = make(chan struct{})
	)
	if *backendK != "headless" {
		be, err = backend.New(comp, width, height, "Lumine")
		if err != nil {
			log.Fatalf("lumine: %v", err)
		}
		be.OnClose = func() { quit.Store(true) }
		width, height = be.Size()
	}

	r, rendererName := newRenderer(*renderer, width, height)
	out := &OutputRenderer{renderer: r, width: width, height: height}
	if be != nil {
		out.present = be.Present
	}

	comp.AddOutput(&compositor.Output{
		Name: "lumine-nested", W: width, H: height, Refresh: 60_000,
		WidthMM: 500, HeightMM: 300, Renderer: out,
	})
	if err := comp.Listen(socketPath, "lumine"); err != nil {
		log.Fatalf("lumine: control socket: %v", err)
	}

	// Any positional arguments are a command to launch once the compositor is
	// listening; the child is pointed at Lumine's own socket.
	if argv := flag.Args(); len(argv) > 0 {
		spawn(displayName, socketPath, argv)
	}

	if *logLevel {
		log.Printf("lumine: display=%s socket=%s renderer=%s backend=%s", displayName, socketPath, rendererName, *backendK)
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

// run drives the compositor: parent events, config changes and frame ticks.
func run(comp *compositor.Compositor, store *config.Store, be *backend.Backend, out *OutputRenderer, quit *atomic.Bool) {
	next := time.Now()
	for !quit.Load() {
		next = next.Add(frameInterval)
		if d := time.Until(next); d > 0 {
			wait(be, store, d)
		}
		if be != nil {
			if be.Conn().Closed() {
				// The host compositor went away; there is nothing left to
				// present into.
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

// wait blocks until the frame deadline, waking early for parent input or a
// configuration change.
func wait(be *backend.Backend, store *config.Store, d time.Duration) {
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
	if _, err := unix.Poll(fds, ms); err != nil && err != syscall.EINTR {
		log.Printf("lumine: poll: %v", err)
		return
	}
	for i := range fds {
		if fds[i].Revents == 0 {
			continue
		}
		switch {
		case int32(store.FD()) == fds[i].Fd:
			store.OnReadable()
		case be != nil && int32(be.FD()) == fds[i].Fd:
			if err := be.Conn().OnReadable(); err != nil {
				return
			}
		}
	}
}

// OutputRenderer adapts a render.Renderer to compositor.Renderer: it owns the
// framebuffer, converts draw items and hands the finished frame to the backend.
type OutputRenderer struct {
	renderer render.Renderer
	present  func(*render.Framebuffer) error
	fb       *render.Framebuffer
	width    int
	height   int
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
	if o.present != nil {
		return o.present(o.fb)
	}
	return nil
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

func defaultConfigPath() string {
	base := os.Getenv("XDG_CONFIG_HOME")
	if base == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return "lumine.toml"
		}
		base = filepath.Join(home, ".config")
	}
	return filepath.Join(base, "lumine", "config.toml")
}

func defaultTerminal() string {
	if t := os.Getenv("LUMINE_TERMINAL"); t != "" {
		return t
	}
	return "kitty"
}
