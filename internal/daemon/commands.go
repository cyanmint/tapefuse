package daemon

import (
	"fmt"
	"os"
	"sort"

	"bazil.org/fuse"
	bazilfs "bazil.org/fuse/fs"

	tapefs "github.com/cyanmint/tapefuse/fs"
	"github.com/cyanmint/tapefuse/internal/ltfs"
)

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

func (d *Daemon) cmdUnassign(letter string) Response {
	d.mu.Lock()
	defer d.mu.Unlock()
	e, ok := d.entries[letter]
	if !ok {
		return Response{OK: false, Error: fmt.Sprintf("letter %q not assigned", letter)}
	}
	if e.mountPoint != "" {
		return Response{OK: false, Error: fmt.Sprintf("letter %q is mounted at %s; umount first", letter, e.mountPoint)}
	}
	d.closeEntry(e)
	delete(d.entries, letter)
	d.logf("unassigned letter %s", letter)
	return Response{OK: true}
}

func (d *Daemon) cmdInit(letter string) Response {
	d.mu.Lock()
	defer d.mu.Unlock()
	e, ok := d.entries[letter]
	if !ok {
		return Response{OK: false, Error: fmt.Sprintf("letter %q not assigned", letter)}
	}
	if e.mountPoint != "" {
		return Response{OK: false, Error: "unmount before init"}
	}
	d.closeEntry(e)

	t, err := tapefs.FormatTape(e.device)
	if err != nil {
		return Response{OK: false, Error: err.Error()}
	}
	e.idxPart = t.IndexPartition
	e.dataPart = t.DataPartition
	e.idxLabel = t.IndexLabel
	e.dataLabel = t.DataLabel
	e.tapeFS = t

	idx, err := t.ReadIndexFromTape()
	if err != nil {
		return Response{OK: false, Error: err.Error()}
	}
	if err := os.MkdirAll(IndexDir, 0o755); err != nil {
		return Response{OK: false, Error: err.Error()}
	}
	if err := ltfs.SaveIndexToFile(IndexPath(letter), idx); err != nil {
		return Response{OK: false, Error: err.Error()}
	}
	d.logf("initialized tape %s for letter %s and cached index at %s", e.device, letter, IndexPath(letter))
	return Response{OK: true}
}

func (d *Daemon) cmdLoad(letter string) Response {
	d.mu.Lock()
	defer d.mu.Unlock()
	e, ok := d.entries[letter]
	if !ok {
		return Response{OK: false, Error: fmt.Sprintf("letter %q not assigned", letter)}
	}
	if e.idxPart == nil {
		idxP, dataP, idxL, dataL, err := tapefs.OpenTapePartitions(e.device)
		if err != nil {
			return Response{OK: false, Error: err.Error()}
		}
		e.idxPart = idxP
		e.dataPart = dataP
		e.idxLabel = idxL
		e.dataLabel = dataL
	}

	tfs := tapefs.NewTapeFSFromParts(e.device, e.idxPart, e.dataPart, e.idxLabel, e.dataLabel)
	idx, err := tfs.ReadIndexFromTape()
	if err != nil {
		return Response{OK: false, Error: "read index from tape: " + err.Error()}
	}
	if err := os.MkdirAll(IndexDir, 0o755); err != nil {
		return Response{OK: false, Error: err.Error()}
	}
	if err := ltfs.SaveIndexToFile(IndexPath(letter), idx); err != nil {
		return Response{OK: false, Error: "save index to disk: " + err.Error()}
	}
	e.tapeFS = tfs
	d.logf("loaded tape %s for letter %s into cache %s", e.device, letter, IndexPath(letter))
	return Response{OK: true}
}

func (d *Daemon) cmdCommit(letter string) Response {
	d.mu.Lock()
	defer d.mu.Unlock()
	e, ok := d.entries[letter]
	if !ok {
		return Response{OK: false, Error: fmt.Sprintf("letter %q not assigned", letter)}
	}
	if e.tapeFS == nil {
		return Response{OK: false, Error: "tape not loaded; run load first"}
	}
	idx, err := ltfs.LoadIndexFromFile(IndexPath(letter))
	if err != nil {
		return Response{OK: false, Error: "load from disk: " + err.Error()}
	}
	if err := e.tapeFS.FlushIndex(idx); err != nil {
		return Response{OK: false, Error: "flush to tape: " + err.Error()}
	}
	if err := ltfs.SaveIndexToFile(IndexPath(letter), idx); err != nil {
		return Response{OK: false, Error: "save updated index to disk: " + err.Error()}
	}
	d.logf("committed cached index %s back to tape for letter %s", IndexPath(letter), letter)
	return Response{OK: true}
}

func (d *Daemon) cmdDiscard(letter string) Response {
	d.mu.Lock()
	defer d.mu.Unlock()
	e, ok := d.entries[letter]
	if !ok {
		return Response{OK: false, Error: fmt.Sprintf("letter %q not assigned", letter)}
	}
	if e.mountPoint != "" {
		return Response{OK: false, Error: "unmount before discard"}
	}
	d.closeEntry(e)
	_ = os.Remove(IndexPath(letter))
	d.logf("discarded cached state for letter %s", letter)
	return Response{OK: true}
}

func (d *Daemon) cmdMount(letter, mountPoint string) Response {
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
		return Response{OK: false, Error: "tape not loaded; run load first"}
	}
	if _, err := os.Stat(IndexPath(letter)); err != nil {
		return Response{OK: false, Error: "index not on disk; run load first"}
	}
	if err := os.MkdirAll(mountPoint, 0o755); err != nil {
		return Response{OK: false, Error: err.Error()}
	}
	conn, err := fuse.Mount(mountPoint, fuse.FSName("ltape-"+letter), fuse.Subtype("ltfs"))
	if err != nil {
		return Response{OK: false, Error: "fuse mount: " + err.Error()}
	}
	fuseFS := tapefs.New(e.tapeFS, IndexPath(letter))
	done := make(chan struct{})
	e.fuseConn = conn
	e.fuseFS = fuseFS
	e.mountPoint = mountPoint
	e.fuseDone = done
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
	defer d.mu.Unlock()
	e, ok := d.entries[letter]
	if !ok {
		return Response{OK: false, Error: fmt.Sprintf("letter %q not assigned", letter)}
	}
	if e.mountPoint == "" {
		return Response{OK: false, Error: fmt.Sprintf("letter %q is not mounted", letter)}
	}
	if err := fuse.Unmount(e.mountPoint); err != nil {
		return Response{OK: false, Error: "unmount: " + err.Error()}
	}
	e.mountPoint = ""
	e.fuseConn = nil
	e.fuseFS = nil
	e.fuseDone = nil
	d.logf("unmounted letter %s", letter)
	return Response{OK: true}
}

func (d *Daemon) cmdEject(letter string) Response {
	d.mu.Lock()
	e, ok := d.entries[letter]
	if !ok {
		d.mu.Unlock()
		return Response{OK: false, Error: fmt.Sprintf("letter %q not assigned", letter)}
	}
	if e.mountPoint != "" {
		if err := fuse.Unmount(e.mountPoint); err != nil {
			d.mu.Unlock()
			return Response{OK: false, Error: "unmount before eject: " + err.Error()}
		}
		done := e.fuseDone
		e.mountPoint = ""
		e.fuseConn = nil
		e.fuseFS = nil
		e.fuseDone = nil
		d.mu.Unlock()
		if done != nil {
			<-done
		}
		d.mu.Lock()
	}
	if e.idxPart != nil {
		_ = e.idxPart.Eject()
	}
	d.closeEntry(e)
	delete(d.entries, letter)
	d.mu.Unlock()
	d.logf("ejected and removed letter %s", letter)
	return Response{OK: true}
}

func (d *Daemon) cmdList() Response {
	d.mu.Lock()
	defer d.mu.Unlock()
	entries := make([]EntryStatus, 0, len(d.entries))
	for letter, e := range d.entries {
		entries = append(entries, EntryStatus{
			Letter:     letter,
			Device:     e.device,
			Loaded:     e.tapeFS != nil,
			MountPoint: e.mountPoint,
		})
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Letter < entries[j].Letter })
	return Response{OK: true, Entries: entries}
}
