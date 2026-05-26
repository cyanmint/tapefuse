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

// Read reads file data directly from tape using the file's extent list.
// No in-memory file cache is kept; every read hits the tape (or its OS
// page-cache equivalent for file-backed partitions).
func (h *FileHandle) Read(_ context.Context, req *fuse.ReadRequest, resp *fuse.ReadResponse) error {
if req.Offset < 0 {
return fuse.Errno(syscall.EINVAL)
}
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

// Write writes data directly to tape.  There is no in-memory write buffer:
// each FUSE Write request produces an immediate tape write.
//
// Two strategies are used depending on the write offset:
//
//   - Append (offset >= file.Length, or file has no extents): the chunk is
//     appended at the end of the data partition via AppendFileData and a new
//     extent is added to the file.  This covers all writes to newly-created
//     and truncated files, including sequential cp(1) copies.
//
//   - In-place edit (offset < file.Length): the entire current file content is
//     read from tape, the chunk is patched in at the requested offset, all old
//     extents are freed as available space, and the merged content is written
//     back as a single new block via WriteFileData (which may reuse a freed
//     block).
//
// Each AppendFileData call writes a trailing filemark followed by the EOD
// marker directly to tape, so all data is durable without an additional sync.
func (h *FileHandle) Write(_ context.Context, req *fuse.WriteRequest, resp *fuse.WriteResponse) error {
if req.Offset < 0 {
return fuse.Errno(syscall.EINVAL)
}
if len(req.Data) == 0 {
resp.Size = 0
return nil
}

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

if err := h.fs.tape.WriteChunkToFile(file, req.Offset, req.Data, idx); err != nil {
return err
}

newEnd := req.Offset + int64(len(req.Data))
if newEnd > file.Length {
file.Length = newEnd
}
now := ltfs.Now()
ltfs.TouchFile(file, now)
ltfs.TouchDirectory(parent, now)
if err := h.fs.saveIndex(idx); err != nil {
return fuse.EIO
}
resp.Size = len(req.Data)
return nil
}

// Flush is called by FUSE when the file descriptor is closed.  Because all
// writes are committed to tape and the index is persisted inside Write(),
// there is nothing left to do here.
func (h *FileHandle) Flush(_ context.Context, _ *fuse.FlushRequest) error {
return nil
}

func (h *FileHandle) Release(_ context.Context, _ *fuse.ReleaseRequest) error {
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
