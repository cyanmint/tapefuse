package daemon

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"
	"sync"

	"bazil.org/fuse"
	bazilfs "bazil.org/fuse/fs"

	tapefs "github.com/cyanmint/tapefuse/fs"
	"github.com/cyanmint/tapefuse/internal/ltfs"
	"github.com/cyanmint/tapefuse/internal/tape"
)

type entry struct {
	device     string
	idxPart    *tape.Partition
	dataPart   *tape.Partition
	idxLabel   *ltfs.Label
	dataLabel  *ltfs.Label
	index      *ltfs.Index
	tapeFS     *tapefs.TapeFS
	fuseFS     *tapefs.FS
	fuseConn   *fuse.Conn
	mountPoint string
}

type Daemon struct {
	mu      sync.Mutex
	entries map[string]*entry
}

func New() *Daemon {
	return &Daemon{entries: make(map[string]*entry)}
}

func (d *Daemon) Serve(sock net.Listener) {
	for {
		conn, err := sock.Accept()
		if err != nil {
			return
		}
		go d.handleConn(conn)
	}
}

func (d *Daemon) handleConn(conn net.Conn) {
	defer conn.Close()
	scanner := bufio.NewScanner(conn)
	enc := json.NewEncoder(conn)
	for scanner.Scan() {
		var req Request
		if err := json.Unmarshal(scanner.Bytes(), &req); err != nil {
			_ = enc.Encode(Response{OK: false, Error: "bad request: " + err.Error()})
			continue
		}
		resp := d.dispatch(req)
		_ = enc.Encode(resp)
	}
}

func (d *Daemon) dispatch(req Request) Response {
	switch req.Cmd {
	case "assign":
		if len(req.Args) != 2 {
			return Response{OK: false, Error: "assign requires <device> <letter>"}
		}
		return d.cmdAssign(req.Args[0], req.Args[1])
	case "unassign":
		if len(req.Args) != 1 {
			return Response{OK: false, Error: "unassign requires <letter>"}
		}
		return d.cmdUnassign(req.Args[0])
	case "init":
		if len(req.Args) != 1 {
			return Response{OK: false, Error: "init requires <letter>"}
		}
		return d.cmdInit(req.Args[0])
	case "load":
		if len(req.Args) != 1 {
			return Response{OK: false, Error: "load requires <letter>"}
		}
		return d.cmdLoad(req.Args[0])
	case "commit":
		if len(req.Args) != 1 {
			return Response{OK: false, Error: "commit requires <letter>"}
		}
		return d.cmdCommit(req.Args[0])
	case "discard":
		if len(req.Args) != 1 {
			return Response{OK: false, Error: "discard requires <letter>"}
		}
		return d.cmdDiscard(req.Args[0])
	case "mount":
		if len(req.Args) != 2 {
			return Response{OK: false, Error: "mount requires <letter> <mountpoint>"}
		}
		return d.cmdMount(req.Args[0], req.Args[1])
	case "umount":
		if len(req.Args) != 1 {
			return Response{OK: false, Error: "umount requires <letter>"}
		}
		return d.cmdUmount(req.Args[0])
	case "eject":
		if len(req.Args) != 1 {
			return Response{OK: false, Error: "eject requires <letter>"}
		}
		return d.cmdEject(req.Args[0])
	default:
		return Response{OK: false, Error: "unknown command: " + req.Cmd}
	}
}

func (d *Daemon) cmdAssign(device, letter string) Response {
	d.mu.Lock()
	defer d.mu.Unlock()
	if _, ok := d.entries[letter]; ok {
		return Response{OK: false, Error: fmt.Sprintf("letter %q already assigned", letter)}
	}
	d.entries[letter] = &entry{device: device}
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
	e.index = t.Index
	e.tapeFS = t
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
	rec, err := e.idxPart.ReadAt(3)
	if err != nil {
		return Response{OK: false, Error: "read index from tape: " + err.Error()}
	}
	idx, err := ltfs.ParseIndex(rec.Data)
	if err != nil {
		return Response{OK: false, Error: "parse index: " + err.Error()}
	}
	e.index = idx
	e.tapeFS = tapefs.NewTapeFSFromParts(e.device, e.idxPart, e.dataPart, e.idxLabel, e.dataLabel, idx)
	if err := os.MkdirAll(IndexDir, 0o755); err != nil {
		return Response{OK: false, Error: err.Error()}
	}
	if err := ltfs.SaveIndexToFile(IndexPath(letter), idx); err != nil {
		return Response{OK: false, Error: "save index to disk: " + err.Error()}
	}
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
	if err := os.MkdirAll(IndexDir, 0o755); err != nil {
		return Response{OK: false, Error: err.Error()}
	}
	if err := ltfs.SaveIndexToFile(IndexPath(letter), e.tapeFS.Index); err != nil {
		return Response{OK: false, Error: "save to disk: " + err.Error()}
	}
	if err := e.tapeFS.FlushIndex(); err != nil {
		return Response{OK: false, Error: "flush to tape: " + err.Error()}
	}
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
	e.index = nil
	if e.tapeFS != nil {
		_ = e.tapeFS.Close()
		e.tapeFS = nil
		e.idxPart = nil
		e.dataPart = nil
	}
	_ = os.Remove(IndexPath(letter))
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
	if err := os.MkdirAll(mountPoint, 0o755); err != nil {
		return Response{OK: false, Error: err.Error()}
	}
	conn, err := fuse.Mount(mountPoint, fuse.FSName("ltape-"+letter), fuse.Subtype("ltfs"))
	if err != nil {
		return Response{OK: false, Error: "fuse mount: " + err.Error()}
	}
	fuseFS := tapefs.New(e.tapeFS)
	e.fuseConn = conn
	e.fuseFS = fuseFS
	e.mountPoint = mountPoint
	go func() {
		_ = bazilfs.Serve(conn, fuseFS)
		conn.Close()
	}()
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
	return Response{OK: true}
}

func (d *Daemon) cmdEject(letter string) Response {
	d.mu.Lock()
	defer d.mu.Unlock()
	e, ok := d.entries[letter]
	if !ok {
		return Response{OK: false, Error: fmt.Sprintf("letter %q not assigned", letter)}
	}
	if e.mountPoint != "" {
		if err := fuse.Unmount(e.mountPoint); err != nil {
			return Response{OK: false, Error: "unmount before eject: " + err.Error()}
		}
		e.mountPoint = ""
		e.fuseConn = nil
		e.fuseFS = nil
	}
	d.closeEntry(e)
	delete(d.entries, letter)
	return Response{OK: true}
}

func (d *Daemon) closeEntry(e *entry) {
	if e.tapeFS != nil {
		_ = e.tapeFS.Close()
		e.tapeFS = nil
		e.idxPart = nil
		e.dataPart = nil
	} else {
		if e.idxPart != nil {
			_ = e.idxPart.Close()
			e.idxPart = nil
		}
		if e.dataPart != nil {
			_ = e.dataPart.Close()
			e.dataPart = nil
		}
	}
	e.index = nil
}
