package wl

import (
	"fmt"
	"net"
	"os"
	"time"
)

// Dial connects to a Wayland compositor socket as a client, in pure Go: no
// libwayland is involved. The returned Conn drives requests through the
// generated Req* methods and delivers events to the implementations passed to
// the generated constructors.
//
// Messages are only sent when Flush is called; call NextEvent (or drive the
// connection from an event loop through OnReadable/FD) to receive events.
func Dial(path string) (*Conn, error) {
	raw, err := net.Dial("unix", path)
	if err != nil {
		return nil, err
	}
	uc := raw.(*net.UnixConn)
	fd := 0
	if sc, err := uc.SyscallConn(); err == nil {
		sc.Control(func(fdInt uintptr) { fd = int(fdInt) })
	}
	c := &Conn{
		raw:      uc,
		fd:       fd,
		objs:     map[uint32]*Object{},
		nextID:   DisplayObjectID,
		isClient: true,
	}
	d := &Object{Conn: c, ID: DisplayObjectID, Iface: displayIface, Version: 1}
	c.objs[DisplayObjectID] = d
	c.display = d
	return c, nil
}

// SocketPath resolves a Wayland display name (as found in WAYLAND_DISPLAY) to
// an absolute socket path.
func SocketPath(name string) string {
	if len(name) > 0 && name[0] == '/' {
		return name
	}
	dir := os.Getenv("XDG_RUNTIME_DIR")
	if dir == "" {
		dir = fmt.Sprintf("/run/user/%d", os.Getuid())
	}
	return dir + "/" + name
}

// AllocID reserves a new client-side object id.
func (c *Conn) AllocID() uint32 {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.nextID++
	return c.nextID
}

// IsClient reports whether this connection is a client proxy.
func (c *Conn) IsClient() bool { return c.isClient }

// Close shuts the connection down.
func (c *Conn) Close() { c.close() }

// Err returns the last protocol error received from the peer.
func (c *Conn) Err() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.err
}

// DisplayGetRegistry sends wl_display.get_registry and returns the proxy.
func (c *Conn) DisplayGetRegistry(impl any) (*WlRegistry, error) {
	id := c.AllocID()
	reg := NewWlRegistry(c, id, 1, impl)
	if err := c.display.req(1, &reg.Object); err != nil {
		return nil, err
	}
	c.mu.Lock()
	c.registry = &reg.Object
	c.mu.Unlock()
	return reg, nil
}

// DisplaySync sends wl_display.sync and returns the sync callback proxy.
func (c *Conn) DisplaySync(impl any) (*WlCallback, error) {
	id := c.AllocID()
	cb := NewWlCallback(c, id, 1, impl)
	if err := c.display.req(0, &cb.Object); err != nil {
		return nil, err
	}
	return cb, nil
}

// NextEvent blocks until one batch of events has been dispatched. It is meant
// for one-shot setup sequences such as RoundTrip, before an event loop owns
// the connection.
func (c *Conn) NextEvent() error {
	buf := make([]byte, 65536)
	oob := make([]byte, 4096)
	for {
		n, oobn, _, _, err := c.raw.ReadMsgUnix(buf, oob)
		if err != nil {
			c.close()
			return err
		}
		var fds []int
		if oobn > 0 {
			fds = unpackFDs(oob[:oobn])
		}
		if err := c.feed(buf[:n], fds); err != nil {
			c.close()
			return err
		}
		if err := c.Flush(); err != nil {
			return err
		}
		return nil
	}
}

// RoundTrip performs a wl_display.sync round trip with the compositor.
func (c *Conn) RoundTrip() error {
	done := make(chan struct{})
	cb, err := c.DisplaySync(roundTripDone{done})
	if err != nil {
		return err
	}
	_ = cb
	if err := c.Flush(); err != nil {
		return err
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		select {
		case <-done:
			return nil
		default:
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("wl: round trip timed out")
		}
		if err := c.NextEvent(); err != nil {
			select {
			case <-done:
				return nil
			default:
			}
			return err
		}
	}
}

type roundTripDone struct{ ch chan struct{} }

func (r roundTripDone) Done(o *WlCallback, callbackData uint32) {
	o.Object.Destroy()
	close(r.ch)
}

// displayEvent handles wl_display events on the client side.
func (c *Conn) displayEvent(op uint16, r *wireReader) error {
	switch op {
	case 0: // error
		obj := r.uint32()
		code := r.uint32()
		msg := r.string()
		err := fmt.Errorf("wl_display error: object %d code %d: %s", obj, code, msg)
		c.mu.Lock()
		c.err = err
		c.mu.Unlock()
		return err
	case 1: // delete_id
		id := r.uint32()
		if o := c.lookup(id); o != nil {
			c.mu.Lock()
			delete(c.objs, id)
			c.mu.Unlock()
		}
	}
	return nil
}
