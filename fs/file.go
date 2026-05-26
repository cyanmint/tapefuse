package fs

import (
	"context"
	"errors"
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
// (dirty=true), the data is served from the write buffer.  Otherwise it is
// read directly from tape.
func (h *FileHandle) Read(_ context.Context, req *fuse.ReadRequest, resp *fuse.ReadResponse) error {
	if req.Offset < 0 {
		return fuse.Errno(syscall.EINVAL)
	}

	h.mu.Lock()
	if h.dirty && h.wbuf != nil {
		data, err := h.wbuf.ReadRange(req.Offset, req.Size)
		h.mu.Unlock()
		if err != nil {
			return fuse.EIO
		}
		resp.Data = data
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

// Write accumulates data in a per-FileHandle write buffer (memory or file-
// backed, depending on the mount's BufferConfig).  No tape I/O occurs here;
// the complete file is written as a single tape block when the last file
// descriptor is closed (see flushBuffer, called from Flush and Release).
//
// This strategy matches how LTFS and LTFSCopyGUI handle writes: buffer the
// entire file and write it in one large tape block.  Writing one small block
// per FUSE write chunk (128 KB) wastes tape due to inter-block gaps and
// prevents the drive from reaching streaming speed.
//
// On the first write to an existing non-empty file the current tape content
// is pre-loaded into the buffer so that writes at arbitrary offsets are
// handled correctly.  The disk-cached index Length is updated on each Write
// so that stat() returns the correct size even before the data reaches tape.
func (h *FileHandle) Write(_ context.Context, req *fuse.WriteRequest, resp *fuse.WriteResponse) error {
	if req.Offset < 0 {
		return fuse.Errno(syscall.EINVAL)
	}
	if len(req.Data) == 0 {
		resp.Size = 0
		return nil
	}

	end := req.Offset + int64(len(req.Data))

	// On the first write, initialise the write buffer.  For an existing
	// non-empty file this also pre-loads the current tape content so that
	// writes at any offset are handled correctly.  Done outside h.mu to avoid
	// holding the handle lock during tape I/O.
	h.mu.Lock()
	needsInit := h.wbuf == nil
	h.mu.Unlock()

	if needsInit {
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
		if h.wbuf == nil {
			wbuf, err := newWriteBuf(h.fs.bufCfg, existing)
			if err != nil {
				h.mu.Unlock()
				if errors.Is(err, syscall.ENOSPC) {
					return fuse.Errno(syscall.ENOSPC)
				}
				return fuse.EIO
			}
			h.wbuf = wbuf
			h.fs.addHandle(h)
		}
		h.mu.Unlock()
	}

	// Accumulate the write into the buffer.
	h.mu.Lock()
	if err := h.wbuf.WriteAt(req.Data, req.Offset); err != nil {
		h.mu.Unlock()
		if errors.Is(err, syscall.ENOSPC) {
			return fuse.Errno(syscall.ENOSPC)
		}
		return fuse.EIO
	}
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

// flushBuffer writes the complete write-buffer contents to tape as a single
// block and updates the file's extent info in the disk-cached index.  It is a
// no-op if no writes are pending.
//
// On tape-write failure the buffer is restored (see restoreWBuf) so that a
// subsequent Release call can retry.
func (h *FileHandle) flushBuffer() error {
	h.mu.Lock()
	if !h.dirty || h.wbuf == nil {
		h.mu.Unlock()
		return nil
	}
	// Snapshot the buffer and optimistically mark as clean.  If the tape write
	// fails we restore the buffer via restoreWBuf.
	wbuf := h.wbuf
	h.wbuf = nil
	h.dirty = false
	h.mu.Unlock()

	h.fs.mu.Lock()
	defer h.fs.mu.Unlock()

	data, err := wbuf.Bytes()
	if err != nil {
		h.restoreWBuf(wbuf)
		return fuse.EIO
	}

	idx, err := h.fs.loadIndex()
	if err != nil {
		h.restoreWBuf(wbuf)
		return fuse.EIO
	}
	file, parent, err := idx.FindFile(h.path)
	if err != nil {
		_ = wbuf.Close()
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
			h.restoreWBuf(wbuf)
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

	_ = wbuf.Close()
	h.fs.removeHandle(h)
	return nil
}

// restoreWBuf puts wbuf back into h if no concurrent write has already
// started a new buffer since the flush attempt.  If a new buffer exists the
// old wbuf is closed to free its resources.
func (h *FileHandle) restoreWBuf(wbuf writeBuf) {
	h.mu.Lock()
	if !h.dirty {
		h.wbuf = wbuf
		h.dirty = true
	} else {
		// A concurrent write started a new buffer; discard the failed one.
		_ = wbuf.Close()
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

