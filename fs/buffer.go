package fs

import (
	"fmt"
	"io"
	"os"
	"syscall"
)

// fileBufferDir is the directory used for temporary write-buffer files when
// using file-backed buffering.
const fileBufferDir = "/tmp/ltape/buffer"

// BufferKind selects the write-buffer backing store for an FS mount.
type BufferKind int

const (
	// BufferKindFile (the default) backs each FileHandle's write buffer with a
	// temporary file under fileBufferDir.  This keeps large write-staging data
	// off the Go heap and allows the OS to page it to disk under memory
	// pressure.
	BufferKindFile BufferKind = iota

	// BufferKindMemory keeps the write buffer in a heap-allocated byte slice.
	BufferKindMemory

	// BufferKindStream bypasses the write buffer entirely and writes each FUSE
	// Write chunk directly to the tape data partition as it arrives.  This
	// delivers maximum streaming throughput for large sequential copies but
	// does not support random-offset writes: each chunk is appended to tape as
	// a separate extent.  Reads after a streaming write are served from the
	// tape itself (no local buffer is retained).
	BufferKindStream
)

// BufferConfig controls per-file-handle write-buffer behaviour for an FS
// mount.
//
//   - Kind     – selects memory or file-backed buffering.
//   - MaxBytes – per-file byte cap; 0 means no limit.
type BufferConfig struct {
	Kind     BufferKind
	MaxBytes int64
}

// DefaultBufferConfig returns the default configuration: file-backed with no
// size limit.  This is equivalent to passing -f0 at mount time.
func DefaultBufferConfig() BufferConfig {
	return BufferConfig{Kind: BufferKindFile, MaxBytes: 0}
}

// writeBuf is the per-FileHandle write-buffer abstraction.  The two
// implementations are memBuffer (heap []byte) and fileBuffer (OS temp file).
type writeBuf interface {
	// WriteAt writes p at byte offset off, extending the logical length if
	// necessary.  Returns syscall.ENOSPC when MaxBytes > 0 and the write
	// would exceed the cap.
	WriteAt(p []byte, off int64) error

	// ReadRange returns a copy of bytes in [off, off+n), clamped to Len().
	ReadRange(off int64, n int) ([]byte, error)

	// Len returns the current logical length of the buffer in bytes.
	Len() int64

	// Bytes returns the complete buffer contents.
	Bytes() ([]byte, error)

	// Close frees any resources held by the buffer (e.g. the temp file fd).
	Close() error
}

// newWriteBuf creates a writeBuf seeded with initial content according to cfg.
// Returns syscall.ENOSPC if initial already exceeds cfg.MaxBytes (> 0).
func newWriteBuf(cfg BufferConfig, initial []byte) (writeBuf, error) {
	switch cfg.Kind {
	case BufferKindMemory:
		return newMemBuffer(cfg.MaxBytes, initial)
	default:
		return newFileBuffer(fileBufferDir, cfg.MaxBytes, initial)
	}
}

// ---------------------------------------------------------------------------
// memBuffer – heap-allocated []byte implementation
// ---------------------------------------------------------------------------

// writeBufInitCap is the minimum initial allocation capacity (512 KiB) so
// that small files do not trigger repeated reallocations.
const writeBufInitCap = 512 * 1024

type memBuffer struct {
	data     []byte
	maxBytes int64
}

func newMemBuffer(maxBytes int64, initial []byte) (writeBuf, error) {
	if maxBytes > 0 && int64(len(initial)) > maxBytes {
		return nil, syscall.ENOSPC
	}
	capacity := int64(len(initial))
	if capacity < writeBufInitCap {
		capacity = writeBufInitCap
	}
	buf := make([]byte, len(initial), capacity)
	copy(buf, initial)
	return &memBuffer{data: buf, maxBytes: maxBytes}, nil
}

func (b *memBuffer) WriteAt(p []byte, off int64) error {
	end := off + int64(len(p))
	if b.maxBytes > 0 && end > b.maxBytes {
		return syscall.ENOSPC
	}
	if end > int64(len(b.data)) {
		newCap := int64(cap(b.data)) * 2
		if newCap < end {
			newCap = end
		}
		grown := make([]byte, end, newCap)
		copy(grown, b.data)
		b.data = grown
	}
	copy(b.data[off:end], p)
	return nil
}

func (b *memBuffer) ReadRange(off int64, n int) ([]byte, error) {
	if off >= int64(len(b.data)) {
		return []byte{}, nil
	}
	end := off + int64(n)
	if end > int64(len(b.data)) {
		end = int64(len(b.data))
	}
	return append([]byte(nil), b.data[off:end]...), nil
}

func (b *memBuffer) Len() int64             { return int64(len(b.data)) }
func (b *memBuffer) Bytes() ([]byte, error) { return b.data, nil }
func (b *memBuffer) Close() error           { return nil }

// ---------------------------------------------------------------------------
// fileBuffer – OS temp-file-backed implementation
// ---------------------------------------------------------------------------

type fileBuffer struct {
	f        *os.File
	len      int64
	maxBytes int64
}

func newFileBuffer(dir string, maxBytes int64, initial []byte) (writeBuf, error) {
	if maxBytes > 0 && int64(len(initial)) > maxBytes {
		return nil, syscall.ENOSPC
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, fmt.Errorf("create buffer dir %s: %w", dir, err)
	}
	f, err := os.CreateTemp(dir, "ltape-buf-*")
	if err != nil {
		return nil, fmt.Errorf("create temp buffer: %w", err)
	}
	// Unlink immediately so the file is removed on process exit even without
	// an explicit Close call (the fd keeps it alive while open).
	_ = os.Remove(f.Name())

	b := &fileBuffer{f: f, maxBytes: maxBytes}
	if len(initial) > 0 {
		if err := b.WriteAt(initial, 0); err != nil {
			_ = f.Close()
			return nil, err
		}
	}
	return b, nil
}

func (b *fileBuffer) WriteAt(p []byte, off int64) error {
	end := off + int64(len(p))
	if b.maxBytes > 0 && end > b.maxBytes {
		return syscall.ENOSPC
	}
	if _, err := b.f.WriteAt(p, off); err != nil {
		return err
	}
	if end > b.len {
		b.len = end
	}
	return nil
}

func (b *fileBuffer) ReadRange(off int64, n int) ([]byte, error) {
	if off >= b.len {
		return []byte{}, nil
	}
	end := off + int64(n)
	if end > b.len {
		end = b.len
	}
	buf := make([]byte, end-off)
	if _, err := b.f.ReadAt(buf, off); err != nil && err != io.EOF {
		return nil, err
	}
	return buf, nil
}

func (b *fileBuffer) Len() int64 { return b.len }

func (b *fileBuffer) Bytes() ([]byte, error) {
	if b.len == 0 {
		return []byte{}, nil
	}
	buf := make([]byte, b.len)
	if _, err := b.f.ReadAt(buf, 0); err != nil && err != io.EOF {
		return nil, err
	}
	return buf, nil
}

func (b *fileBuffer) Close() error { return b.f.Close() }
