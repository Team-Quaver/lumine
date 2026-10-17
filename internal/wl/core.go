// Package wl implements the Wayland wire protocol and server-side object
// model in pure Go. No libwayland is involved: connections are raw Unix
// sockets driven by the compositor's epoll loop.
package wl

import (
	"encoding/binary"
	"fmt"
	"reflect"
)

// Object ID ranges from the Wayland specification: clients allocate from
// [1, 0xFEFFFFFF], the compositor from [0xFF000000, 0xFFFFFFFF].
const (
	ClientIDMax     uint32 = 0xFEFFFFFF
	ServerIDMin     uint32 = 0xFF000000
	DisplayObjectID uint32 = 1
)

// ArgType identifies the wire type of a message argument.
type ArgType uint8

const (
	ArgInt ArgType = iota
	ArgUint
	ArgFixed
	ArgString
	ArgObject
	ArgNewID
	ArgArray
	ArgFD
)

// Arg describes one message argument.
type Arg struct {
	Type   ArgType
	Iface  string // interface name, for object/new_id args
	NullOK bool   // object args may be 0
}

// Message describes one request or event.
type Message struct {
	Name string
	Args []Arg
}

// Interface describes a Wayland interface.
type Interface struct {
	Name     string
	Version  uint32
	Requests []*Message
	Events   []*Message
}

// Fixed is a 24.8 signed fixed-point number.
type Fixed float64

func (f Fixed) Float() float64 { return float64(f) }
func (f Fixed) Int() int       { return int(f) }

func fixedToWire(f Fixed) int32 { return int32(f * 256) }
func wireToFixed(v int32) Fixed { return Fixed(float64(v) / 256) }

// Dispatcher decodes requests for a resource. Implemented by generated types.
type Dispatcher interface {
	dispatch(opcode uint16, r *wireReader)
}

// EventDispatcher decodes events for a proxy. Implemented by generated types
// when the connection is a client.
type EventDispatcher interface {
	dispatchEvent(opcode uint16, r *wireReader)
}

// ClientGone is implemented by resource implementations that need to clean up
// when their client disconnects.
type ClientGone interface {
	ClientGone()
}

// Object is a Wayland protocol object living in a connection.
type Object struct {
	Conn      *Conn
	ID        uint32
	Iface     *Interface
	Version   uint32
	Impl      any
	res       any // the outer generated resource, implements Dispatcher
	destroyed bool

	// Data carries compositor-side per-resource state (shm images, pools,
	// role objects). It is never touched by the wire layer.
	Data any
}

// Destroy removes the object from its connection's object table and runs
// client-gone cleanup.
func (o *Object) Destroy() { o.Conn.destroyObject(o) }

// encodeArgs marshals a message's arguments according to its signature.
func encodeArgs(iface string, m *Message, args []any) (*wireWriter, error) {
	if len(args) != len(m.Args) {
		return nil, fmt.Errorf("wl: %s.%s: want %d args, got %d", iface, m.Name, len(m.Args), len(args))
	}
	w := &wireWriter{}
	for i, a := range args {
		switch m.Args[i].Type {
		case ArgInt:
			v, ok := a.(int32)
			if !ok {
				return nil, argErr(iface, m, a)
			}
			w.int32(v)
		case ArgUint:
			v, ok := a.(uint32)
			if !ok {
				return nil, argErr(iface, m, a)
			}
			w.uint32(v)
		case ArgFixed:
			v, ok := a.(Fixed)
			if !ok {
				return nil, argErr(iface, m, a)
			}
			w.int32(fixedToWire(v))
		case ArgString:
			v, ok := a.(string)
			if !ok {
				return nil, argErr(iface, m, a)
			}
			w.string(v)
		case ArgObject, ArgNewID:
			switch v := a.(type) {
			case *Object:
				if v == nil {
					w.uint32(0)
				} else {
					w.uint32(v.ID)
				}
			case nil:
				w.uint32(0)
			case uint32:
				w.uint32(v)
			default:
				id, ok := embeddedObjectID(a)
				if !ok {
					return nil, argErr(iface, m, a)
				}
				w.uint32(id)
			}
		case ArgArray:
			v, ok := a.([]byte)
			if !ok {
				return nil, argErr(iface, m, a)
			}
			w.array(v)
		case ArgFD:
			v, ok := a.(int)
			if !ok {
				return nil, argErr(iface, m, a)
			}
			w.fd(v)
		}
	}
	return w, nil
}

func argErr(iface string, m *Message, a any) error {
	return fmt.Errorf("wl: %s.%s: bad argument %T", iface, m.Name, a)
}

// embeddedObjectID returns the wire id of a generated resource type. Every
// resource embeds Object by value, and the generated send helpers pass those
// typed pointers (e.g. *WlSurface) rather than *Object. A typed nil pointer
// encodes as the null object id 0.
func embeddedObjectID(a any) (uint32, bool) {
	v := reflect.ValueOf(a)
	if !v.IsValid() || v.Kind() != reflect.Ptr {
		return 0, false
	}
	if v.IsNil() {
		return 0, true
	}
	obj := v.Elem().FieldByName("Object")
	if !obj.IsValid() || obj.Type() != reflect.TypeOf(Object{}) {
		return 0, false
	}
	return uint32(obj.FieldByName("ID").Uint()), true
}

func header(id uint32, opcode uint16, size int) []byte {
	hdr := make([]byte, 8)
	binary.LittleEndian.PutUint32(hdr, id)
	binary.LittleEndian.PutUint32(hdr[4:], uint32(size)<<16|uint32(opcode))
	return hdr
}

// send emits one event (compositor -> client).
func (o *Object) send(opcode uint16, args ...any) error {
	m := o.Iface.Events[opcode]
	w, err := encodeArgs(o.Iface.Name, m, args)
	if err != nil {
		return err
	}
	return o.Conn.queueEvent(append(header(o.ID, opcode, 8+len(w.buf)), w.buf...), w.fds)
}

// req emits one request (client -> compositor). Generated Req* methods call
// this; it is the mirror image of send.
func (o *Object) req(opcode uint16, args ...any) error {
	m := o.Iface.Requests[opcode]
	w, err := encodeArgs(o.Iface.Name, m, args)
	if err != nil {
		return err
	}
	return o.Conn.queueEvent(append(header(o.ID, opcode, 8+len(w.buf)), w.buf...), w.fds)
}

// ---- wire encoding ----

type wireWriter struct {
	buf []byte
	fds []int
}

func (w *wireWriter) uint32(v uint32) {
	w.buf = binary.LittleEndian.AppendUint32(w.buf, v)
}

func (w *wireWriter) int32(v int32) {
	w.buf = binary.LittleEndian.AppendUint32(w.buf, uint32(v))
}

func (w *wireWriter) string(s string) {
	w.uint32(uint32(len(s) + 1))
	w.buf = append(w.buf, s...)
	w.buf = append(w.buf, 0)
	for len(w.buf)%4 != 0 {
		w.buf = append(w.buf, 0)
	}
}

func (w *wireWriter) array(b []byte) {
	w.uint32(uint32(len(b)))
	w.buf = append(w.buf, b...)
	for len(w.buf)%4 != 0 {
		w.buf = append(w.buf, 0)
	}
}

func (w *wireWriter) fd(fd int) { w.fds = append(w.fds, fd) }

type wireReader struct {
	data []byte
	off  int
	fds  []int
	fi   int
	err  error
}

func (r *wireReader) fail(format string, args ...any) {
	if r.err == nil {
		r.err = fmt.Errorf(format, args...)
	}
}

func (r *wireReader) need(n int) bool {
	if r.err != nil {
		return false
	}
	if r.off+n > len(r.data) {
		r.fail("truncated message")
		return false
	}
	return true
}

func (r *wireReader) uint32() uint32 {
	if !r.need(4) {
		return 0
	}
	v := binary.LittleEndian.Uint32(r.data[r.off:])
	r.off += 4
	return v
}

func (r *wireReader) int32() int32 { return int32(r.uint32()) }

func (r *wireReader) fixed() Fixed { return wireToFixed(r.int32()) }

func (r *wireReader) string() string {
	n := int(r.uint32())
	if r.err != nil || n < 1 || !r.need(n) {
		if r.err == nil {
			r.fail("bad string length")
		}
		return ""
	}
	if r.data[r.off+n-1] != 0 {
		r.fail("unterminated string")
		return ""
	}
	s := string(r.data[r.off : r.off+n-1])
	r.off += n
	for r.off%4 != 0 {
		r.off++
	}
	return s
}

func (r *wireReader) array() []byte {
	n := int(r.uint32())
	if r.err != nil || !r.need(n) {
		if r.err == nil {
			r.fail("bad array length")
		}
		return nil
	}
	b := make([]byte, n)
	copy(b, r.data[r.off:r.off+n])
	r.off += n
	for r.off%4 != 0 {
		r.off++
	}
	return b
}

func (r *wireReader) fd() int {
	if r.fi >= len(r.fds) {
		r.fail("missing file descriptor")
		return -1
	}
	fd := r.fds[r.fi]
	r.fi++
	return fd
}
