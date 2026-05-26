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

// Open returns a FileHandle for the file.  For a truncating open (O_TRUNC)
// all existing extents are freed (marked as available space) and the file
// length is reset to zero so subsequent writes start from a clean slate.
func (f *File) Open(_ context.Context, req *fuse.OpenRequest, _ *fuse.OpenResponse) (bazilfs.Handle, error) {
	if req.Flags&fuse.OpenTruncate == 0 {
		// Non-truncating open: just verify the file exists.
		f.fs.mu.RLock()
		defer f.fs.mu.RUnlock()
		idx, err := f.fs.loadIndex()
		if err != nil {
			return nil, fuse.EIO
		}
		if _, _, err := idx.FindFile(f.path); err != nil {
			return nil, fuse.ENOENT
		}
		return &FileHandle{fs: f.fs, path: f.path}, nil
	}

	// Truncating open: free existing extents and reset the file length.
	f.fs.mu.Lock()
	defer f.fs.mu.Unlock()
	idx, err := f.fs.loadIndex()
	if err != nil {
		return nil, fuse.EIO
	}
	file, _, err := idx.FindFile(f.path)
	if err != nil {
		return nil, fuse.ENOENT
	}
	for _, ext := range file.ExtentInfo.Extents {
		if ext.Partition == "b" && ext.ByteCount > 0 {
			physCap := f.fs.tape.DataBlockCapacity(uint64(ext.StartBlock))
			if physCap <= 0 {
				physCap = ext.ByteCount
			}
			idx.AvailableSpaces = append(idx.AvailableSpaces, ltfs.AvailableSpace{
				Partition:  ext.Partition,
				StartBlock: ext.StartBlock,
				ByteCount:  physCap,
			})
		}
	}
	file.ExtentInfo.Extents = nil
	file.Length = 0
	if err := f.fs.saveIndex(idx); err != nil {
		return nil, fuse.EIO
	}
	return &FileHandle{fs: f.fs, path: f.path}, nil
}

func (f *File) Fsync(_ context.Context, _ *fuse.FsyncRequest) error {
	return nil
}

// Read reads file data.  If there are pending buffered writes on this handle
// (dirty=true), the data is served from the in-memory buffer.  Otherwise it
// is read directly from tape.
func (h *FileHandle) Read(_ context.Context, req *fuse.ReadRequest, resp *fuse.ReadResponse) error {
	if req.Offset < 0 {
		return fuse.Errno(syscall.EINVAL)
	}

	h.mu.Lock()
	if h.dirty {
		defer h.mu.Unlock()
		if req.Offset >= int64(len(h.buf)) {
			resp.Data = []byte{}
			return nil
		}
		end := req.Offset + int64(req.Size)
		if end > int64(len(h.buf)) {
			end = int64(len(h.buf))
		}
		resp.Data = append([]byte(nil), h.buf[req.Offset:end]...)
		return nil
	}
	h.mu.Unlock()

	// No pending writes: read directly from tape.
	h.fs.mu.RLock()
	defer h.fs.mu.RUnlock()

	idx, err := h.fs.loadIndex()
	if err != nil {
		return fuse.EIO
	}
	file, _, err := idx.FindFile(h.path)
	if err != nil {
		return fuse.ENOENT
	}
	data, err := h.fs.tape.ReadFileRange(file, req.Offset, req.Size)
	if err != nil {
		return fuse.EIO
	}
	resp.Data = data
	return nil
}

// Write accumulates data in an in-memory buffer (per FileHandle).  No tape
// I/O occurs here; the complete file is written as a single tape block when
// the last file descriptor is closed (see flushBuffer, called from Flush and
// Release).
//
// This strategy matches how LTFS and LTFSCopyGUI handle writes: buffer the
// entire file in memory and write it in one large tape block.  Writing one
// small block per FUSE write chunk (128 KB) would waste tape due to
// inter-block gaps and reduces streaming performance.
//
// On the first write to an existing non-empty file the current content is
// pre-loaded from tape so that writes at arbitrary offsets are handled
// correctly.  The disk-cached index Length is updated on each Write so that
// stat() returns the correct size even before the data reaches tape.
func (h *FileHandle) Write(_ context.Context, req *fuse.WriteRequest, resp *fuse.WriteResponse) error {
	if req.Offset < 0 {
		return fuse.Errno(syscall.EINVAL)
	}
	if len(req.Data) == 0 {
		resp.Size = 0
		return nil
	}

	end := req.Offset + int64(len(req.Data))

	// On the first write to an existing non-empty file, pre-load the current
	// tape content so writes at any offset are handled correctly.  This is done
	// outside h.mu to avoid holding the handle lock during tape I/O.
	h.mu.Lock()
	needsPreload := !h.dirty && h.buf == nil
	h.mu.Unlock()

	if needsPreload {
		h.fs.mu.RLock()
		idx, err := h.fs.loadIndex()
		if err != nil {
			h.fs.mu.RUnlock()
			return fuse.EIO
		}
		file, _, err := idx.FindFile(h.path)
		if err != nil {
			h.fs.mu.RUnlock()
			return fuse.ENOENT
		}
		var existing []byte
		if file.Length > 0 {
			existing, err = h.fs.tape.ReadFileData(file)
			if err != nil {
				h.fs.mu.RUnlock()
				return fuse.EIO
			}
		}
		h.fs.mu.RUnlock()

		h.mu.Lock()
		if h.buf == nil {
			h.buf = existing
		}
		h.mu.Unlock()
	}

	// Accumulate the write into the in-memory buffer.
	h.mu.Lock()
	if end > int64(len(h.buf)) {
		grown := make([]byte, end, max(end, writeBufferSize))
		copy(grown, h.buf)
		h.buf = grown
	}
	copy(h.buf[req.Offset:end], req.Data)
	h.dirty = true
	h.mu.Unlock()

	// Update the disk-cached index Length so that concurrent stat() calls
	// return the correct file size while the file is being written.
	h.fs.mu.Lock()
	defer h.fs.mu.Unlock()
	idx, err := h.fs.loadIndex()
	if err != nil {
		return fuse.EIO
	}
	file, parent, err := idx.FindFile(h.path)
	if err != nil {
		return fuse.ENOENT
	}
	if end > file.Length {
		file.Length = end
		now := ltfs.Now()
		ltfs.TouchFile(file, now)
		ltfs.TouchDirectory(parent, now)
		if err := h.fs.saveIndex(idx); err != nil {
			return fuse.EIO
		}
	}
	resp.Size = len(req.Data)
	return nil
}

// flushBuffer writes the complete in-memory buffer to tape as a single block
// and updates the file's extent info in the disk-cached index.  It is a no-op
// if no writes are pending.
//
// On write failure the buffer is restored so that a subsequent Release call
// can retry.
func (h *FileHandle) flushBuffer() error {
	h.mu.Lock()
	if !h.dirty {
		h.mu.Unlock()
		return nil
	}
	// Snapshot the buffer and optimistically mark as clean.  If the tape write
	// fails we restore the buffer (see restoreBuffer).
	data := h.buf
	h.buf = nil
	h.dirty = false
	h.mu.Unlock()

	h.fs.mu.Lock()
	defer h.fs.mu.Unlock()

	idx, err := h.fs.loadIndex()
	if err != nil {
		h.restoreBuffer(data)
		return fuse.EIO
	}
	file, parent, err := idx.FindFile(h.path)
	if err != nil {
		h.restoreBuffer(data)
		return fuse.ENOENT
	}

	// Free existing tape extents so the space can be reused.
	for _, ext := range file.ExtentInfo.Extents {
		if ext.Partition == "b" && ext.ByteCount > 0 {
			physCap := h.fs.tape.DataBlockCapacity(uint64(ext.StartBlock))
			if physCap <= 0 {
				physCap = ext.ByteCount
			}
			idx.AvailableSpaces = append(idx.AvailableSpaces, ltfs.AvailableSpace{
				Partition:  ext.Partition,
				StartBlock: ext.StartBlock,
				ByteCount:  physCap,
			})
		}
	}
	file.ExtentInfo.Extents = nil

	if len(data) > 0 {
		// Write the complete file as a single tape block.
		extent, err := h.fs.tape.AppendFileData(data)
		if err != nil {
			h.restoreBuffer(data)
			return err
		}
		extent.FileOffset = 0
		file.ExtentInfo.Extents = []ltfs.Extent{extent}
	}
	file.Length = int64(len(data))

	now := ltfs.Now()
	ltfs.TouchFile(file, now)
	ltfs.TouchDirectory(parent, now)
	if err := h.fs.saveIndex(idx); err != nil {
		return fuse.EIO
	}
	return nil
}

// restoreBuffer puts data back into h.buf if no concurrent write has already
// started a new buffer since the flush attempt.
func (h *FileHandle) restoreBuffer(data []byte) {
	h.mu.Lock()
	if !h.dirty {
		h.buf = data
		h.dirty = true
	}
	h.mu.Unlock()
}

// Flush is called when a file descriptor is closed.  It writes the complete
// buffered file content to tape as a single block.
func (h *FileHandle) Flush(_ context.Context, _ *fuse.FlushRequest) error {
	return h.flushBuffer()
}

// Release is called when the last reference to the file is gone.  It flushes
// any remaining buffered data, which also handles a retry if Flush failed.
func (h *FileHandle) Release(_ context.Context, _ *fuse.ReleaseRequest) error {
	return h.flushBuffer()
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

