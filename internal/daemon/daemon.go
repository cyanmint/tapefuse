package daemon

import (
	"bufio"
	"encoding/json"
	"log"
	"net"
	"sync"

	"bazil.org/fuse"

	tapefs "github.com/cyanmint/tapefuse/fs"
	"github.com/cyanmint/tapefuse/internal/ltfs"
	"github.com/cyanmint/tapefuse/internal/tape"
)

type entry struct {
	device     string
	idxPart    tape.Tape
	dataPart   tape.Tape
	idxLabel   *ltfs.Label
	dataLabel  *ltfs.Label
	tapeFS     *tapefs.TapeFS
	fuseFS     *tapefs.FS
	fuseConn   *fuse.Conn
	mountPoint string
	fuseDone   chan struct{}
}

type Daemon struct {
	mu      sync.Mutex
	entries map[string]*entry
}

func New() *Daemon {
	return &Daemon{entries: make(map[string]*entry)}
}

func (d *Daemon) logf(format string, args ...any) {
	log.Printf("ltaped: "+format, args...)
}

func (d *Daemon) Serve(sock net.Listener) {
	d.logf("serving on %s", sock.Addr())
	for {
		conn, err := sock.Accept()
		if err != nil {
			d.logf("listener stopped: %v", err)
			return
		}
		d.logf("accepted connection from %s", conn.RemoteAddr())
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
			d.logf("rejecting malformed request from %s: %v", conn.RemoteAddr(), err)
			_ = enc.Encode(Response{OK: false, Error: "bad request: " + err.Error()})
			continue
		}
		d.logf("received command=%s args=%v", req.Cmd, req.Args)
		resp := d.dispatch(req)
		if resp.OK {
			d.logf("command=%s completed successfully", req.Cmd)
		} else {
			d.logf("command=%s failed: %s", req.Cmd, resp.Error)
		}
		_ = enc.Encode(resp)
	}
	if err := scanner.Err(); err != nil {
		d.logf("connection read error from %s: %v", conn.RemoteAddr(), err)
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
	case "swallow":
		if len(req.Args) != 1 {
			return Response{OK: false, Error: "swallow requires <letter>"}
		}
		return d.cmdSwallow(req.Args[0])
	case "list":
		if len(req.Args) != 0 {
			return Response{OK: false, Error: "list takes no arguments"}
		}
		return d.cmdList()
	default:
		return Response{OK: false, Error: "unknown command: " + req.Cmd}
	}
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
	e.idxLabel = nil
	e.dataLabel = nil
	e.fuseFS = nil
	e.fuseConn = nil
	e.fuseDone = nil
}
