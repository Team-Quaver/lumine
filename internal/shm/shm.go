// Package shm maps wl_shm client buffers into readable pixel data without
// touching libwayland. Buffers arrive as file descriptors shared over the
// wire; we mmap them and decode the declared format.
package shm

import (
	"fmt"
	"syscall"

	"golang.org/x/sys/unix"
)

// Format is the subset of wl_shm.format the renderer consumes.
type Format uint32

const (
	ARGB8888 Format = 0
	XRGB8888 Format = 1
)

// Image is a decoded client buffer.
//
// Pix aliases the pool mapping: it is cheap to hand out but only valid while
// the pool still holds the mapping, which is why every Image owns a reference
// on its pool (see Pool.refs). Callers that need the pixels to outlive the
// buffer must copy them (see Snapshot).
type Image struct {
	Pix    []byte // little-endian ARGB8888/XRGB8888
	Width  int
	Height int
	Stride int
	Format Format

	pool *Pool // mapping this Pix points into, nil for owned copies
}

// Pool maps a wl_shm pool fd into memory.
//
// A pool's mapping must outlive the wl_shm_pool object itself: the protocol
// explicitly allows a client to destroy the pool while buffers created from it
// are still in use, and most clients (kitty, GTK, smithay) do exactly that.
// Lifetime is therefore refcounted — every Image takes a reference, and the
// mapping is unmapped only once the pool is destroyed and every buffer created
// from it has been released.
type Pool struct {
	// Data is the most recent mapping. Mappings replaced by wl_shm_pool.resize
	// are kept in extra so slices already handed out stay valid.
	Data  []byte
	extra [][]byte
	fd    int

	refs      int
	destroyed bool
}

// MapPool maps a client shm fd. It takes ownership of fd on success; the
// caller must close it when MapPool returns an error.
func MapPool(fd int, size int32) (*Pool, error) {
	if size <= 0 {
		return nil, fmt.Errorf("shm: bad pool size %d", size)
	}
	// The declared size must fit inside the underlying file. Mapping past EOF
	// would only fail later, as a SIGBUS on first touch, so reject it now.
	var st unix.Stat_t
	if err := unix.Fstat(fd, &st); err != nil {
		return nil, fmt.Errorf("shm: fstat: %w", err)
	}
	if int64(size) > st.Size {
		return nil, fmt.Errorf("shm: pool size %d exceeds backing file %d", size, st.Size)
	}
	data, err := syscall.Mmap(fd, 0, int(size), syscall.PROT_READ, syscall.MAP_SHARED)
	if err != nil {
		return nil, fmt.Errorf("shm: mmap %d bytes: %w", size, err)
	}
	return &Pool{Data: data, fd: fd}, nil
}

// Resize grows the pool mapping after wl_shm_pool.resize. The client has
// already ftruncated the backing file, so the extra pages are ours to map. The
// previous mapping is kept alive: buffers created before the resize hold
// slices into it and must keep working.
func (p *Pool) Resize(newSize int32) error {
	if newSize <= 0 || int(newSize) <= len(p.Data) {
		return fmt.Errorf("shm: resize to %d must grow the pool", newSize)
	}
	var st unix.Stat_t
	if err := unix.Fstat(p.fd, &st); err != nil {
		return fmt.Errorf("shm: fstat: %w", err)
	}
	if int64(newSize) > st.Size {
		return fmt.Errorf("shm: resize to %d exceeds backing file %d", newSize, st.Size)
	}
	newData, err := syscall.Mmap(p.fd, 0, int(newSize), syscall.PROT_READ, syscall.MAP_SHARED)
	if err != nil {
		return err
	}
	if p.Data != nil {
		p.extra = append(p.extra, p.Data)
	}
	p.Data = newData
	return nil
}

// Image slices a buffer out of a pool. It copies nothing; the returned Image
// aliases the mapping and takes a reference on the pool, which the caller must
// drop with Image.Release once the pixels are no longer needed.
func (p *Pool) Image(offset int32, width, height, stride int32, format Format) (*Image, error) {
	if width <= 0 || height <= 0 || stride < width*4 || offset < 0 {
		return nil, fmt.Errorf("shm: bad buffer geometry %dx%d stride %d offset %d", width, height, stride, offset)
	}
	if p.Data == nil {
		return nil, fmt.Errorf("shm: pool is closed")
	}
	length := int64(stride) * int64(height)
	end := int64(offset) + length
	if end > int64(len(p.Data)) {
		return nil, fmt.Errorf("shm: buffer exceeds pool (%d > %d)", end, len(p.Data))
	}
	p.refs++
	return &Image{
		Pix: p.Data[offset:end], Width: int(width), Height: int(height),
		Stride: int(stride), Format: format, pool: p,
	}, nil
}

// Destroy is wl_shm_pool.destroy. The mapping stays until the last buffer
// created from this pool is released.
func (p *Pool) Destroy() {
	p.destroyed = true
	if p.refs <= 0 {
		p.close()
	}
}

// Release drops one buffer reference. Called when a wl_buffer is destroyed or
// when the compositor is otherwise done with an Image.
func (p *Pool) Release() {
	if p.refs > 0 {
		p.refs--
	}
	if p.destroyed && p.refs <= 0 {
		p.close()
	}
}

func (p *Pool) close() {
	for _, m := range p.extra {
		_ = syscall.Munmap(m)
	}
	p.extra = nil
	if p.Data != nil {
		_ = syscall.Munmap(p.Data)
		p.Data = nil
	}
	if p.fd >= 0 {
		_ = unix.Close(p.fd)
		p.fd = -1
	}
}

// Release drops this image's reference on its pool mapping. It is safe to call
// more than once and on owned copies (which hold no reference).
func (img *Image) Release() {
	if img == nil || img.pool == nil {
		return
	}
	img.pool.Release()
	img.pool = nil
}

// Snapshot returns an owned copy of the image pixels. The copy is detached
// from the pool, so it stays valid after the client's buffers are gone.
func (img *Image) Snapshot() *Image {
	pix := make([]byte, len(img.Pix))
	copy(pix, img.Pix)
	return &Image{Pix: pix, Width: img.Width, Height: img.Height, Stride: img.Stride, Format: img.Format}
}
