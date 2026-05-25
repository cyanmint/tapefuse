//go:build linux

package tape

import (
	"errors"
	"io"
	"path/filepath"
	"strings"
	"unsafe"

	"github.com/google/uuid"
	"golang.org/x/sys/unix"

	"github.com/cyanmint/tapefuse/internal/ltfs"
)

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

func (t *SCSITape) writeRecordLocked(rec Record) (uint64, error) {
	if rec.Type == RecordFilemark || rec.Type == RecordEOD {
		return t.writeFilemarkLocked()
	}
	if rec.Type != RecordData {
		return 0, errors.New("unsupported record type")
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
