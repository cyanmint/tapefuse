package daemon

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
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
	bufCfg     tapefs.BufferConfig
	// memMode is true when the working index is kept in memory (indexread -m)
	// rather than in the on-disk cache file.  memIndex holds that index.
	memMode  bool
	memIndex *ltfs.Index
}

// loadIndex returns the working index for e, either from memory (memMode) or
// from the on-disk cache file for letter.
func (d *Daemon) loadIndex(letter string, e *entry) (*ltfs.Index, error) {
	if e.memMode {
		if e.memIndex == nil {
			return nil, fmt.Errorf("index not loaded; run indexread first")
		}
		return e.memIndex, nil
	}
	return ltfs.LoadIndexFromFile(IndexPath(letter))
}

// saveIndex persists idx as the working index for e.
func (d *Daemon) saveIndex(letter string, e *entry, idx *ltfs.Index) error {
	if e.memMode {
		e.memIndex = idx
		return nil
	}
	if err := os.MkdirAll(IndexDir, 0o755); err != nil {
		return err
	}
	return ltfs.SaveIndexToFile(IndexPath(letter), idx)
}

// physCapFn returns a function suitable for ltfs.Index.FreeFileExtents that
// reports the physical block capacity for e's data partition.
func (d *Daemon) physCapFn(e *entry) func(int64) int64 {
	if e.tapeFS == nil {
		return nil
	}
	return func(startBlock int64) int64 {
		return e.tapeFS.DataBlockCapacity(uint64(startBlock))
	}
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
	cmd, err := ResolveCmd(req.Cmd, KnownCmds)
	if err != nil {
		return Response{OK: false, Error: err.Error()}
	}
	switch cmd {
	case "assign":
		if len(req.Args) != 2 {
			return Response{OK: false, Error: "assign requires <device> <letter>"}
		}
		return d.cmdAssign(req.Args[0], req.Args[1])
	case "indexread":
		if len(req.Args) != 2 {
			return Response{OK: false, Error: "indexread requires <letter> <mode>"}
		}
		return d.cmdIndexRead(req.Args[0], req.Args[1])
	case "ls":
		if len(req.Args) != 2 {
			return Response{OK: false, Error: "ls requires <letter> <path>"}
		}
		return d.cmdLs(req.Args[0], req.Args[1])
	case "rm":
		if len(req.Args) != 3 {
			return Response{OK: false, Error: "rm requires <letter> <path> <recursive>"}
		}
		return d.cmdRm(req.Args[0], req.Args[1], req.Args[2] == "true")
	case "get":
		if len(req.Args) != 3 {
			return Response{OK: false, Error: "get requires <letter> <path> <dest>"}
		}
		return d.cmdGet(req.Args[0], req.Args[1], req.Args[2])
	case "push":
		if len(req.Args) != 3 {
			return Response{OK: false, Error: "push requires <letter> <path> <src>"}
		}
		return d.cmdPush(req.Args[0], req.Args[1], req.Args[2])
	case "mount":
		if len(req.Args) != 3 {
			return Response{OK: false, Error: "mount requires <letter> <mountpoint> <tempkind>"}
		}
		bufCfg, err := parseBufferConfig(req.Args[2])
		if err != nil {
			return Response{OK: false, Error: "temp dir config: " + err.Error()}
		}
		return d.cmdMount(req.Args[0], req.Args[1], bufCfg)
	case "umount":
		if len(req.Args) != 1 {
			return Response{OK: false, Error: "umount requires <letter>"}
		}
		return d.cmdUmount(req.Args[0])
	case "flush":
		if len(req.Args) != 1 {
			return Response{OK: false, Error: "flush requires <letter>"}
		}
		return d.cmdFlush(req.Args[0])
	case "commitindex":
		if len(req.Args) != 1 {
			return Response{OK: false, Error: "commitindex requires <letter>"}
		}
		return d.cmdCommitIndex(req.Args[0])
	case "discardindex":
		if len(req.Args) != 1 {
			return Response{OK: false, Error: "discardindex requires <letter>"}
		}
		return d.cmdDiscardIndex(req.Args[0])
	case "defrag":
		if len(req.Args) != 1 {
			return Response{OK: false, Error: "defrag requires <letter>"}
		}
		return d.cmdDefrag(req.Args[0])
	}
	// Unreachable: ResolveCmd guarantees cmd is a known command.
	return Response{OK: false, Error: "unknown command: " + cmd}
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
	e.memMode = false
	e.memIndex = nil
}
