package tape

import (
	"encoding/binary"
	"fmt"
	"io"
	"os"
	"sync"
)

type RecordType uint32

const (
	RecordData     RecordType = 0
	RecordFilemark RecordType = 1
	RecordEOD      RecordType = 0xFFFF
)

type Record struct {
	Type RecordType
	Data []byte
}

var _ Tape = (*Partition)(nil)

type Partition struct {
	mu         sync.Mutex
	f          *os.File
	blockIndex []int64
	nextBlock  uint64
}

func Open(path string) (*Partition, error) {
	f, err := os.OpenFile(path, os.O_RDWR, 0o644)
	if err != nil {
		return nil, err
	}
	p := &Partition{f: f}
	if err := p.rebuildIndex(); err != nil {
		_ = f.Close()
		return nil, err
	}
	return p, nil
}

func Create(path string) (*Partition, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR|os.O_TRUNC, 0o644)
	if err != nil {
		return nil, err
	}
	return &Partition{f: f}, nil
}

func (p *Partition) rebuildIndex() error {
	p.mu.Lock()
	defer p.mu.Unlock()

	info, err := p.f.Stat()
	if err != nil {
		return err
	}

	p.blockIndex = p.blockIndex[:0]
	var offset int64
	for offset < info.Size() {
		if offset+8 > info.Size() {
			return fmt.Errorf("short record header at offset %d", offset)
		}
		hdr := make([]byte, 8)
		if _, err := p.f.ReadAt(hdr, offset); err != nil {
			return err
		}
		dataLen := int64(binary.LittleEndian.Uint32(hdr[4:8]))
		next := offset + 8 + dataLen
		if next > info.Size() {
			return fmt.Errorf("record at offset %d exceeds file size", offset)
		}
		p.blockIndex = append(p.blockIndex, offset)
		offset = next
	}
	p.nextBlock = uint64(len(p.blockIndex))
	return nil
}

func (p *Partition) WriteRecord(rec Record) (uint64, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	offset, err := p.f.Seek(0, io.SeekEnd)
	if err != nil {
		return 0, err
	}

	hdr := make([]byte, 8)
	binary.LittleEndian.PutUint32(hdr[0:4], uint32(rec.Type))
	binary.LittleEndian.PutUint32(hdr[4:8], uint32(len(rec.Data)))

	if _, err := p.f.Write(hdr); err != nil {
		return 0, err
	}
	if len(rec.Data) > 0 {
		if _, err := p.f.Write(rec.Data); err != nil {
			return 0, err
		}
	}

	blockNum := p.nextBlock
	p.blockIndex = append(p.blockIndex, offset)
	p.nextBlock++
	return blockNum, nil
}

func (p *Partition) WriteFilemark() (uint64, error) {
	return p.WriteRecord(Record{Type: RecordFilemark})
}

func (p *Partition) WriteEOD() (uint64, error) {
	return p.WriteRecord(Record{Type: RecordEOD})
}

func (p *Partition) ReadAt(blockNum uint64) (*Record, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if blockNum >= p.nextBlock {
		return nil, io.EOF
	}

	offset := p.blockIndex[blockNum]
	hdr := make([]byte, 8)
	if _, err := p.f.ReadAt(hdr, offset); err != nil {
		return nil, err
	}

	rec := &Record{Type: RecordType(binary.LittleEndian.Uint32(hdr[0:4]))}
	dataLen := binary.LittleEndian.Uint32(hdr[4:8])
	if dataLen == 0 {
		return rec, nil
	}

	rec.Data = make([]byte, dataLen)
	if _, err := p.f.ReadAt(rec.Data, offset+8); err != nil {
		return nil, err
	}
	return rec, nil
}

func (p *Partition) TruncateAt(blockNum uint64) error {
	p.mu.Lock()
	defer p.mu.Unlock()

	if blockNum > p.nextBlock {
		return fmt.Errorf("block %d out of range", blockNum)
	}

	var offset int64
	if blockNum == p.nextBlock {
		info, err := p.f.Stat()
		if err != nil {
			return err
		}
		offset = info.Size()
	} else {
		offset = p.blockIndex[blockNum]
	}

	if err := p.f.Truncate(offset); err != nil {
		return err
	}
	p.blockIndex = p.blockIndex[:blockNum]
	p.nextBlock = blockNum
	_, err := p.f.Seek(offset, io.SeekStart)
	return err
}

func (p *Partition) BlockCount() uint64 {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.nextBlock
}

func (p *Partition) LastRecordType() (RecordType, bool, error) {
	p.mu.Lock()
	defer p.mu.Unlock()

	if p.nextBlock == 0 {
		return 0, false, nil
	}

	offset := p.blockIndex[p.nextBlock-1]
	hdr := make([]byte, 8)
	if _, err := p.f.ReadAt(hdr, offset); err != nil {
		return 0, false, err
	}
	return RecordType(binary.LittleEndian.Uint32(hdr[0:4])), true, nil
}

func (p *Partition) Sync() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.f.Sync()
}

func (p *Partition) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.f.Close()
}

func (p *Partition) Eject() error {
	return nil
}
