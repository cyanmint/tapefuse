package fs

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path"
	"path/filepath"
	"strings"
	"sync"
	"syscall"

	"bazil.org/fuse"
	bazilfs "bazil.org/fuse/fs"
	"github.com/google/uuid"

	"github.com/cyanmint/tapefuse/internal/ltfs"
	"github.com/cyanmint/tapefuse/internal/tape"
)

const flushFileName = ".tapefuse_flush"

type TapeFS struct {
	Device         string
	IndexPartition *tape.Partition
	DataPartition  *tape.Partition
	IndexLabel     *ltfs.Label
	DataLabel      *ltfs.Label
	Index          *ltfs.Index
}

type FS struct {
	tape  *TapeFS
	index *ltfs.Index
	mu    sync.RWMutex
}

type Dir struct {
	fs   *FS
	path string
}

type File struct {
	fs   *FS
	path string
}

type FileHandle struct {
	fs    *FS
	path  string
	data  []byte
	dirty bool
}

type FlushFile struct {
	fs *FS
}

type FlushHandle struct {
	fs *FS
}

func OpenTape(device string) (*TapeFS, error) {
	p0 := device + ".p0.dat"
	p1 := device + ".p1.dat"

	_, err0 := os.Stat(p0)
	_, err1 := os.Stat(p1)
	if errors.Is(err0, os.ErrNotExist) && errors.Is(err1, os.ErrNotExist) {
		return formatTape(device)
	}
	if err0 != nil && !errors.Is(err0, os.ErrNotExist) {
		return nil, err0
	}
	if err1 != nil && !errors.Is(err1, os.ErrNotExist) {
		return nil, err1
	}
	if errors.Is(err0, os.ErrNotExist) || errors.Is(err1, os.ErrNotExist) {
		return nil, fmt.Errorf("tape image is incomplete for device %q", device)
	}

	idxPart, err := tape.Open(p0)
	if err != nil {
		return nil, err
	}
	dataPart, err := tape.Open(p1)
	if err != nil {
		_ = idxPart.Close()
		return nil, err
	}

	closeBoth := func() {
		_ = idxPart.Close()
		_ = dataPart.Close()
	}

	idxLabelRec, err := idxPart.ReadAt(1)
	if err != nil {
		closeBoth()
		return nil, err
	}
	idxLabel, err := ltfs.ParseLabel(idxLabelRec.Data)
	if err != nil {
		closeBoth()
		return nil, err
	}

	dataLabelRec, err := dataPart.ReadAt(1)
	if err != nil {
		closeBoth()
		return nil, err
	}
	dataLabel, err := ltfs.ParseLabel(dataLabelRec.Data)
	if err != nil {
		closeBoth()
		return nil, err
	}

	indexRec, err := idxPart.ReadAt(3)
	if err != nil {
		closeBoth()
		return nil, err
	}
	index, err := ltfs.ParseIndex(indexRec.Data)
	if err != nil {
		closeBoth()
		return nil, err
	}

	return &TapeFS{
		Device:         device,
		IndexPartition: idxPart,
		DataPartition:  dataPart,
		IndexLabel:     idxLabel,
		DataLabel:      dataLabel,
		Index:          index,
	}, nil
}

func formatTape(device string) (*TapeFS, error) {
	p0 := device + ".p0.dat"
	p1 := device + ".p1.dat"

	idxPart, err := tape.Create(p0)
	if err != nil {
		return nil, err
	}
	dataPart, err := tape.Create(p1)
	if err != nil {
		_ = idxPart.Close()
		return nil, err
	}

	closeBoth := func() {
		_ = idxPart.Close()
		_ = dataPart.Close()
	}

	volumeUUID := uuid.NewString()
	idxLabel := ltfs.NewLabel("a", volumeUUID)
	dataLabel := ltfs.NewLabel("b", volumeUUID)
	index := ltfs.NewEmptyIndex(volumeUUID)

	vol1 := makeVOL1Label(device)
	idxLabelXML, err := idxLabel.Marshal()
	if err != nil {
		closeBoth()
		return nil, err
	}
	dataLabelXML, err := dataLabel.Marshal()
	if err != nil {
		closeBoth()
		return nil, err
	}
	indexXML, err := index.Marshal()
	if err != nil {
		closeBoth()
		return nil, err
	}

	if _, err := idxPart.WriteRecord(tape.Record{Type: tape.RecordData, Data: vol1}); err != nil {
		closeBoth()
		return nil, err
	}
	if _, err := idxPart.WriteRecord(tape.Record{Type: tape.RecordData, Data: idxLabelXML}); err != nil {
		closeBoth()
		return nil, err
	}
	if _, err := idxPart.WriteFilemark(); err != nil {
		closeBoth()
		return nil, err
	}
	if _, err := idxPart.WriteRecord(tape.Record{Type: tape.RecordData, Data: indexXML}); err != nil {
		closeBoth()
		return nil, err
	}
	if _, err := idxPart.WriteFilemark(); err != nil {
		closeBoth()
		return nil, err
	}
	if _, err := idxPart.WriteFilemark(); err != nil {
		closeBoth()
		return nil, err
	}

	if _, err := dataPart.WriteRecord(tape.Record{Type: tape.RecordData, Data: vol1}); err != nil {
		closeBoth()
		return nil, err
	}
	if _, err := dataPart.WriteRecord(tape.Record{Type: tape.RecordData, Data: dataLabelXML}); err != nil {
		closeBoth()
		return nil, err
	}
	if _, err := dataPart.WriteFilemark(); err != nil {
		closeBoth()
		return nil, err
	}
	if _, err := dataPart.WriteEOD(); err != nil {
		closeBoth()
		return nil, err
	}

	if err := idxPart.Sync(); err != nil {
		closeBoth()
		return nil, err
	}
	if err := dataPart.Sync(); err != nil {
		closeBoth()
		return nil, err
	}

	return &TapeFS{
		Device:         device,
		IndexPartition: idxPart,
		DataPartition:  dataPart,
		IndexLabel:     idxLabel,
		DataLabel:      dataLabel,
		Index:          index,
	}, nil
}

func New(tapeFS *TapeFS) *FS {
	return &FS{tape: tapeFS, index: tapeFS.Index}
}

func (f *FS) Root() (bazilfs.Node, error) {
	return &Dir{fs: f, path: "/"}, nil
}

func (f *FS) FlushIndex() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tape.FlushIndex()
}

func (f *FS) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tape.Close()
}

func (t *TapeFS) Close() error {
	var firstErr error
	if err := t.IndexPartition.Close(); err != nil && firstErr == nil {
		firstErr = err
	}
	if err := t.DataPartition.Close(); err != nil && firstErr == nil {
		firstErr = err
	}
	return firstErr
}

func (t *TapeFS) FlushIndex() error {
	prev := t.Index.Location
	t.Index.GenerationNumber++
	t.Index.UpdateTime = ltfs.Now()
	t.Index.PrevGenLocation = prev
	t.Index.Location = ltfs.Location{Partition: "a", StartBlock: 3}

	data, err := t.Index.Marshal()
	if err != nil {
		return err
	}
	if err := t.IndexPartition.TruncateAt(3); err != nil {
		return err
	}
	blockNum, err := t.IndexPartition.WriteRecord(tape.Record{Type: tape.RecordData, Data: data})
	if err != nil {
		return err
	}
	t.Index.Location = ltfs.Location{Partition: "a", StartBlock: blockNum}
	if _, err := t.IndexPartition.WriteFilemark(); err != nil {
		return err
	}
	if _, err := t.IndexPartition.WriteFilemark(); err != nil {
		return err
	}
	return t.IndexPartition.Sync()
}

func (t *TapeFS) ReadFileData(file *ltfs.File) ([]byte, error) {
	return t.ReadFileRange(file, 0, int(file.Length))
}

func (t *TapeFS) ReadFileRange(file *ltfs.File, offset int64, size int) ([]byte, error) {
	if size <= 0 || offset >= file.Length {
		return []byte{}, nil
	}
	if offset < 0 {
		return nil, fmt.Errorf("negative offset")
	}

	maxEnd := offset + int64(size)
	if maxEnd > file.Length {
		maxEnd = file.Length
	}

	out := make([]byte, 0, maxEnd-offset)
	for _, extent := range file.ExtentInfo.Extents {
		extentStart := extent.FileOffset
		extentEnd := extent.FileOffset + extent.ByteCount
		if maxEnd <= extentStart || offset >= extentEnd {
			continue
		}

		part := t.partitionFor(extent.Partition)
		if part == nil {
			return nil, fmt.Errorf("unknown partition %q", extent.Partition)
		}
		rec, err := part.ReadAt(uint64(extent.StartBlock))
		if err != nil {
			return nil, err
		}
		if rec.Type != tape.RecordData {
			return nil, fmt.Errorf("block %d is not a data record", extent.StartBlock)
		}

		dataStart := extent.ByteOffset
		dataEnd := extent.ByteOffset + extent.ByteCount
		if dataStart < 0 || dataEnd < dataStart || dataEnd > int64(len(rec.Data)) {
			return nil, fmt.Errorf("extent exceeds record bounds")
		}
		recordData := rec.Data[dataStart:dataEnd]

		copyStart := max64(offset, extentStart)
		copyEnd := min64(maxEnd, extentEnd)
		if copyEnd <= copyStart {
			continue
		}
		startInExtent := copyStart - extentStart
		endInExtent := copyEnd - extentStart
		out = append(out, recordData[startInExtent:endInExtent]...)
	}
	return out, nil
}

func (t *TapeFS) AppendFileData(data []byte) (ltfs.Extent, error) {
	if lastType, ok, err := t.DataPartition.LastRecordType(); err != nil {
		return ltfs.Extent{}, err
	} else if ok && lastType == tape.RecordEOD {
		if err := t.DataPartition.TruncateAt(t.DataPartition.BlockCount() - 1); err != nil {
			return ltfs.Extent{}, err
		}
	}

	blockNum, err := t.DataPartition.WriteRecord(tape.Record{Type: tape.RecordData, Data: data})
	if err != nil {
		return ltfs.Extent{}, err
	}
	if _, err := t.DataPartition.WriteFilemark(); err != nil {
		return ltfs.Extent{}, err
	}
	if _, err := t.DataPartition.WriteEOD(); err != nil {
		return ltfs.Extent{}, err
	}
	if err := t.DataPartition.Sync(); err != nil {
		return ltfs.Extent{}, err
	}

	return ltfs.Extent{
		FileOffset: 0,
		Partition:  "b",
		StartBlock: int64(blockNum),
		ByteOffset: 0,
		ByteCount:  int64(len(data)),
	}, nil
}

func (t *TapeFS) partitionFor(partition string) *tape.Partition {
	switch partition {
	case "a":
		return t.IndexPartition
	case "b":
		return t.DataPartition
	default:
		return nil
	}
}

func makeVOL1Label(device string) []byte {
	barcode := sanitizeBarcode(filepath.Base(device))
	label := make([]byte, 80)
	for i := range label {
		label[i] = ' '
	}
	copy(label[0:4], []byte("VOL1"))
	copy(label[4:10], []byte(barcode))
	copy(label[24:28], []byte("LTFS"))
	return label
}

func sanitizeBarcode(base string) string {
	upper := strings.ToUpper(base)
	var b strings.Builder
	for _, r := range upper {
		if (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') {
			b.WriteRune(r)
		}
		if b.Len() == 6 {
			break
		}
	}
	for b.Len() < 6 {
		b.WriteByte(' ')
	}
	return b.String()
}

func joinPath(parent, name string) string {
	if parent == "/" {
		return path.Join("/", name)
	}
	return path.Join(parent, name)
}

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}

func (d *Dir) Attr(_ context.Context, a *fuse.Attr) error {
	d.fs.mu.RLock()
	defer d.fs.mu.RUnlock()

	dir := d.fs.index.FindDirectory(d.path)
	if dir == nil {
		return fuse.ENOENT
	}
	a.Mode = os.ModeDir | 0o755
	a.Mtime = ltfs.ParseTime(dir.ModifyTime)
	a.Ctime = ltfs.ParseTime(dir.ChangeTime)
	a.Atime = ltfs.ParseTime(dir.AccessTime)
	return nil
}

func (d *Dir) Lookup(_ context.Context, name string) (bazilfs.Node, error) {
	if d.path == "/" && name == flushFileName {
		return &FlushFile{fs: d.fs}, nil
	}

	d.fs.mu.RLock()
	defer d.fs.mu.RUnlock()

	dir := d.fs.index.FindDirectory(d.path)
	if dir == nil {
		return nil, fuse.ENOENT
	}
	if dir.FindDirectory(name) != nil {
		return &Dir{fs: d.fs, path: joinPath(d.path, name)}, nil
	}
	if dir.FindFile(name) != nil {
		return &File{fs: d.fs, path: joinPath(d.path, name)}, nil
	}
	return nil, fuse.ENOENT
}

func (d *Dir) ReadDirAll(_ context.Context) ([]fuse.Dirent, error) {
	d.fs.mu.RLock()
	defer d.fs.mu.RUnlock()

	dir := d.fs.index.FindDirectory(d.path)
	if dir == nil {
		return nil, fuse.ENOENT
	}

	out := make([]fuse.Dirent, 0, len(dir.Contents.Directories)+len(dir.Contents.Files)+1)
	for _, child := range dir.Contents.Directories {
		out = append(out, fuse.Dirent{Name: child.Name, Type: fuse.DT_Dir})
	}
	for _, child := range dir.Contents.Files {
		out = append(out, fuse.Dirent{Name: child.Name, Type: fuse.DT_File})
	}
	if d.path == "/" {
		out = append(out, fuse.Dirent{Name: flushFileName, Type: fuse.DT_File})
	}
	return out, nil
}

func (d *Dir) Create(_ context.Context, req *fuse.CreateRequest, _ *fuse.CreateResponse) (bazilfs.Node, bazilfs.Handle, error) {
	if req.Name == flushFileName {
		return nil, nil, fuse.Errno(syscall.EPERM)
	}

	d.fs.mu.Lock()
	defer d.fs.mu.Unlock()

	dir := d.fs.index.FindDirectory(d.path)
	if dir == nil {
		return nil, nil, fuse.ENOENT
	}
	if dir.FindDirectory(req.Name) != nil || dir.FindFile(req.Name) != nil {
		return nil, nil, fuse.EEXIST
	}

	file := ltfs.NewFile(req.Name, d.fs.index.NextFileUID())
	dir.Contents.Files = append(dir.Contents.Files, file)
	ltfs.TouchDirectory(dir, ltfs.Now())

	childPath := joinPath(d.path, req.Name)
	handle := &FileHandle{fs: d.fs, path: childPath, data: []byte{}}
	return &File{fs: d.fs, path: childPath}, handle, nil
}

func (d *Dir) Mkdir(_ context.Context, req *fuse.MkdirRequest) (bazilfs.Node, error) {
	if req.Name == flushFileName {
		return nil, fuse.Errno(syscall.EPERM)
	}

	d.fs.mu.Lock()
	defer d.fs.mu.Unlock()

	dir := d.fs.index.FindDirectory(d.path)
	if dir == nil {
		return nil, fuse.ENOENT
	}
	if dir.FindDirectory(req.Name) != nil || dir.FindFile(req.Name) != nil {
		return nil, fuse.EEXIST
	}

	child := ltfs.NewDirectory(req.Name)
	dir.Contents.Directories = append(dir.Contents.Directories, child)
	ltfs.TouchDirectory(dir, ltfs.Now())
	return &Dir{fs: d.fs, path: joinPath(d.path, req.Name)}, nil
}

func (d *Dir) Remove(_ context.Context, req *fuse.RemoveRequest) error {
	if d.path == "/" && req.Name == flushFileName {
		return fuse.Errno(syscall.EPERM)
	}

	d.fs.mu.Lock()
	defer d.fs.mu.Unlock()

	dir := d.fs.index.FindDirectory(d.path)
	if dir == nil {
		return fuse.ENOENT
	}

	now := ltfs.Now()
	if req.Dir {
		for i, child := range dir.Contents.Directories {
			if child.Name != req.Name {
				continue
			}
			if len(child.Contents.Directories) > 0 || len(child.Contents.Files) > 0 {
				return fuse.Errno(syscall.ENOTEMPTY)
			}
			dir.Contents.Directories = append(dir.Contents.Directories[:i], dir.Contents.Directories[i+1:]...)
			ltfs.TouchDirectory(dir, now)
			return nil
		}
		return fuse.ENOENT
	}

	for i, child := range dir.Contents.Files {
		if child.Name != req.Name {
			continue
		}
		dir.Contents.Files = append(dir.Contents.Files[:i], dir.Contents.Files[i+1:]...)
		ltfs.TouchDirectory(dir, now)
		return nil
	}
	return fuse.ENOENT
}

func (d *Dir) Rename(_ context.Context, req *fuse.RenameRequest, newDir bazilfs.Node) error {
	targetDir, ok := newDir.(*Dir)
	if !ok {
		return fuse.Errno(syscall.EIO)
	}
	if req.OldName == flushFileName || (targetDir.path == "/" && req.NewName == flushFileName) {
		return fuse.Errno(syscall.EPERM)
	}

	d.fs.mu.Lock()
	defer d.fs.mu.Unlock()

	oldParent := d.fs.index.FindDirectory(d.path)
	newParent := d.fs.index.FindDirectory(targetDir.path)
	if oldParent == nil || newParent == nil {
		return fuse.ENOENT
	}
	if newParent.FindDirectory(req.NewName) != nil || newParent.FindFile(req.NewName) != nil {
		return fuse.EEXIST
	}

	oldPath := joinPath(d.path, req.OldName)
	newPath := joinPath(targetDir.path, req.NewName)
	if strings.HasPrefix(newPath+"/", oldPath+"/") {
		return fuse.Errno(syscall.EINVAL)
	}

	now := ltfs.Now()
	for i, child := range oldParent.Contents.Files {
		if child.Name != req.OldName {
			continue
		}
		oldParent.Contents.Files = append(oldParent.Contents.Files[:i], oldParent.Contents.Files[i+1:]...)
		child.Name = req.NewName
		child.ChangeTime = now
		child.ModifyTime = now
		newParent.Contents.Files = append(newParent.Contents.Files, child)
		ltfs.TouchDirectory(oldParent, now)
		if oldParent != newParent {
			ltfs.TouchDirectory(newParent, now)
		}
		return nil
	}

	for i, child := range oldParent.Contents.Directories {
		if child.Name != req.OldName {
			continue
		}
		oldParent.Contents.Directories = append(oldParent.Contents.Directories[:i], oldParent.Contents.Directories[i+1:]...)
		child.Name = req.NewName
		child.ChangeTime = now
		child.ModifyTime = now
		newParent.Contents.Directories = append(newParent.Contents.Directories, child)
		ltfs.TouchDirectory(oldParent, now)
		if oldParent != newParent {
			ltfs.TouchDirectory(newParent, now)
		}
		return nil
	}

	return fuse.ENOENT
}

func (f *File) Attr(_ context.Context, a *fuse.Attr) error {
	f.fs.mu.RLock()
	defer f.fs.mu.RUnlock()

	file, _, err := f.fs.index.FindFile(f.path)
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

	file, _, err := f.fs.index.FindFile(f.path)
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

	h.fs.mu.Lock()
	defer h.fs.mu.Unlock()

	file, parent, err := h.fs.index.FindFile(h.path)
	if err != nil {
		return fuse.ENOENT
	}

	now := ltfs.Now()
	file.Length = int64(len(h.data))
	file.OpenForWrite = false
	if len(h.data) == 0 {
		file.ExtentInfo.Extents = nil
	} else {
		extent, err := h.fs.tape.AppendFileData(h.data)
		if err != nil {
			return err
		}
		file.ExtentInfo.Extents = []ltfs.Extent{extent}
	}
	ltfs.TouchFile(file, now)
	ltfs.TouchDirectory(parent, now)
	return nil
}

func (h *FileHandle) Flush(_ context.Context, _ *fuse.FlushRequest) error {
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
	if err := h.fs.FlushIndex(); err != nil {
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

var (
	_ bazilfs.FS                 = (*FS)(nil)
	_ bazilfs.Node               = (*Dir)(nil)
	_ bazilfs.Node               = (*File)(nil)
	_ bazilfs.Node               = (*FlushFile)(nil)
	_ bazilfs.NodeStringLookuper = (*Dir)(nil)
	_ bazilfs.HandleReadDirAller = (*Dir)(nil)
	_ bazilfs.NodeCreater        = (*Dir)(nil)
	_ bazilfs.NodeMkdirer        = (*Dir)(nil)
	_ bazilfs.NodeRemover        = (*Dir)(nil)
	_ bazilfs.NodeRenamer        = (*Dir)(nil)
	_ bazilfs.NodeOpener         = (*File)(nil)
	_ bazilfs.NodeFsyncer        = (*File)(nil)
	_ bazilfs.HandleReader       = (*FileHandle)(nil)
	_ bazilfs.HandleWriter       = (*FileHandle)(nil)
	_ bazilfs.HandleReleaser     = (*FileHandle)(nil)
	_ bazilfs.HandleFlusher      = (*FileHandle)(nil)
	_ bazilfs.NodeOpener         = (*FlushFile)(nil)
	_ bazilfs.HandleReader       = (*FlushHandle)(nil)
	_ bazilfs.HandleWriter       = (*FlushHandle)(nil)
	_ bazilfs.HandleReleaser     = (*FlushHandle)(nil)
	_ bazilfs.HandleFlusher      = (*FlushHandle)(nil)
)
