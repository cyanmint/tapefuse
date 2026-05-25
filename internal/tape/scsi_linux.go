//go:build linux

package tape

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"unsafe"

	"github.com/google/uuid"
	"golang.org/x/sys/unix"

	"github.com/cyanmint/tapefuse/internal/ltfs"
)

const (
	mtIoctlTop = 0x40086d01
	mtFSF      = 1
	mtFSR      = 3
	mtWEOF     = 6
	mtREW      = 7
	mtOFFL     = 8

	maxSCSIRecordSize = 512 * 1024
	scsiDataOffset    = 6
)

type mtop struct {
	Op    int16
	Count int32
}

type SCSITape struct {
	mu         sync.Mutex
	f          *os.File
	fd         int
	blockTypes []RecordType
	writePos   uint64
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
		return nil, err
	}
	return &SCSITape{f: f, fd: int(f.Fd())}, nil
}

func (t *SCSITape) initialize(device string) error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if err := ioctlMtop(t.fd, mtREW, 1); err != nil {
		return err
	}
	t.blockTypes = nil
	t.writePos = 0

	volumeUUID := uuid.NewString()
	idxLabel := ltfs.NewLabel("a", volumeUUID)
	index := ltfs.NewEmptyIndex(volumeUUID)

	idxLabelXML, err := idxLabel.Marshal()
	if err != nil {
		return err
	}
	indexXML, err := index.Marshal()
	if err != nil {
		return err
	}

	for _, rec := range []Record{
		{Type: RecordData, Data: makeVOL1Label(device)},
		{Type: RecordData, Data: idxLabelXML},
	} {
		if _, err := t.writeRecordLocked(rec); err != nil {
			return err
		}
	}
	if _, err := t.writeFilemarkLocked(); err != nil {
		return err
	}
	if _, err := t.writeRecordLocked(Record{Type: RecordData, Data: indexXML}); err != nil {
		return err
	}
	if _, err := t.writeFilemarkLocked(); err != nil {
		return err
	}
	if _, err := t.writeFilemarkLocked(); err != nil {
		return err
	}
	return t.f.Sync()
}

func (t *SCSITape) scan() error {
	t.mu.Lock()
	defer t.mu.Unlock()

	if err := ioctlMtop(t.fd, mtREW, 1); err != nil {
		return err
	}
	t.blockTypes = t.blockTypes[:0]
	for {
		rec, err := t.readOneLocked()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			return err
		}
		t.blockTypes = append(t.blockTypes, rec.Type)
	}
	t.writePos = uint64(len(t.blockTypes))
	return nil
}

func (t *SCSITape) ReadAt(blockNum uint64) (*Record, error) {
	t.mu.Lock()
	defer t.mu.Unlock()

	if blockNum >= uint64(len(t.blockTypes)) {
		return nil, io.EOF
	}
	if err := t.positionToLocked(blockNum); err != nil {
		return nil, err
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

func (t *SCSITape) Sync() error {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.f.Sync()
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
	return ioctlMtop(t.fd, mtOFFL, 1)
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
	rec, err := r.ReadAt(count - 1)
	if err != nil {
		return 0, false, err
	}
	return rec.Type, true, nil
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

func (t *SCSITape) writeRecordLocked(rec Record) (uint64, error) {
	if rec.Type == RecordFilemark {
		return t.writeFilemarkLocked()
	}
	if rec.Type == RecordEOD {
		return t.writeFilemarkLocked()
	}
	if rec.Type != RecordData {
		return 0, fmt.Errorf("unsupported record type %d", rec.Type)
	}
	if err := t.positionToLocked(t.writePos); err != nil {
		return 0, err
	}
	n, err := unix.Write(t.fd, rec.Data)
	if err != nil {
		return 0, err
	}
	if n != len(rec.Data) {
		return 0, io.ErrShortWrite
	}
	blockNum := t.writePos
	if blockNum < uint64(len(t.blockTypes)) {
		t.blockTypes = t.blockTypes[:blockNum]
	}
	t.blockTypes = append(t.blockTypes, RecordData)
	t.writePos++
	return blockNum, nil
}

func (t *SCSITape) writeFilemarkLocked() (uint64, error) {
	if err := t.positionToLocked(t.writePos); err != nil {
		return 0, err
	}
	if err := ioctlMtop(t.fd, mtWEOF, 1); err != nil {
		return 0, err
	}
	blockNum := t.writePos
	if blockNum < uint64(len(t.blockTypes)) {
		t.blockTypes = t.blockTypes[:blockNum]
	}
	t.blockTypes = append(t.blockTypes, RecordFilemark)
	t.writePos++
	return blockNum, nil
}

func (t *SCSITape) positionToLocked(blockNum uint64) error {
	if err := ioctlMtop(t.fd, mtREW, 1); err != nil {
		return err
	}
	for i := uint64(0); i < blockNum && i < uint64(len(t.blockTypes)); i++ {
		switch t.blockTypes[i] {
		case RecordFilemark:
			if err := ioctlMtop(t.fd, mtFSF, 1); err != nil {
				return err
			}
		default:
			if err := ioctlMtop(t.fd, mtFSR, 1); err != nil {
				return err
			}
		}
	}
	return nil
}

func (t *SCSITape) readOneLocked() (*Record, error) {
	buf := make([]byte, maxSCSIRecordSize)
	n, err := unix.Read(t.fd, buf)
	if err != nil {
		if errors.Is(err, unix.ENOSPC) || errors.Is(err, unix.ENODATA) || errors.Is(err, unix.EIO) {
			return nil, io.EOF
		}
		return nil, err
	}
	if n == 0 {
		return &Record{Type: RecordFilemark}, nil
	}
	return &Record{Type: RecordData, Data: append([]byte(nil), buf[:n]...)}, nil
}

func ioctlMtop(fd int, op int16, count int32) error {
	mt := mtop{Op: op, Count: count}
	_, _, errno := unix.Syscall(unix.SYS_IOCTL, uintptr(fd), uintptr(mtIoctlTop), uintptr(unsafe.Pointer(&mt)))
	if errno != 0 {
		return errno
	}
	return nil
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
