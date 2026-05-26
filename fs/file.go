package fs

import (
	"context"
	"syscall"

	"bazil.org/fuse"
	bazilfs "bazil.org/fuse/fs"

	"github.com/cyanmint/tapefuse/internal/ltfs"
)

func (f *File) Attr(_ context.Context, a *fuse.Attr) error {
	f.fs.mu.RLock()
	defer f.fs.mu.RUnlock()

	idx, err := f.fs.loadIndex()
	if err != nil {
		return fuse.EIO
	}
	file, _, err := idx.FindFile(f.path)
	if err != nil {
		return fuse.ENOENT
	}
	a.Mode = 0o644
	a.Size = uint64(file.Length)
	a.Mtime = ltfs.ParseTime(file.ModifyTime)
	a.Ctime = ltfs.ParseTime(file.ChangeTime)
	a.Atime = ltfs.ParseTime(file.AccessTime)
	return nil
}

func (f *File) Open(_ context.Context, req *fuse.OpenRequest, _ *fuse.OpenResponse) (bazilfs.Handle, error) {
	f.fs.mu.RLock()
	defer f.fs.mu.RUnlock()

	idx, err := f.fs.loadIndex()
	if err != nil {
		return nil, fuse.EIO
	}
	file, _, err := idx.FindFile(f.path)
	if err != nil {
		return nil, fuse.ENOENT
	}
	data, err := f.fs.tape.ReadFileData(file)
	if err != nil {
		return nil, err
	}
	handle := &FileHandle{fs: f.fs, path: f.path, data: data}
	if req.Flags&fuse.OpenTruncate != 0 {
		handle.data = []byte{}
		handle.dirty = true
	}
	return handle, nil
}

func (f *File) Fsync(_ context.Context, _ *fuse.FsyncRequest) error {
	return nil
}

func (h *FileHandle) Read(_ context.Context, req *fuse.ReadRequest, resp *fuse.ReadResponse) error {
	if req.Offset < 0 {
		return fuse.Errno(syscall.EINVAL)
	}
	if req.Offset >= int64(len(h.data)) {
		resp.Data = []byte{}
		return nil
	}
	start := int(req.Offset)
	end := start + req.Size
	if end > len(h.data) {
		end = len(h.data)
	}
	resp.Data = append([]byte(nil), h.data[start:end]...)
	return nil
}

func (h *FileHandle) Write(_ context.Context, req *fuse.WriteRequest, resp *fuse.WriteResponse) error {
	if req.Offset < 0 {
		return fuse.Errno(syscall.EINVAL)
	}
	start := int(req.Offset)
	end := start + len(req.Data)
	if end < 0 {
		return fuse.Errno(syscall.EINVAL)
	}
	if end > len(h.data) {
		grown := make([]byte, end)
		copy(grown, h.data)
		h.data = grown
	}
	copy(h.data[start:end], req.Data)
	h.dirty = true
	resp.Size = len(req.Data)
	return nil
}

func (h *FileHandle) Release(_ context.Context, _ *fuse.ReleaseRequest) error {
	if !h.dirty {
		return nil
	}
	// Flush() should have persisted the data already.  If it was skipped or
	// failed, try one last time here.  FUSE does not wait for the RELEASE
	// reply, so any error is silently swallowed by the kernel; this is
	// best-effort only.
	h.fs.mu.Lock()
	defer h.fs.mu.Unlock()
	_ = h.writeLocked()
	return nil
}

func (h *FileHandle) Flush(_ context.Context, _ *fuse.FlushRequest) error {
	if !h.dirty {
		return nil
	}
	// FUSE_FLUSH is synchronous: the kernel waits for the reply before
	// close() returns to the caller.  Writing here ensures that the data
	// reaches the tape and the index is updated before cp (or any other
	// writer) sees close() succeed.
	h.fs.mu.Lock()
	defer h.fs.mu.Unlock()
	return h.writeLocked()
}

// writeLocked persists dirty data to tape and saves the updated index.
// It must be called with h.fs.mu held for writing.
// On full success it clears h.dirty so subsequent calls are no-ops.
func (h *FileHandle) writeLocked() error {
	idx, err := h.fs.loadIndex()
	if err != nil {
		return fuse.EIO
	}
	file, parent, err := idx.FindFile(h.path)
	if err != nil {
		return fuse.ENOENT
	}

	now := ltfs.Now()
	file.Length = int64(len(h.data))
	file.OpenForWrite = false
	if len(h.data) == 0 {
		file.ExtentInfo.Extents = nil
	} else {
		extent, err := h.fs.tape.WriteFileData(h.data, idx)
		if err != nil {
			return err
		}
		file.ExtentInfo.Extents = []ltfs.Extent{extent}
	}
	ltfs.TouchFile(file, now)
	ltfs.TouchDirectory(parent, now)
	if err := h.fs.saveIndex(idx); err != nil {
		return err
	}
	h.dirty = false
	return nil
}

func (ff *FlushFile) Attr(_ context.Context, a *fuse.Attr) error {
	a.Mode = 0o222
	a.Size = 0
	return nil
}

func (ff *FlushFile) Open(_ context.Context, _ *fuse.OpenRequest, _ *fuse.OpenResponse) (bazilfs.Handle, error) {
	return &FlushHandle{fs: ff.fs}, nil
}

func (h *FlushHandle) Read(_ context.Context, _ *fuse.ReadRequest, resp *fuse.ReadResponse) error {
	resp.Data = []byte{}
	return nil
}

func (h *FlushHandle) Write(_ context.Context, req *fuse.WriteRequest, resp *fuse.WriteResponse) error {
	h.fs.mu.Lock()
	defer h.fs.mu.Unlock()

	idx, err := h.fs.loadIndex()
	if err != nil {
		return fuse.EIO
	}
	if err := h.fs.tape.FlushIndex(idx); err != nil {
		return err
	}
	if err := h.fs.saveIndex(idx); err != nil {
		return err
	}
	resp.Size = len(req.Data)
	return nil
}

func (h *FlushHandle) Release(_ context.Context, _ *fuse.ReleaseRequest) error {
	return nil
}

func (h *FlushHandle) Flush(_ context.Context, _ *fuse.FlushRequest) error {
	return nil
}
