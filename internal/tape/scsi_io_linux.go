//go:build linux

package tape

import (
	"errors"
	"io"
	"log"
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

	log.Printf("tape %s: init: rewinding", t.device)
	if err := ioctlMtop(t.fd, mtREW, 1); err != nil {
		return err
	}
	t.blockTypes = nil
	t.writePos = 0
	t.tapPos = 0 // tape head is now at BOT

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

	log.Printf("tape %s: init: writing VOL1 label (%d bytes)", t.device, 80)
	if _, err := t.writeRecordLocked(Record{Type: RecordData, Data: makeVOL1Label(device)}); err != nil {
		return err
	}
	log.Printf("tape %s: init: writing LTFS index label (%d bytes)", t.device, len(idxLabelXML))
	if _, err := t.writeRecordLocked(Record{Type: RecordData, Data: idxLabelXML}); err != nil {
		return err
	}
	log.Printf("tape %s: init: writing filemark", t.device)
	if _, err := t.writeFilemarkLocked(); err != nil {
		return err
	}
	log.Printf("tape %s: init: writing LTFS index (%d bytes)", t.device, len(indexXML))
	if _, err := t.writeRecordLocked(Record{Type: RecordData, Data: indexXML}); err != nil {
		return err
	}
	log.Printf("tape %s: init: writing filemark", t.device)
	if _, err := t.writeFilemarkLocked(); err != nil {
		return err
	}
	log.Printf("tape %s: init: writing EOD filemark", t.device)
	if _, err := t.writeFilemarkLocked(); err != nil {
		return err
	}
	log.Printf("tape %s: init: syncing; tape initialized with %d blocks", t.device, len(t.blockTypes))
	// MTWEOF 0 flushes the drive write buffer without writing an additional filemark.
	return ioctlMtop(t.fd, mtWEOF, 0)
}

func (t *SCSITape) scan() error {
	t.mu.Lock()
	defer t.mu.Unlock()

	log.Printf("tape %s: scan: rewinding", t.device)
	if err := ioctlMtop(t.fd, mtREW, 1); err != nil {
		return err
	}
	t.blockTypes = t.blockTypes[:0]
	t.tapPos = 0
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
	log.Printf("tape %s: scan: complete, found %d blocks", t.device, len(t.blockTypes))
	return nil
}

func (t *SCSITape) writeRecordLocked(rec Record) (uint64, error) {
	if rec.Type == RecordFilemark || rec.Type == RecordEOD {
		return t.writeFilemarkLocked()
	}
	if rec.Type != RecordData {
		return 0, errors.New("unsupported record type")
	}
	// Only seek if the tape head is not already at the write position.
	if t.tapPos != t.writePos {
		if err := t.positionToLocked(t.writePos); err != nil {
			return 0, err
		}
	}
	log.Printf("tape %s: write: data record at block %d (%d bytes)", t.device, t.writePos, len(rec.Data))
	n, err := unix.Write(t.fd, rec.Data)
	if err != nil {
		t.tapPos = unknownTapePos
		return 0, err
	}
	if n != len(rec.Data) {
		t.tapPos = unknownTapePos
		return 0, io.ErrShortWrite
	}
	blockNum := t.writePos
	if blockNum < uint64(len(t.blockTypes)) {
		t.blockTypes = t.blockTypes[:blockNum]
	}
	t.blockTypes = append(t.blockTypes, RecordData)
	t.tapPos = blockNum + 1
	t.writePos++
	return blockNum, nil
}

func (t *SCSITape) writeFilemarkLocked() (uint64, error) {
	// Only seek if the tape head is not already at the write position.
	if t.tapPos != t.writePos {
		if err := t.positionToLocked(t.writePos); err != nil {
			return 0, err
		}
	}
	log.Printf("tape %s: write: filemark at block %d", t.device, t.writePos)
	if err := ioctlMtop(t.fd, mtWEOF, 1); err != nil {
		t.tapPos = unknownTapePos
		return 0, err
	}
	blockNum := t.writePos
	if blockNum < uint64(len(t.blockTypes)) {
		t.blockTypes = t.blockTypes[:blockNum]
	}
	t.blockTypes = append(t.blockTypes, RecordFilemark)
	t.tapPos = blockNum + 1
	t.writePos++
	return blockNum, nil
}

func (t *SCSITape) positionToLocked(blockNum uint64) error {
	log.Printf("tape %s: seek: rewind, then forward to block %d", t.device, blockNum)
	if err := ioctlMtop(t.fd, mtREW, 1); err != nil {
		t.tapPos = unknownTapePos
		return err
	}
	t.tapPos = 0
	for i := uint64(0); i < blockNum && i < uint64(len(t.blockTypes)); i++ {
		switch t.blockTypes[i] {
		case RecordFilemark:
			if err := ioctlMtop(t.fd, mtFSF, 1); err != nil {
				t.tapPos = unknownTapePos
				return err
			}
		default:
			if err := ioctlMtop(t.fd, mtFSR, 1); err != nil {
				t.tapPos = unknownTapePos
				return err
			}
		}
		t.tapPos++
	}
	return nil
}

func (t *SCSITape) readOneLocked() (*Record, error) {
	buf := make([]byte, maxSCSIRecordSize)
	n, err := unix.Read(t.fd, buf)
	if err != nil {
		if errors.Is(err, unix.ENOSPC) || errors.Is(err, unix.ENODATA) || errors.Is(err, unix.EIO) {
			log.Printf("tape %s: read: EOD/EOF", t.device)
			return nil, io.EOF
		}
		t.tapPos = unknownTapePos
		return nil, err
	}
	if n == 0 {
		log.Printf("tape %s: read: filemark", t.device)
		t.tapPos++
		return &Record{Type: RecordFilemark}, nil
	}
	log.Printf("tape %s: read: data block (%d bytes)", t.device, n)
	t.tapPos++
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
