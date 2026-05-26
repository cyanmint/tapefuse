//go:build linux

package tape

import (
	"errors"
	"fmt"
	"io"
	"log"
	"os"
	"sync"

	"golang.org/x/sys/unix"
)

const (
	mtIoctlTop = 0x40086d01
	mtFSF      = 1
	mtFSR      = 3
	mtWEOF     = 5
	mtREW      = 6
	mtOFFL     = 7
	mtLOAD     = 30

	maxSCSIRecordSize = 512 * 1024
	scsiDataOffset    = 6
)

type mtop struct {
	Op    int16
	Count int32
}

// unknownTapePos is a sentinel for tapPos meaning "position not tracked".
const unknownTapePos = ^uint64(0)

type SCSITape struct {
	mu         sync.Mutex
	f          *os.File
	fd         int
	device     string
	blockTypes []RecordType
	writePos   uint64
	// tapPos tracks the current physical tape-head position so that
	// sequential writes and reads can skip the expensive MTREW+FSR seek.
	// Set to unknownTapePos when position is uncertain (e.g. after open
	// before any explicit rewind, or after an error).
	tapPos uint64
}

type SCSIRegionTape struct {
	tape    *SCSITape
	offset  uint64
	isOwner bool
}

var _ Tape = (*SCSIRegionTape)(nil)

func OpenSCSI(device string) (Tape, Tape, error) {
	t, err := openSCSITape(device)
	if err != nil {
		return nil, nil, err
	}
	if err := t.scan(); err != nil {
		_ = t.Close()
		return nil, nil, err
	}
	return &SCSIRegionTape{tape: t, offset: 0}, &SCSIRegionTape{tape: t, offset: scsiDataOffset, isOwner: true}, nil
}

func CreateSCSI(device string) (Tape, Tape, error) {
	t, err := openSCSITape(device)
	if err != nil {
		return nil, nil, err
	}
	if err := t.initialize(device); err != nil {
		_ = t.Close()
		return nil, nil, err
	}
	return &SCSIRegionTape{tape: t, offset: 0}, &SCSIRegionTape{tape: t, offset: scsiDataOffset, isOwner: true}, nil
}

func openSCSITape(device string) (*SCSITape, error) {
	f, err := os.OpenFile(device, os.O_RDWR, 0)
	if err != nil {
		if errors.Is(err, unix.ENOMEDIUM) {
			return nil, fmt.Errorf("%s: no tape loaded — insert a tape and run 'swallow' if needed: %w", device, err)
		}
		return nil, err
	}
	return &SCSITape{f: f, fd: int(f.Fd()), device: device, tapPos: unknownTapePos}, nil
}

// openSCSITapeNonBlocking opens the tape device with O_NONBLOCK so the call
// succeeds even when no tape is currently loaded in the drive.  This is the
// right mode for sending MTLOAD (swallow), because the ioctl itself blocks
// until the drive reports ready.
func openSCSITapeNonBlocking(device string) (*SCSITape, error) {
	f, err := os.OpenFile(device, os.O_RDWR|unix.O_NONBLOCK, 0)
	if err != nil {
		return nil, fmt.Errorf("open tape device %s: %w", device, err)
	}
	return &SCSITape{f: f, fd: int(f.Fd()), device: device, tapPos: unknownTapePos}, nil
}

func SwallowSCSI(device string) error {
	t, err := openSCSITapeNonBlocking(device)
	if err != nil {
		return err
	}
	defer t.Close()
	return t.Swallow()
}

func (t *SCSITape) ReadAt(blockNum uint64) (*Record, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if blockNum >= uint64(len(t.blockTypes)) {
		return nil, io.EOF
	}
	if t.tapPos != blockNum {
		if err := t.positionToLocked(blockNum); err != nil {
			return nil, err
		}
	}
	return t.readOneLocked()
}

func (t *SCSITape) WriteRecord(rec Record) (uint64, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.writeRecordLocked(rec)
}

func (t *SCSITape) WriteFilemark() (uint64, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.writeFilemarkLocked()
}

func (t *SCSITape) WriteEOD() (uint64, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.writeFilemarkLocked()
}

func (t *SCSITape) TruncateAt(blockNum uint64) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if blockNum > uint64(len(t.blockTypes)) {
		return fmt.Errorf("block %d out of range", blockNum)
	}
	log.Printf("tape %s: erase: truncating at block %d (was %d blocks)", t.device, blockNum, len(t.blockTypes))
	if err := t.positionToLocked(blockNum); err != nil {
		return err
	}
	t.blockTypes = t.blockTypes[:blockNum]
	t.writePos = blockNum
	return nil
}

func (t *SCSITape) BlockCount() uint64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return uint64(len(t.blockTypes))
}

func (t *SCSITape) LastRecordType() (RecordType, bool, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if len(t.blockTypes) == 0 {
		return 0, false, nil
	}
	return t.blockTypes[len(t.blockTypes)-1], true, nil
}

// blockTypeAt returns the type of the physical block at the given index from the
// in-memory scan table, without issuing any tape I/O.
func (t *SCSITape) blockTypeAt(n uint64) (RecordType, bool) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if n >= uint64(len(t.blockTypes)) {
		return 0, false
	}
	return t.blockTypes[n], true
}

func (t *SCSITape) Sync() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	// fsync(2) is not supported on SCSI tape character devices (returns EINVAL).
	// MTWEOF with count 0 flushes the drive's write buffer without writing an
	// additional filemark, which is the correct way to sync tape writes.
	return ioctlMtop(t.fd, mtWEOF, 0)
}

func (t *SCSITape) Close() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.f == nil {
		return nil
	}
	err := t.f.Close()
	t.f = nil
	t.fd = -1
	return err
}

func (t *SCSITape) Eject() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.fd < 0 {
		return nil
	}
	log.Printf("tape %s: issuing MTOFFL (eject)", t.device)
	return ioctlMtop(t.fd, mtOFFL, 1)
}

func (t *SCSITape) Swallow() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.fd < 0 {
		return nil
	}
	log.Printf("tape %s: issuing MTLOAD (load media)", t.device)
	return ioctlMtop(t.fd, mtLOAD, 1)
}

func (r *SCSIRegionTape) ReadAt(blockNum uint64) (*Record, error) {
	return r.tape.ReadAt(r.offset + blockNum)
}

func (r *SCSIRegionTape) WriteRecord(rec Record) (uint64, error) {
	n, err := r.tape.WriteRecord(rec)
	if err != nil {
		return 0, err
	}
	if n < r.offset {
		return 0, fmt.Errorf("write before region offset")
	}
	return n - r.offset, nil
}

func (r *SCSIRegionTape) WriteFilemark() (uint64, error) {
	n, err := r.tape.WriteFilemark()
	if err != nil {
		return 0, err
	}
	if n < r.offset {
		return 0, fmt.Errorf("write before region offset")
	}
	return n - r.offset, nil
}

func (r *SCSIRegionTape) WriteEOD() (uint64, error) {
	n, err := r.tape.WriteEOD()
	if err != nil {
		return 0, err
	}
	if n < r.offset {
		return 0, fmt.Errorf("write before region offset")
	}
	return n - r.offset, nil
}

func (r *SCSIRegionTape) TruncateAt(blockNum uint64) error {
	return r.tape.TruncateAt(r.offset + blockNum)
}

func (r *SCSIRegionTape) BlockCount() uint64 {
	total := r.tape.BlockCount()
	if total <= r.offset {
		return 0
	}
	return total - r.offset
}

func (r *SCSIRegionTape) LastRecordType() (RecordType, bool, error) {
	count := r.BlockCount()
	if count == 0 {
		return 0, false, nil
	}
	// Read the block type from the in-memory scan table rather than issuing a
	// physical tape read.  On real LTO drives, reading the second of two
	// consecutive filemarks (the LTFS double-FM EOD marker) returns EIO, which
	// our read path maps to io.EOF.  That error would propagate up through
	// AppendFileData → FileHandle.Release and silently prevent the index from
	// being updated, leaving all files after the first with Length=0.
	bt, ok := r.tape.blockTypeAt(r.offset + count - 1)
	if !ok {
		return 0, false, nil
	}
	return bt, true, nil
}

func (r *SCSIRegionTape) Sync() error {
	return r.tape.Sync()
}

func (r *SCSIRegionTape) Close() error {
	if r.isOwner {
		return r.tape.Close()
	}
	return nil
}

func (r *SCSIRegionTape) Eject() error {
	if r.isOwner {
		return r.tape.Eject()
	}
	return nil
}

func (r *SCSIRegionTape) Swallow() error {
	if r.isOwner {
		return r.tape.Swallow()
	}
	return nil
}
