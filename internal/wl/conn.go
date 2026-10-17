package wl

import (
	"encoding/binary"
	"errors"
	"fmt"
	"net"
	"os"
	"sync"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// Global is a protocol object advertised on wl_registry. Bind is called when
// a client binds the global; it must create the server-side resource.
type Global struct {
	Name   uint32
	Iface  string
	Version uint32
	Bind   func(c *Conn, id uint32, version uint32)
}

// Server listens for Wayland clients.
type Server struct {
	sock    net.Listener
	Globals []*Global
	NewConn func(c *Conn)

	mu      sync.Mutex
	serial  uint32
	globals map[uint32]bool
	nextGID uint32
}

func NewServer(path string) (*Server, error) {
	os.Remove(path)
	l, err := net.Listen("unix", path)
	if err != nil {
		return nil, err
	}
	os.Chmod(path, 0o660)
	return &Server{sock: l, globals: map[uint32]bool{}, nextGID: 1}, nil
}

func (s *Server) AddGlobal(iface string, version uint32, bind func(c *Conn, id uint32, version uint32)) *Global {
	s.mu.Lock()
	defer s.mu.Unlock()
	g := &Global{Name: s.nextGID, Iface: iface, Version: version, Bind: bind}
	s.nextGID++
	s.Globals = append(s.Globals, g)
	return g
}

// Accept runs until the listener closes. Each connection is handled inline;
// the compositor's event loop multiplexes conns by calling c.Dispatch when
// the underlying fd is readable.
func (s *Server) Accept() error {
	for {
		nc, err := s.sock.Accept()
		if err != nil {
			return err
		}
		c := newConn(nc.(*net.UnixConn))
		s.mu.Lock()
		c.globalsCache = s.Globals
		s.mu.Unlock()
		if s.NewConn != nil {
			s.NewConn(c)
		}
		go c.readLoop()
	}
}

func (s *Server) nextSerial() uint32 {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.serial++
	return s.serial
}

// Conn is one connected client.
type Conn struct {
	raw *net.UnixConn
	fd  int

	mu       sync.Mutex
	objs     map[uint32]*Object
	nextID   uint32
	out      []byte
	outFDs   []int
	closed   bool
	serial   uint32
	display  *Object
	registry *Object

	in    []byte
	inFDs []int

	// isClient is set on connections created with Dial. It flips the direction
	// of the object table: incoming messages are events, outgoing are requests.
	isClient bool
	// err is the last protocol error received (client mode).
	err error

	globalsCache []*Global
}

func newConn(raw *net.UnixConn) *Conn {
	fd := 0
	if sc, err := raw.SyscallConn(); err == nil {
		sc.Control(func(fdInt uintptr) { fd = int(fdInt) })
	}
	c := &Conn{
		raw:    raw,
		fd:     fd,
		objs:   map[uint32]*Object{},
		nextID: 2, // 1 is wl_display
	}
	d := &Object{Conn: c, ID: DisplayObjectID, Iface: displayIface, Version: 1, res: (*displayRes)(nil)}
	c.objs[DisplayObjectID] = d
	c.display = d
	return c
}

// displayIface describes wl_display for the object table.
var displayIface = &Interface{
	Name:    "wl_display",
	Version: 1,
	Requests: []*Message{
		{Name: "sync", Args: []Arg{{Type: ArgNewID, Iface: "wl_callback"}}},
		{Name: "get_registry", Args: []Arg{{Type: ArgNewID, Iface: "wl_registry"}}},
	},
	Events: []*Message{
		{Name: "error", Args: []Arg{{Type: ArgObject}, {Type: ArgUint}, {Type: ArgString}}},
		{Name: "delete_id", Args: []Arg{{Type: ArgUint}}},
	},
}

type displayRes struct{ o *Object }

func (d *displayRes) object() *Object { return d.o }

// resource is implemented by every generated wrapper (embeds Object).
type Resource interface {
	object() *Object
}

func (o *Object) object() *Object { return o }

// objAs decodes an object argument typed as T. It looks the id up in the
// connection's object table and unwraps the generated resource.
func objAs[T Resource](r *wireReader, iface string, parent *Object) T {
	var zero T
	id := r.uint32()
	if r.err != nil {
		return zero
	}
	obj := parent.Conn.lookup(id)
	if obj == nil {
		if id == 0 {
			return zero
		}
		r.fail("%s: no object %d", iface, id)
		return zero
	}
	if obj.Iface != nil && obj.Iface.Name != iface {
		r.fail("obj %d is %s, want %s", id, obj.Iface.Name, iface)
		return zero
	}
	res, ok := obj.res.(T)
	if !ok {
		r.fail("obj %d: bad wrapper for %s", id, iface)
		return zero
	}
	return res
}

func (c *Conn) lookup(id uint32) *Object {
	c.mu.Lock()
	defer c.mu.Unlock()
	o := c.objs[id]
	if o != nil && o.destroyed {
		return nil
	}
	return o
}

// register adds a resource to the object table. The caller owns id allocation:
// server-side requests use the client-supplied id, while standalone resources
// (wl_output) get an id in the server range allocated by allocServerID.
func (c *Conn) register(res Resource) {
	o := res.object()
	c.mu.Lock()
	defer c.mu.Unlock()
	if !c.isClient && o.ID == 0 {
		o.ID = c.nextServerIDLocked()
	}
	c.objs[o.ID] = o
}

// allocServerID reserves an object id in the compositor's id range.
func (c *Conn) allocServerID() uint32 {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.nextServerIDLocked()
}

func (c *Conn) nextServerIDLocked() uint32 {
	c.nextID++
	return ServerIDMin + c.nextID
}

func (c *Conn) destroyObject(o *Object) {
	c.mu.Lock()
	o.destroyed = true
	id := o.ID
	iface := ""
	if o.Iface != nil {
		iface = o.Iface.Name
	}
	impl := o.Impl
	delete(c.objs, id)
	c.mu.Unlock()
	if cg, ok := impl.(ClientGone); ok {
		cg.ClientGone()
	}
	// Only a compositor sends wl_display.delete_id.
	if !c.isClient && id < ServerIDMin && iface != "wl_callback" {
		c.sendDisplayEvent(1, uint32(id)) // wl_display.delete_id
	}
}

func (c *Conn) sendDisplayEvent(opcode uint16, args ...any) {
	c.display.send(opcode, args...)
}

func (c *Conn) close() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	objs := make([]*Object, 0, len(c.objs))
	for _, o := range c.objs {
		objs = append(objs, o)
	}
	// Drop any fds that were queued but never flushed.
	for _, fd := range c.outFDs {
		_ = unix.Close(fd)
	}
	c.outFDs = nil
	c.out = nil
	c.mu.Unlock()
	for _, o := range objs {
		if cg, ok := o.Impl.(ClientGone); ok {
			cg.ClientGone()
		}
	}
	c.raw.Close()
}

// queueEvent appends an encoded event to the outgoing buffer.
//
// File descriptors are duplicated: the socket only receives them when Flush
// writes the message, which may happen well after the caller returns (and
// after the caller has closed its own copy). queueEvent takes ownership of the
// duplicates and Flush closes them once they have been handed to the kernel.
func (c *Conn) queueEvent(msg []byte, fds []int) error {
	dups := make([]int, 0, len(fds))
	for _, fd := range fds {
		d, err := unix.Dup(fd)
		if err != nil {
			for _, x := range dups {
				_ = unix.Close(x)
			}
			return err
		}
		dups = append(dups, d)
	}
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		for _, x := range dups {
			_ = unix.Close(x)
		}
		return errors.New("connection closed")
	}
	c.out = append(c.out, msg...)
	c.outFDs = append(c.outFDs, dups...)
	c.mu.Unlock()
	return nil
}

// Flush writes queued events. FDs ride on the first control message chunk.
func (c *Conn) Flush() error {
	c.mu.Lock()
	if c.closed || (len(c.out) == 0 && len(c.outFDs) == 0) {
		c.out = nil
		c.outFDs = nil
		c.mu.Unlock()
		return nil
	}
	data := c.out
	fds := c.outFDs
	c.out = nil
	c.outFDs = nil
	c.mu.Unlock()

	// The fds were duplicated for us by queueEvent; close our copies once they
	// have been (or failed to be) sent.
	defer func() {
		for _, fd := range fds {
			_ = unix.Close(fd)
		}
	}()

	sent := false
	for len(data) > 0 {
		var cmsg []byte
		if !sent && len(fds) > 0 {
			cmsg = packFDs(fds)
		}
		n, err := unix.SendmsgN(c.fd, data, cmsg, nil, 0)
		if err != nil {
			if err == syscall.EAGAIN || err == syscall.EINTR {
				continue
			}
			c.close()
			return err
		}
		if cmsg != nil {
			sent = true
		}
		data = data[n:]
	}
	return nil
}

func packFDs(fds []int) []byte {
	hdr := unix.Cmsghdr{
		Level: unix.SOL_SOCKET,
		Type:  unix.SCM_RIGHTS,
		Len:   uint64(unix.CmsgLen(4 * len(fds))),
	}
	buf := make([]byte, unix.CmsgSpace(4*len(fds)))
	*(*unix.Cmsghdr)(unsafe.Pointer(&buf[0])) = hdr
	for i, fd := range fds {
		binary.LittleEndian.PutUint32(buf[unix.CmsgLen(0)+4*i:], uint32(fd))
	}
	return buf
}

// readLoop blocks on the socket, decodes messages and dispatches them.
func (c *Conn) readLoop() {
	defer c.close()
	buf := make([]byte, 65536)
	oob := make([]byte, 4096)
	for {
		n, oobn, _, _, err := c.raw.ReadMsgUnix(buf, oob)
		if err != nil {
			return
		}
		var fds []int
		if oobn > 0 {
			fds = unpackFDs(oob[:oobn])
		}
		if err := c.feed(buf[:n], fds); err != nil {
			c.postError(err)
			return
		}
		if err := c.Flush(); err != nil {
			return
		}
	}
}

// unpackFDs extracts the SCM_RIGHTS fds carried in the control-message buffer.
// The caller owns the returned fds and must close any it does not hand to a
// resource (for example an shm pool or a keymap reader).
func unpackFDs(oob []byte) []int {
	var fds []int
	for len(oob) >= int(unsafe.Sizeof(unix.Cmsghdr{})) {
		h := *(*unix.Cmsghdr)(unsafe.Pointer(&oob[0]))
		if int(h.Level) == unix.SOL_SOCKET && int(h.Type) == unix.SCM_RIGHTS {
			payload := oob[unix.CmsgLen(0):h.Len]
			for len(payload) >= 4 {
				fds = append(fds, int(binary.LittleEndian.Uint32(payload)))
				payload = payload[4:]
			}
		}
		oob = oob[unix.CmsgSpace(0)+pad8(int(h.Len)-unix.CmsgLen(0)):]
		if h.Len == 0 {
			break
		}
	}
	return fds
}

func pad8(n int) int { return (n + 7) &^ 7 }

// feed appends freshly read bytes (and any fds carried with them) to the
// input buffer and routes every complete message it now holds. Wayland
// messages may be split across recvmsg boundaries, so incomplete tails stay
// buffered until the rest arrives.
func (c *Conn) feed(data []byte, fds []int) error {
	c.mu.Lock()
	c.in = append(c.in, data...)
	c.inFDs = append(c.inFDs, fds...)
	c.mu.Unlock()

	for {
		c.mu.Lock()
		if len(c.in) < 8 {
			c.mu.Unlock()
			return nil
		}
		objID := binary.LittleEndian.Uint32(c.in)
		sizeOp := binary.LittleEndian.Uint32(c.in[4:])
		size := int(sizeOp >> 16)
		op := uint16(sizeOp & 0xFFFF)
		if size < 8 {
			c.mu.Unlock()
			return fmt.Errorf("wl: bad message size %d", size)
		}
		if size > len(c.in) {
			c.mu.Unlock()
			return nil // wait for the rest
		}
		msg := c.in[:size]
		rest := append([]byte(nil), c.in[size:]...)
		c.in = rest
		fds := c.inFDs
		c.mu.Unlock()

		r := &wireReader{data: msg[8:], fds: fds}
		err := c.route(objID, op, r)
		if r.err != nil && err == nil {
			err = r.err
		}
		c.mu.Lock()
		if r.fi > 0 && r.fi <= len(c.inFDs) {
			c.inFDs = append([]int(nil), c.inFDs[r.fi:]...)
		}
		c.mu.Unlock()
		if err != nil {
			return err
		}
	}
}

// route decodes one message's arguments for its target object. Requests go to
// the compositor-side Dispatcher, events go to the client-side event handlers.
func (c *Conn) route(objID uint32, op uint16, r *wireReader) error {
	if objID == DisplayObjectID {
		if c.isClient {
			return c.displayEvent(op, r)
		}
		return c.displayRequest(op, r)
	}
	o := c.lookup(objID)
	if o == nil {
		return fmt.Errorf("unknown object %d", objID)
	}
	if !c.isClient && o.Iface != nil && o.Iface.Name == "wl_registry" {
		return c.registryRequest(o, op, r)
	}
	if c.isClient {
		if d, ok := o.res.(EventDispatcher); ok {
			d.dispatchEvent(op, r)
			return r.err
		}
		return fmt.Errorf("object %s (%d) has no event dispatcher", o.Iface.Name, objID)
	}
	if d, ok := o.res.(Dispatcher); ok {
		d.dispatch(op, r)
		return r.err
	}
	return fmt.Errorf("object %s (%d) has no dispatcher", o.Iface.Name, objID)
}

// displayRequest handles wl_display requests from clients.
func (c *Conn) displayRequest(op uint16, r *wireReader) error {
	switch op {
	case 0: // sync(new_id)
		id := r.uint32()
		if r.err != nil {
			return r.err
		}
		cb := c.newObject(id, "wl_callback", 1, nil)
		cb.send(0, c.nextSerial()) // done
		c.destroyObject(cb)
	case 1: // get_registry(new_id)
		id := r.uint32()
		if r.err != nil {
			return r.err
		}
		reg := c.newObject(id, "wl_registry", 1, nil)
		c.mu.Lock()
		c.registry = reg
		c.mu.Unlock()
		for _, g := range c.serverGlobals() {
			reg.send(0, g.Name, g.Iface, g.Version) // global
		}
	default:
		return fmt.Errorf("wl_display: bad opcode %d", op)
	}
	return r.err
}

func (c *Conn) nextSerial() uint32 {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.serial++
	return c.serial
}

func (c *Conn) serverGlobals() []*Global {
	// Globals are registered on the Server; keep a copy on the Conn at
	// creation time so handlers can be per-connection.
	if c.globalsCache != nil {
		return c.globalsCache
	}
	return nil
}

func (c *Conn) registryRequest(o *Object, op uint16, r *wireReader) error {
	if op != 0 {
		return fmt.Errorf("wl_registry: bad opcode %d", op)
	}
	name := r.uint32()
	iface := r.string()
	version := r.uint32()
	id := r.uint32()
	if r.err != nil {
		return r.err
	}
	for _, g := range c.globalsCache {
		if g.Name == name && g.Iface == iface {
			g.Bind(c, id, min(version, g.Version))
			return nil
		}
	}
	return fmt.Errorf("wl_registry.bind: no global %s", iface)
}

// newObject creates a server-side object for a client-allocated id.
func (c *Conn) newObject(id uint32, iface string, version uint32, impl any) *Object {
	ifaceDef := c.ifaceByName(iface)
	o := &Object{Conn: c, ID: id, Iface: ifaceDef, Version: version, Impl: impl}
	c.mu.Lock()
	c.objs[id] = o
	c.mu.Unlock()
	return o
}

// knownIfaces lets the display-level object table resolve interfaces for
// objects it creates itself (wl_registry, wl_callback) and for generically
// constructed proxies. The generated interface definitions register
// themselves so the map cannot silently drift out of sync with the protocol
// XML.
var knownIfaces = map[string]*Interface{}

// registerIface is called by the generated interface initialisers.
func registerIface(i *Interface) { knownIfaces[i.Name] = i }

func (c *Conn) ifaceByName(name string) *Interface {
	if i, ok := knownIfaces[name]; ok {
		return i
	}
	return &Interface{Name: name, Version: 1}
}

func (c *Conn) postError(err error) {
	if c.display != nil {
		c.display.send(0, c.display, uint32(1), err.Error())
	}
	c.Flush()
	c.close()
}

// PostError reports a protocol error to the client and closes the
// connection, as required by the Wayland spec for fatal errors. obj may be
// any resource (or nil) and is sent as the error's object argument.
func (c *Conn) PostError(obj any, code uint32, msg string) {
	o := c.display
	if r, ok := obj.(Resource); ok {
		o = r.object()
	} else if ob, ok := obj.(*Object); ok && ob != nil {
		o = ob
	}
	if o != nil {
		c.display.send(0, o, code, msg)
	}
	c.Flush()
	c.close()
}

// FD returns the raw socket fd for event-loop integration.
func (c *Conn) FD() int { return c.fd }

// Closed reports whether the connection has been shut down.
func (c *Conn) Closed() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.closed
}

// OnReadable is used by epoll-driven loops: read + dispatch + flush.
func (c *Conn) OnReadable() error {
	buf := make([]byte, 65536)
	oob := make([]byte, 4096)
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
		c.postError(err)
		return err
	}
	return c.Flush()
}

func min(a, b uint32) uint32 {
	if a < b {
		return a
	}
	return b
}
