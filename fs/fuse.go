package fs

import (
	"sync"

	bazilfs "bazil.org/fuse/fs"

	"github.com/cyanmint/tapefuse/internal/ltfs"
	"github.com/cyanmint/tapefuse/internal/tape"
)

const flushFileName = ".tapefuse_flush"

type TapeFS struct {
	Device         string
	IndexPartition tape.Tape
	DataPartition  tape.Tape
	IndexLabel     *ltfs.Label
	DataLabel      *ltfs.Label
}

type FS struct {
	tape      *TapeFS
	indexPath string
	mu        sync.RWMutex
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
	fs   *FS
	path string
}

type FlushFile struct {
	fs *FS
}

type FlushHandle struct {
	fs *FS
}

func New(tapeFS *TapeFS, indexPath string) *FS {
	return &FS{tape: tapeFS, indexPath: indexPath}
}

func (f *FS) Root() (bazilfs.Node, error) {
	return &Dir{fs: f, path: "/"}, nil
}

func (f *FS) loadIndex() (*ltfs.Index, error) {
	return ltfs.LoadIndexFromFile(f.indexPath)
}

func (f *FS) saveIndex(idx *ltfs.Index) error {
	return ltfs.SaveIndexToFile(f.indexPath, idx)
}

func (f *FS) Close() error {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.tape.Close()
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
