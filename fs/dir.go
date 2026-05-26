package fs

import (
	"context"
	"os"
	"path"
	"strings"
	"syscall"

	"bazil.org/fuse"
	bazilfs "bazil.org/fuse/fs"

	"github.com/cyanmint/tapefuse/internal/ltfs"
)

func joinPath(parent, name string) string {
	if parent == "/" {
		return path.Join("/", name)
	}
	return path.Join(parent, name)
}

func (d *Dir) Attr(_ context.Context, a *fuse.Attr) error {
	d.fs.mu.RLock()
	defer d.fs.mu.RUnlock()

	idx, err := d.fs.loadIndex()
	if err != nil {
		return fuse.EIO
	}
	dir := idx.FindDirectory(d.path)
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

	idx, err := d.fs.loadIndex()
	if err != nil {
		return nil, fuse.EIO
	}
	dir := idx.FindDirectory(d.path)
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

	idx, err := d.fs.loadIndex()
	if err != nil {
		return nil, fuse.EIO
	}
	dir := idx.FindDirectory(d.path)
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

	idx, err := d.fs.loadIndex()
	if err != nil {
		return nil, nil, fuse.EIO
	}
	dir := idx.FindDirectory(d.path)
	if dir == nil {
		return nil, nil, fuse.ENOENT
	}
	if dir.FindDirectory(req.Name) != nil || dir.FindFile(req.Name) != nil {
		return nil, nil, fuse.EEXIST
	}

	file := ltfs.NewFile(req.Name, idx.NextFileUID())
	dir.Contents.Files = append(dir.Contents.Files, file)
	ltfs.TouchDirectory(dir, ltfs.Now())
	if err := d.fs.saveIndex(idx); err != nil {
		return nil, nil, err
	}

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

	idx, err := d.fs.loadIndex()
	if err != nil {
		return nil, fuse.EIO
	}
	dir := idx.FindDirectory(d.path)
	if dir == nil {
		return nil, fuse.ENOENT
	}
	if dir.FindDirectory(req.Name) != nil || dir.FindFile(req.Name) != nil {
		return nil, fuse.EEXIST
	}

	child := ltfs.NewDirectory(req.Name)
	dir.Contents.Directories = append(dir.Contents.Directories, child)
	ltfs.TouchDirectory(dir, ltfs.Now())
	if err := d.fs.saveIndex(idx); err != nil {
		return nil, err
	}
	return &Dir{fs: d.fs, path: joinPath(d.path, req.Name)}, nil
}

func (d *Dir) Remove(_ context.Context, req *fuse.RemoveRequest) error {
	if d.path == "/" && req.Name == flushFileName {
		return fuse.Errno(syscall.EPERM)
	}

	d.fs.mu.Lock()
	defer d.fs.mu.Unlock()

	idx, err := d.fs.loadIndex()
	if err != nil {
		return fuse.EIO
	}
	dir := idx.FindDirectory(d.path)
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
			return d.fs.saveIndex(idx)
		}
		return fuse.ENOENT
	}

	for i, child := range dir.Contents.Files {
		if child.Name != req.Name {
			continue
		}
		// Record the deleted file's extents as available spaces so they can
		// be reused by future writes (sparse appending strategy).
		for _, ext := range child.ExtentInfo.Extents {
			if ext.ByteCount <= 0 {
				continue
			}
			// Prefer the physical block capacity so that re-reused slots
			// accurately report their true capacity.
			physCap := d.fs.tape.DataBlockCapacity(uint64(ext.StartBlock))
			if physCap <= 0 {
				physCap = ext.ByteCount
			}
			idx.AvailableSpaces = append(idx.AvailableSpaces, ltfs.AvailableSpace{
				Partition:  ext.Partition,
				StartBlock: ext.StartBlock,
				ByteCount:  physCap,
			})
		}
		dir.Contents.Files = append(dir.Contents.Files[:i], dir.Contents.Files[i+1:]...)
		ltfs.TouchDirectory(dir, now)
		return d.fs.saveIndex(idx)
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

	idx, err := d.fs.loadIndex()
	if err != nil {
		return fuse.EIO
	}
	oldParent := idx.FindDirectory(d.path)
	newParent := idx.FindDirectory(targetDir.path)
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
		return d.fs.saveIndex(idx)
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
		return d.fs.saveIndex(idx)
	}

	return fuse.ENOENT
}
