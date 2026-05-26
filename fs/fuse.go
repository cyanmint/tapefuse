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
	bufCfg    BufferConfig
	mu        sync.RWMutex
	// handlesMu protects the handles map.  Lock order: handlesMu is
	// independent of mu; never hold both at the same time.
	handlesMu sync.Mutex
	handles   map[*FileHandle]struct{} // only handles with pending writes
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
	// mu protects wbuf and dirty.  Lock order: mu before fs.mu.
	mu    sync.Mutex
	wbuf  writeBuf // nil until first write; closed after a successful flush
	dirty bool     // true when wbuf holds data not yet written to tape
}

type FlushFile struct {
	fs *FS
}

type FlushHandle struct {
	fs *FS
}

// New creates an FS backed by tapeFS.  cfg controls per-file-handle write
// buffering; pass DefaultBufferConfig() for the default (file-backed,
// unlimited).
func New(tapeFS *TapeFS, indexPath string, cfg BufferConfig) *FS {
	return &FS{
		tape:      tapeFS,
		indexPath: indexPath,
		bufCfg:    cfg,
		handles:   make(map[*FileHandle]struct{}),
	}
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
	// Defensively clean up any handles that were not released before Close.
	f.handlesMu.Lock()
	for h := range f.handles {
		h.mu.Lock()
		if h.wbuf != nil {
			_ = h.wbuf.Close()
			h.wbuf = nil
		}
		h.dirty = false
		h.mu.Unlock()
	}
	f.handles = make(map[*FileHandle]struct{})
	f.handlesMu.Unlock()
	return f.tape.Close()
}

// addHandle registers h in the dirty-handle set.  Called when the first write
// initialises a wbuf for h.
func (f *FS) addHandle(h *FileHandle) {
	f.handlesMu.Lock()
	f.handles[h] = struct{}{}
	f.handlesMu.Unlock()
}

// removeHandle removes h from the dirty-handle set after a successful flush.
func (f *FS) removeHandle(h *FileHandle) {
	f.handlesMu.Lock()
	delete(f.handles, h)
	f.handlesMu.Unlock()
}

// FlushAllHandles flushes every currently-dirty FileHandle to tape.  It is
// used by the "flushfiles" daemon command.  Each handle is flushed
// independently; all handles are attempted even if one fails, and the first
// error is returned.
func (f *FS) FlushAllHandles() error {
	f.handlesMu.Lock()
	snapshot := make([]*FileHandle, 0, len(f.handles))
	for h := range f.handles {
		snapshot = append(snapshot, h)
	}
	f.handlesMu.Unlock()

	var firstErr error
	for _, h := range snapshot {
		if err := h.flushBuffer(); err != nil && firstErr == nil {
			firstErr = err
		}
	}
	return firstErr
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
