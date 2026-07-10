package daemon

import (
	"fmt"
	"os"
	"path"
	"path/filepath"
	"sort"
	"strings"

	"bazil.org/fuse"
	bazilfs "bazil.org/fuse/fs"

	tapefs "github.com/cyanmint/tapefuse/fs"
	"github.com/cyanmint/tapefuse/internal/ltfs"
)

// cmdAssign records a device -> letter mapping.  The tape is not opened until
// indexread is run.
func (d *Daemon) cmdAssign(device, letter string) Response {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.entries[letter]; ok {
		return Response{OK: false, Error: fmt.Sprintf("letter %q already assigned", letter)}
	}
	d.entries[letter] = &entry{device: device}
	d.logf("assigned letter %s to device %s", letter, device)
	return Response{OK: true}
}

// cmdIndexRead opens the assigned tape (creating a fresh file-backed image if
// none exists yet) and reads its LTFS index either into memory (mode
// "memory") or into the on-disk cache file (mode "file").
func (d *Daemon) cmdIndexRead(letter, mode string) Response {
	d.mu.Lock()
	defer d.mu.Unlock()
	e, ok := d.entries[letter]
	if !ok {
		return Response{OK: false, Error: fmt.Sprintf("letter %q not assigned", letter)}
	}
	if e.mountPoint != "" {
		return Response{OK: false, Error: "unmount before indexread"}
	}

	if e.tapeFS == nil {
		tfs, err := tapefs.OpenTape(e.device)
		if err != nil {
			return Response{OK: false, Error: "open tape: " + err.Error()}
		}
		e.tapeFS = tfs
		e.idxPart = tfs.IndexPartition
		e.dataPart = tfs.DataPartition
		e.idxLabel = tfs.IndexLabel
		e.dataLabel = tfs.DataLabel
	}

	idx, err := e.tapeFS.ReadIndexFromTape()
	if err != nil {
		// A blank or unreadable tape has no committed index yet; format it
		// (writing labels + an empty index) and try once more.  For a fresh
		// file-backed path OpenTape already formatted it, so this handles
		// real/blank tapes.
		d.logf("indexread: no readable index on %s (%v); formatting", e.device, err)
		if ferr := e.tapeFS.Close(); ferr != nil {
			d.logf("indexread: close before format: %v", ferr)
		}
		e.tapeFS = nil
		tfs, ferr := tapefs.FormatTape(e.device)
		if ferr != nil {
			return Response{OK: false, Error: "format tape: " + ferr.Error()}
		}
		e.tapeFS = tfs
		e.idxPart = tfs.IndexPartition
		e.dataPart = tfs.DataPartition
		e.idxLabel = tfs.IndexLabel
		e.dataLabel = tfs.DataLabel
		idx, err = e.tapeFS.ReadIndexFromTape()
		if err != nil {
			return Response{OK: false, Error: "read index after format: " + err.Error()}
		}
	}

	switch mode {
	case "memory":
		e.memMode = true
		e.memIndex = idx
		// Drop any stale on-disk cache so the two representations cannot
		// diverge.
		_ = os.Remove(IndexPath(letter))
		d.logf("read index for letter %s into memory", letter)
	case "file":
		e.memMode = false
		e.memIndex = nil
		if err := os.MkdirAll(IndexDir, 0o755); err != nil {
			return Response{OK: false, Error: err.Error()}
		}
		if err := ltfs.SaveIndexToFile(IndexPath(letter), idx); err != nil {
			return Response{OK: false, Error: "save index to disk: " + err.Error()}
		}
		d.logf("read index for letter %s into cache %s", letter, IndexPath(letter))
	default:
		return Response{OK: false, Error: fmt.Sprintf("unknown indexread mode %q (want \"memory\" or \"file\")", mode)}
	}
	return Response{OK: true}
}

// cmdLs lists the directory (or single file) at tapePath in the working index.
func (d *Daemon) cmdLs(letter, tapePath string) Response {
	d.mu.Lock()
	defer d.mu.Unlock()
	e, ok := d.entries[letter]
	if !ok {
		return Response{OK: false, Error: fmt.Sprintf("letter %q not assigned", letter)}
	}
	idx, err := d.loadIndex(letter, e)
	if err != nil {
		return Response{OK: false, Error: err.Error()}
	}

	if dir := idx.FindDirectory(tapePath); dir != nil {
		var b strings.Builder
		names := make([]string, 0, len(dir.Contents.Directories)+len(dir.Contents.Files))
		lines := make(map[string]string)
		for _, sub := range dir.Contents.Directories {
			names = append(names, sub.Name)
			lines[sub.Name] = fmt.Sprintf("%-40s <dir>", sub.Name+"/")
		}
		for _, f := range dir.Contents.Files {
			names = append(names, f.Name)
			lines[f.Name] = fmt.Sprintf("%-40s %d", f.Name, f.Length)
		}
		sort.Strings(names)
		for _, n := range names {
			b.WriteString(lines[n])
			b.WriteByte('\n')
		}
		return Response{OK: true, Output: b.String()}
	}

	if file, _, err := idx.FindFile(tapePath); err == nil {
		return Response{OK: true, Output: fmt.Sprintf("%-40s %d\n", path.Base(ltfs.CleanPath(tapePath)), file.Length)}
	}
	return Response{OK: false, Error: fmt.Sprintf("no such file or directory: %s", ltfs.CleanPath(tapePath))}
}

// cmdRm removes a file or (when recursive) a directory tree at tapePath,
// marking freed tape ranges as available space in the index.
func (d *Daemon) cmdRm(letter, tapePath string, recursive bool) Response {
	d.mu.Lock()
	defer d.mu.Unlock()
	e, ok := d.entries[letter]
	if !ok {
		return Response{OK: false, Error: fmt.Sprintf("letter %q not assigned", letter)}
	}
	if e.mountPoint != "" {
		return Response{OK: false, Error: "unmount before rm"}
	}
	idx, err := d.loadIndex(letter, e)
	if err != nil {
		return Response{OK: false, Error: err.Error()}
	}
	cleaned := ltfs.CleanPath(tapePath)
	if cleaned == "/" {
		return Response{OK: false, Error: "refusing to remove the root directory"}
	}
	parent, name, err := idx.ParentDirectory(cleaned)
	if err != nil {
		return Response{OK: false, Error: err.Error()}
	}

	physCap := d.physCapFn(e)

	if file := parent.FindFile(name); file != nil {
		idx.FreeFileExtents(file, physCap)
		removeFile(parent, name)
		ltfs.TouchDirectory(parent, ltfs.Now())
		if err := d.saveIndex(letter, e, idx); err != nil {
			return Response{OK: false, Error: err.Error()}
		}
		d.logf("removed file %s:%s", letter, cleaned)
		return Response{OK: true}
	}

	if sub := parent.FindDirectory(name); sub != nil {
		if !recursive && (len(sub.Contents.Files) > 0 || len(sub.Contents.Directories) > 0) {
			return Response{OK: false, Error: fmt.Sprintf("directory not empty: %s (use -r)", cleaned)}
		}
		for _, f := range ltfs.AllFiles(sub) {
			idx.FreeFileExtents(f, physCap)
		}
		removeDirectory(parent, name)
		ltfs.TouchDirectory(parent, ltfs.Now())
		if err := d.saveIndex(letter, e, idx); err != nil {
			return Response{OK: false, Error: err.Error()}
		}
		d.logf("removed directory %s:%s (recursive=%v)", letter, cleaned, recursive)
		return Response{OK: true}
	}

	return Response{OK: false, Error: fmt.Sprintf("no such file or directory: %s", cleaned)}
}

// cmdGet reads a file or directory tree directly from tape and writes it to
// the local filesystem at dest.  dest is an absolute path supplied by the
// client.  When dest is an existing directory the source base name is placed
// underneath it.
func (d *Daemon) cmdGet(letter, tapePath, dest string) Response {
	d.mu.Lock()
	defer d.mu.Unlock()
	e, ok := d.entries[letter]
	if !ok {
		return Response{OK: false, Error: fmt.Sprintf("letter %q not assigned", letter)}
	}
	if e.tapeFS == nil {
		return Response{OK: false, Error: "tape not loaded; run indexread first"}
	}
	idx, err := d.loadIndex(letter, e)
	if err != nil {
		return Response{OK: false, Error: err.Error()}
	}
	cleaned := ltfs.CleanPath(tapePath)
	base := path.Base(cleaned)

	// Resolve destination: if dest is an existing directory, append base name.
	target := dest
	if info, err := os.Stat(dest); err == nil && info.IsDir() && cleaned != "/" {
		target = filepath.Join(dest, base)
	}

	if file, _, err := idx.FindFile(cleaned); err == nil {
		if err := d.getFile(e, file, target); err != nil {
			return Response{OK: false, Error: err.Error()}
		}
		d.logf("got file %s:%s -> %s", letter, cleaned, target)
		return Response{OK: true, Output: fmt.Sprintf("wrote %s\n", target)}
	}

	if dir := idx.FindDirectory(cleaned); dir != nil {
		if err := d.getDir(e, dir, target); err != nil {
			return Response{OK: false, Error: err.Error()}
		}
		d.logf("got directory %s:%s -> %s", letter, cleaned, target)
		return Response{OK: true, Output: fmt.Sprintf("wrote %s/\n", target)}
	}

	return Response{OK: false, Error: fmt.Sprintf("no such file or directory: %s", cleaned)}
}

func (d *Daemon) getFile(e *entry, file *ltfs.File, target string) error {
	if err := os.MkdirAll(filepath.Dir(target), 0o755); err != nil {
		return err
	}
	data, err := e.tapeFS.ReadFileData(file)
	if err != nil {
		return fmt.Errorf("read %q from tape: %w", file.Name, err)
	}
	if err := os.WriteFile(target, data, 0o644); err != nil {
		return err
	}
	return nil
}

func (d *Daemon) getDir(e *entry, dir *ltfs.Directory, target string) error {
	if err := os.MkdirAll(target, 0o755); err != nil {
		return err
	}
	for i := range dir.Contents.Files {
		f := &dir.Contents.Files[i]
		if err := d.getFile(e, f, filepath.Join(target, f.Name)); err != nil {
			return err
		}
	}
	for i := range dir.Contents.Directories {
		sub := &dir.Contents.Directories[i]
		if err := d.getDir(e, sub, filepath.Join(target, sub.Name)); err != nil {
			return err
		}
	}
	return nil
}

// cmdPush writes a local file or directory tree to tape, updating the index
// and always overwriting any existing target.  src is an absolute path
// supplied by the client.
func (d *Daemon) cmdPush(letter, tapePath, src string) Response {
	d.mu.Lock()
	defer d.mu.Unlock()
	e, ok := d.entries[letter]
	if !ok {
		return Response{OK: false, Error: fmt.Sprintf("letter %q not assigned", letter)}
	}
	if e.tapeFS == nil {
		return Response{OK: false, Error: "tape not loaded; run indexread first"}
	}
	if e.mountPoint != "" {
		return Response{OK: false, Error: "unmount before push"}
	}
	idx, err := d.loadIndex(letter, e)
	if err != nil {
		return Response{OK: false, Error: err.Error()}
	}

	info, err := os.Stat(src)
	if err != nil {
		return Response{OK: false, Error: err.Error()}
	}

	cleaned := ltfs.CleanPath(tapePath)
	if info.IsDir() {
		if err := d.pushDir(e, idx, src, cleaned); err != nil {
			return Response{OK: false, Error: err.Error()}
		}
	} else {
		if err := d.pushFile(e, idx, src, cleaned); err != nil {
			return Response{OK: false, Error: err.Error()}
		}
	}

	if err := d.saveIndex(letter, e, idx); err != nil {
		return Response{OK: false, Error: err.Error()}
	}
	d.logf("pushed %s -> %s:%s", src, letter, cleaned)
	return Response{OK: true, Output: fmt.Sprintf("pushed %s -> %s:%s\n", src, letter, cleaned)}
}

func (d *Daemon) pushFile(e *entry, idx *ltfs.Index, src, tapePath string) error {
	data, err := os.ReadFile(src)
	if err != nil {
		return err
	}
	parentPath, base := path.Split(tapePath)
	parent, err := idx.MkdirAll(parentPath)
	if err != nil {
		return err
	}
	if parent.FindDirectory(base) != nil {
		return fmt.Errorf("%s is a directory", tapePath)
	}

	now := ltfs.Now()
	file := parent.FindFile(base)
	if file == nil {
		nf := ltfs.NewFile(base, idx.NextFileUID())
		parent.Contents.Files = append(parent.Contents.Files, nf)
		file = &parent.Contents.Files[len(parent.Contents.Files)-1]
	} else {
		// Overwrite: free the existing extents before writing the new data so
		// the freed slots can be reused (best-fit).
		idx.FreeFileExtents(file, d.physCapFn(e))
		file.ExtentInfo.Extents = nil
	}

	if len(data) > 0 {
		extent, err := e.tapeFS.WriteFileData(data, idx)
		if err != nil {
			return fmt.Errorf("write %q to tape: %w", tapePath, err)
		}
		extent.FileOffset = 0
		file.ExtentInfo.Extents = []ltfs.Extent{extent}
	}
	file.Length = int64(len(data))
	ltfs.TouchFile(file, now)
	ltfs.TouchDirectory(parent, now)
	return nil
}

func (d *Daemon) pushDir(e *entry, idx *ltfs.Index, src, tapePath string) error {
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	if _, err := idx.MkdirAll(tapePath); err != nil {
		return err
	}
	for _, de := range entries {
		childSrc := filepath.Join(src, de.Name())
		childTape := path.Join(tapePath, de.Name())
		if de.IsDir() {
			if err := d.pushDir(e, idx, childSrc, childTape); err != nil {
				return err
			}
		} else {
			if err := d.pushFile(e, idx, childSrc, childTape); err != nil {
				return err
			}
		}
	}
	return nil
}

func (d *Daemon) cmdMount(letter, mountPoint string, bufCfg tapefs.BufferConfig) Response {
	d.mu.Lock()
	defer d.mu.Unlock()
	e, ok := d.entries[letter]
	if !ok {
		return Response{OK: false, Error: fmt.Sprintf("letter %q not assigned", letter)}
	}
	if e.mountPoint != "" {
		return Response{OK: false, Error: fmt.Sprintf("letter %q already mounted at %s", letter, e.mountPoint)}
	}
	if e.tapeFS == nil {
		return Response{OK: false, Error: "tape not loaded; run indexread first"}
	}

	// The FUSE filesystem is backed by an on-disk index cache file.  When the
	// working index is in memory (indexread -m) persist it to the cache path
	// for the duration of the mount; it is reloaded into memory on umount.
	if e.memMode {
		if e.memIndex == nil {
			return Response{OK: false, Error: "index not loaded; run indexread first"}
		}
		if err := os.MkdirAll(IndexDir, 0o755); err != nil {
			return Response{OK: false, Error: err.Error()}
		}
		if err := ltfs.SaveIndexToFile(IndexPath(letter), e.memIndex); err != nil {
			return Response{OK: false, Error: "persist in-memory index: " + err.Error()}
		}
	} else if _, err := os.Stat(IndexPath(letter)); err != nil {
		return Response{OK: false, Error: "index not on disk; run indexread first"}
	}

	if err := os.MkdirAll(mountPoint, 0o755); err != nil {
		return Response{OK: false, Error: err.Error()}
	}
	conn, err := fuse.Mount(mountPoint, fuse.FSName("ltape-"+letter), fuse.Subtype("ltfs"))
	if err != nil {
		return Response{OK: false, Error: "fuse mount: " + err.Error()}
	}
	fuseFS := tapefs.New(e.tapeFS, IndexPath(letter), bufCfg)
	done := make(chan struct{})
	e.fuseConn = conn
	e.fuseFS = fuseFS
	e.mountPoint = mountPoint
	e.fuseDone = done
	e.bufCfg = bufCfg
	go func() {
		defer close(done)
		_ = bazilfs.Serve(conn, fuseFS)
		conn.Close()
	}()
	d.logf("mounted letter %s at %s", letter, mountPoint)
	return Response{OK: true}
}

func (d *Daemon) cmdUmount(letter string) Response {
	d.mu.Lock()
	e, ok := d.entries[letter]
	if !ok {
		d.mu.Unlock()
		return Response{OK: false, Error: fmt.Sprintf("letter %q not assigned", letter)}
	}
	if e.mountPoint == "" {
		d.mu.Unlock()
		return Response{OK: false, Error: fmt.Sprintf("letter %q is not mounted", letter)}
	}
	if err := fuse.Unmount(e.mountPoint); err != nil {
		d.mu.Unlock()
		return Response{OK: false, Error: "unmount: " + err.Error()}
	}
	done := e.fuseDone
	e.mountPoint = ""
	e.fuseConn = nil
	e.fuseFS = nil
	e.fuseDone = nil
	memMode := e.memMode
	d.mu.Unlock()
	// Wait for the FUSE serve goroutine to finish processing any in-flight
	// requests (e.g. pending Flush/Release) before returning to the caller.
	if done != nil {
		<-done
	}
	// Reload the working index back into memory if this was a memory-mode
	// mount, so subsequent commands see the mount's changes.
	if memMode {
		d.mu.Lock()
		if idx, err := ltfs.LoadIndexFromFile(IndexPath(letter)); err == nil {
			e.memIndex = idx
			_ = os.Remove(IndexPath(letter))
		}
		d.mu.Unlock()
	}
	d.logf("unmounted letter %s", letter)
	return Response{OK: true}
}

// cmdFlush flushes all pending write buffers (the mount's temp dir) for letter
// to tape immediately, pushing each staged file into the tape data partition.
func (d *Daemon) cmdFlush(letter string) Response {
	d.mu.Lock()
	e, ok := d.entries[letter]
	if !ok {
		d.mu.Unlock()
		return Response{OK: false, Error: fmt.Sprintf("letter %q not assigned", letter)}
	}
	fuseFS := e.fuseFS
	d.mu.Unlock()

	if fuseFS == nil {
		return Response{OK: false, Error: fmt.Sprintf("letter %q is not mounted", letter)}
	}
	if err := fuseFS.FlushAllHandles(); err != nil {
		return Response{OK: false, Error: "flush: " + err.Error()}
	}
	d.logf("flushed temp dir for letter %s", letter)
	return Response{OK: true}
}

// cmdCommitIndex writes the working index (from memory or the disk cache) back
// to the tape's index partition.
func (d *Daemon) cmdCommitIndex(letter string) Response {
	d.mu.Lock()
	defer d.mu.Unlock()
	e, ok := d.entries[letter]
	if !ok {
		return Response{OK: false, Error: fmt.Sprintf("letter %q not assigned", letter)}
	}
	if e.tapeFS == nil {
		return Response{OK: false, Error: "tape not loaded; run indexread first"}
	}
	idx, err := d.loadIndex(letter, e)
	if err != nil {
		return Response{OK: false, Error: err.Error()}
	}
	if err := e.tapeFS.FlushIndex(idx); err != nil {
		return Response{OK: false, Error: "write index to tape: " + err.Error()}
	}
	if err := d.saveIndex(letter, e, idx); err != nil {
		return Response{OK: false, Error: "save updated index: " + err.Error()}
	}
	d.logf("committed index to tape for letter %s", letter)
	return Response{OK: true}
}

// cmdDiscardIndex discards the working index without writing it to tape and
// closes the tape handles.
func (d *Daemon) cmdDiscardIndex(letter string) Response {
	d.mu.Lock()
	defer d.mu.Unlock()
	e, ok := d.entries[letter]
	if !ok {
		return Response{OK: false, Error: fmt.Sprintf("letter %q not assigned", letter)}
	}
	if e.mountPoint != "" {
		return Response{OK: false, Error: "unmount before discardindex"}
	}
	d.closeEntry(e)
	_ = os.Remove(IndexPath(letter))
	d.logf("discarded working index for letter %s", letter)
	return Response{OK: true}
}

// cmdDefrag is currently a stub: it logs the request and does nothing.
func (d *Daemon) cmdDefrag(letter string) Response {
	d.mu.Lock()
	_, ok := d.entries[letter]
	d.mu.Unlock()
	if !ok {
		return Response{OK: false, Error: fmt.Sprintf("letter %q not assigned", letter)}
	}
	d.logf("defrag requested for letter %s (stub: no-op)", letter)
	return Response{OK: true, Output: "defrag is a no-op stub\n"}
}

// parseBufferConfig converts a temp-dir kind string ("memory" or "file") into
// a BufferConfig with no size limit.
func parseBufferConfig(kindStr string) (tapefs.BufferConfig, error) {
	switch kindStr {
	case "memory":
		return tapefs.BufferConfig{Kind: tapefs.BufferKindMemory}, nil
	case "file":
		return tapefs.BufferConfig{Kind: tapefs.BufferKindFile}, nil
	default:
		return tapefs.BufferConfig{}, fmt.Errorf("unknown temp dir kind %q (want \"memory\" or \"file\")", kindStr)
	}
}

func removeFile(dir *ltfs.Directory, name string) {
	for i := range dir.Contents.Files {
		if dir.Contents.Files[i].Name == name {
			dir.Contents.Files = append(dir.Contents.Files[:i], dir.Contents.Files[i+1:]...)
			return
		}
	}
}

func removeDirectory(dir *ltfs.Directory, name string) {
	for i := range dir.Contents.Directories {
		if dir.Contents.Directories[i].Name == name {
			dir.Contents.Directories = append(dir.Contents.Directories[:i], dir.Contents.Directories[i+1:]...)
			return
		}
	}
}
