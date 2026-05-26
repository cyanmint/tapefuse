package fs

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/google/uuid"

	"github.com/cyanmint/tapefuse/internal/ltfs"
	"github.com/cyanmint/tapefuse/internal/tape"
)

func OpenTape(device string) (*TapeFS, error) {
	isChar, err := tape.IsCharDevice(device)
	if err == nil && isChar {
		idxPart, dataPart, idxLabel, dataLabel, err := openSCSIPartitions(device)
		if err != nil {
			return nil, err
		}
		return NewTapeFSFromParts(device, idxPart, dataPart, idxLabel, dataLabel), nil
	}

	p0 := device + ".p0.dat"
	p1 := device + ".p1.dat"
	_, err0 := os.Stat(p0)
	_, err1 := os.Stat(p1)
	if errors.Is(err0, os.ErrNotExist) && errors.Is(err1, os.ErrNotExist) {
		return formatTape(device)
	}
	if err0 != nil && !errors.Is(err0, os.ErrNotExist) {
		return nil, err0
	}
	if err1 != nil && !errors.Is(err1, os.ErrNotExist) {
		return nil, err1
	}
	if errors.Is(err0, os.ErrNotExist) || errors.Is(err1, os.ErrNotExist) {
		return nil, fmt.Errorf("tape image is incomplete for device %q", device)
	}

	idxPart, dataPart, idxLabel, dataLabel, err := OpenTapePartitions(device)
	if err != nil {
		return nil, err
	}
	return NewTapeFSFromParts(device, idxPart, dataPart, idxLabel, dataLabel), nil
}

func OpenTapePartitions(device string) (tape.Tape, tape.Tape, *ltfs.Label, *ltfs.Label, error) {
	isChar, err := tape.IsCharDevice(device)
	if err == nil && isChar {
		return openSCSIPartitions(device)
	}
	return openFileTapePartitions(device)
}

func openFileTapePartitions(device string) (tape.Tape, tape.Tape, *ltfs.Label, *ltfs.Label, error) {
	p0 := device + ".p0.dat"
	p1 := device + ".p1.dat"
	idxPart, err := tape.Open(p0)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	dataPart, err := tape.Open(p1)
	if err != nil {
		_ = idxPart.Close()
		return nil, nil, nil, nil, err
	}
	idxLabelRec, err := idxPart.ReadAt(1)
	if err != nil {
		_ = idxPart.Close()
		_ = dataPart.Close()
		return nil, nil, nil, nil, err
	}
	idxLabel, err := ltfs.ParseLabel(idxLabelRec.Data)
	if err != nil {
		_ = idxPart.Close()
		_ = dataPart.Close()
		return nil, nil, nil, nil, err
	}
	dataLabelRec, err := dataPart.ReadAt(1)
	if err != nil {
		_ = idxPart.Close()
		_ = dataPart.Close()
		return nil, nil, nil, nil, err
	}
	dataLabel, err := ltfs.ParseLabel(dataLabelRec.Data)
	if err != nil {
		_ = idxPart.Close()
		_ = dataPart.Close()
		return nil, nil, nil, nil, err
	}
	return idxPart, dataPart, idxLabel, dataLabel, nil
}

func openSCSIPartitions(device string) (tape.Tape, tape.Tape, *ltfs.Label, *ltfs.Label, error) {
	idxPart, dataPart, err := tape.OpenSCSI(device)
	if err != nil {
		return nil, nil, nil, nil, err
	}
	idxLabelRec, err := idxPart.ReadAt(1)
	if err != nil {
		_ = idxPart.Close()
		_ = dataPart.Close()
		return nil, nil, nil, nil, err
	}
	idxLabel, err := ltfs.ParseLabel(idxLabelRec.Data)
	if err != nil {
		_ = idxPart.Close()
		_ = dataPart.Close()
		return nil, nil, nil, nil, err
	}
	dataLabel := ltfs.NewLabel("b", idxLabel.VolumeUUID)
	return idxPart, dataPart, idxLabel, dataLabel, nil
}

func formatTape(device string) (*TapeFS, error) {
	p0 := device + ".p0.dat"
	p1 := device + ".p1.dat"

	idxPart, err := tape.Create(p0)
	if err != nil {
		return nil, err
	}
	dataPart, err := tape.Create(p1)
	if err != nil {
		_ = idxPart.Close()
		return nil, err
	}

	closeBoth := func() {
		_ = idxPart.Close()
		_ = dataPart.Close()
	}

	volumeUUID := uuid.NewString()
	idxLabel := ltfs.NewLabel("a", volumeUUID)
	dataLabel := ltfs.NewLabel("b", volumeUUID)
	index := ltfs.NewEmptyIndex(volumeUUID)

	vol1 := makeVOL1Label(device)
	idxLabelXML, err := idxLabel.Marshal()
	if err != nil {
		closeBoth()
		return nil, err
	}
	dataLabelXML, err := dataLabel.Marshal()
	if err != nil {
		closeBoth()
		return nil, err
	}
	indexXML, err := index.Marshal()
	if err != nil {
		closeBoth()
		return nil, err
	}

	if _, err := idxPart.WriteRecord(tape.Record{Type: tape.RecordData, Data: vol1}); err != nil {
		closeBoth()
		return nil, err
	}
	if _, err := idxPart.WriteRecord(tape.Record{Type: tape.RecordData, Data: idxLabelXML}); err != nil {
		closeBoth()
		return nil, err
	}
	if _, err := idxPart.WriteFilemark(); err != nil {
		closeBoth()
		return nil, err
	}
	if _, err := idxPart.WriteRecord(tape.Record{Type: tape.RecordData, Data: indexXML}); err != nil {
		closeBoth()
		return nil, err
	}
	if _, err := idxPart.WriteFilemark(); err != nil {
		closeBoth()
		return nil, err
	}
	if _, err := idxPart.WriteFilemark(); err != nil {
		closeBoth()
		return nil, err
	}

	if _, err := dataPart.WriteRecord(tape.Record{Type: tape.RecordData, Data: vol1}); err != nil {
		closeBoth()
		return nil, err
	}
	if _, err := dataPart.WriteRecord(tape.Record{Type: tape.RecordData, Data: dataLabelXML}); err != nil {
		closeBoth()
		return nil, err
	}
	if _, err := dataPart.WriteFilemark(); err != nil {
		closeBoth()
		return nil, err
	}
	if _, err := dataPart.WriteEOD(); err != nil {
		closeBoth()
		return nil, err
	}

	if err := idxPart.Sync(); err != nil {
		closeBoth()
		return nil, err
	}
	if err := dataPart.Sync(); err != nil {
		closeBoth()
		return nil, err
	}

	return &TapeFS{
		Device:         device,
		IndexPartition: idxPart,
		DataPartition:  dataPart,
		IndexLabel:     idxLabel,
		DataLabel:      dataLabel,
	}, nil
}

func formatSCSITape(device string) (*TapeFS, error) {
	idxPart, dataPart, err := tape.CreateSCSI(device)
	if err != nil {
		return nil, err
	}
	idxLabelRec, err := idxPart.ReadAt(1)
	if err != nil {
		_ = idxPart.Close()
		_ = dataPart.Close()
		return nil, err
	}
	idxLabel, err := ltfs.ParseLabel(idxLabelRec.Data)
	if err != nil {
		_ = idxPart.Close()
		_ = dataPart.Close()
		return nil, err
	}
	dataLabel := ltfs.NewLabel("b", idxLabel.VolumeUUID)
	return &TapeFS{
		Device:         device,
		IndexPartition: idxPart,
		DataPartition:  dataPart,
		IndexLabel:     idxLabel,
		DataLabel:      dataLabel,
	}, nil
}

func NewTapeFSFromParts(device string, idxPart, dataPart tape.Tape, idxLabel, dataLabel *ltfs.Label) *TapeFS {
	return &TapeFS{
		Device:         device,
		IndexPartition: idxPart,
		DataPartition:  dataPart,
		IndexLabel:     idxLabel,
		DataLabel:      dataLabel,
	}
}

func FormatTape(device string) (*TapeFS, error) {
	isChar, err := tape.IsCharDevice(device)
	if err == nil && isChar {
		return formatSCSITape(device)
	}
	return formatTape(device)
}

func (t *TapeFS) Close() error {
	var firstErr error
	if err := t.IndexPartition.Close(); err != nil && firstErr == nil {
		firstErr = err
	}
	if err := t.DataPartition.Close(); err != nil && firstErr == nil {
		firstErr = err
	}
	return firstErr
}

func (t *TapeFS) ReadIndexFromTape() (*ltfs.Index, error) {
	rec, err := t.IndexPartition.ReadAt(3)
	if err != nil {
		return nil, err
	}
	return ltfs.ParseIndex(rec.Data)
}

func (t *TapeFS) FlushIndex(idx *ltfs.Index) error {
	prev := idx.Location
	idx.GenerationNumber++
	idx.UpdateTime = ltfs.Now()
	idx.PrevGenLocation = prev
	idx.Location = ltfs.Location{Partition: "a", StartBlock: 3}

	data, err := idx.Marshal()
	if err != nil {
		return err
	}
	if err := t.IndexPartition.TruncateAt(3); err != nil {
		return err
	}
	blockNum, err := t.IndexPartition.WriteRecord(tape.Record{Type: tape.RecordData, Data: data})
	if err != nil {
		return err
	}
	idx.Location = ltfs.Location{Partition: "a", StartBlock: blockNum}
	if _, err := t.IndexPartition.WriteFilemark(); err != nil {
		return err
	}
	if _, err := t.IndexPartition.WriteFilemark(); err != nil {
		return err
	}
	return t.IndexPartition.Sync()
}

func (t *TapeFS) ReadFileData(file *ltfs.File) ([]byte, error) {
	return t.ReadFileRange(file, 0, int(file.Length))
}

func (t *TapeFS) ReadFileRange(file *ltfs.File, offset int64, size int) ([]byte, error) {
	if size <= 0 || offset >= file.Length {
		return []byte{}, nil
	}
	if offset < 0 {
		return nil, fmt.Errorf("negative offset")
	}

	maxEnd := offset + int64(size)
	if maxEnd > file.Length {
		maxEnd = file.Length
	}

	out := make([]byte, 0, maxEnd-offset)
	for _, extent := range file.ExtentInfo.Extents {
		extentStart := extent.FileOffset
		extentEnd := extent.FileOffset + extent.ByteCount
		if maxEnd <= extentStart || offset >= extentEnd {
			continue
		}

		part := t.partitionFor(extent.Partition)
		if part == nil {
			return nil, fmt.Errorf("unknown partition %q", extent.Partition)
		}
		rec, err := part.ReadAt(uint64(extent.StartBlock))
		if err != nil {
			return nil, err
		}
		if rec.Type != tape.RecordData {
			return nil, fmt.Errorf("block %d is not a data record", extent.StartBlock)
		}

		dataStart := extent.ByteOffset
		dataEnd := extent.ByteOffset + extent.ByteCount
		if dataStart < 0 || dataEnd < dataStart || dataEnd > int64(len(rec.Data)) {
			return nil, fmt.Errorf("extent exceeds record bounds")
		}
		recordData := rec.Data[dataStart:dataEnd]

		copyStart := max64(offset, extentStart)
		copyEnd := min64(maxEnd, extentEnd)
		if copyEnd <= copyStart {
			continue
		}
		startInExtent := copyStart - extentStart
		endInExtent := copyEnd - extentStart
		out = append(out, recordData[startInExtent:endInExtent]...)
	}
	return out, nil
}

func (t *TapeFS) AppendFileData(data []byte) (ltfs.Extent, error) {
	if lastType, ok, err := t.DataPartition.LastRecordType(); err != nil {
		return ltfs.Extent{}, err
	} else if ok && lastType == tape.RecordEOD {
		if err := t.DataPartition.TruncateAt(t.DataPartition.BlockCount() - 1); err != nil {
			return ltfs.Extent{}, err
		}
	}

	blockNum, err := t.DataPartition.WriteRecord(tape.Record{Type: tape.RecordData, Data: data})
	if err != nil {
		return ltfs.Extent{}, err
	}
	if _, err := t.DataPartition.WriteFilemark(); err != nil {
		return ltfs.Extent{}, err
	}
	if _, err := t.DataPartition.WriteEOD(); err != nil {
		return ltfs.Extent{}, err
	}
	if err := t.DataPartition.Sync(); err != nil {
		return ltfs.Extent{}, err
	}

	return ltfs.Extent{
		FileOffset: 0,
		Partition:  "b",
		StartBlock: int64(blockNum),
		ByteOffset: 0,
		ByteCount:  int64(len(data)),
	}, nil
}

// DataBlockCapacity returns the physical byte capacity of the data block at
// blockNum. Returns -1 if the underlying backend does not support in-place
// block inspection (e.g. SCSI tape).
func (t *TapeFS) DataBlockCapacity(blockNum uint64) int64 {
	part, ok := t.DataPartition.(*tape.Partition)
	if !ok {
		return -1
	}
	n, err := part.BlockDataLen(blockNum)
	if err != nil {
		return -1
	}
	return n
}

// WriteFileData writes data to the tape, preferring to reuse a previously
// freed block (sparse appending) before falling back to appending at the tail.
//
// Best-fit selection: among available spaces whose ByteCount >= len(data),
// the one with the smallest ByteCount is chosen.
//
// File-backed tape: the best-fit block is overwritten in-place with zero-
// padding to preserve the block's physical size.
//
// SCSI tape: the tape head is rewound to the best-fit block via TruncateAt,
// then AppendFileData writes the new file from that position. This is safe
// whenever len(data) <= sp.ByteCount: the write occupies exactly len(data)
// bytes (one tape block), and the drive advances to the next position, leaving
// all subsequent live data blocks intact.
//
// On reuse the selected entry is removed from idx.AvailableSpaces.
func (t *TapeFS) WriteFileData(data []byte, idx *ltfs.Index) (ltfs.Extent, error) {
	if len(idx.AvailableSpaces) == 0 {
		return t.AppendFileData(data)
	}

	if part, ok := t.DataPartition.(*tape.Partition); ok {
		// File-backed tape: any space may be overwritten in-place.
		best := -1
		for i, sp := range idx.AvailableSpaces {
			if sp.Partition != "b" || sp.ByteCount < int64(len(data)) {
				continue
			}
			if best < 0 || idx.AvailableSpaces[best].ByteCount > sp.ByteCount {
				best = i
			}
		}
		if best >= 0 {
			sp := idx.AvailableSpaces[best]
			if err := part.OverwriteBlock(uint64(sp.StartBlock), data); err != nil {
				return ltfs.Extent{}, err
			}
			idx.AvailableSpaces = append(
				idx.AvailableSpaces[:best],
				idx.AvailableSpaces[best+1:]...,
			)
			return ltfs.Extent{
				FileOffset: 0,
				Partition:  "b",
				StartBlock: sp.StartBlock,
				ByteOffset: 0,
				ByteCount:  int64(len(data)),
			}, nil
		}
	} else {
		// SCSI tape: seek to the available space block and write from there.
		// This is safe as long as len(data) <= sp.ByteCount: the new file's
		// single write() call occupies exactly len(data) bytes and the tape
		// drive advances the head one block; the existing filemark that
		// followed the original file on tape is then overwritten by our new
		// filemark, and all live data blocks beyond sp.StartBlock+1 are
		// preserved because we do not advance the head past them.
		//
		// Best-fit: pick the smallest available space whose ByteCount >=
		// len(data) so that the remaining gap is minimised.
		best := -1
		for i, sp := range idx.AvailableSpaces {
			if sp.Partition != "b" || sp.ByteCount < int64(len(data)) {
				continue
			}
			if best < 0 || idx.AvailableSpaces[best].ByteCount > sp.ByteCount {
				best = i
			}
		}
		if best >= 0 {
			sp := idx.AvailableSpaces[best]
			// Remove the entry before writing so the index stays consistent
			// even if the write fails after TruncateAt.
			idx.AvailableSpaces = append(
				idx.AvailableSpaces[:best],
				idx.AvailableSpaces[best+1:]...,
			)
			// Rewind to the freed block; AppendFileData then writes from here.
			if err := t.DataPartition.TruncateAt(uint64(sp.StartBlock)); err != nil {
				return ltfs.Extent{}, fmt.Errorf("seek to available space: %w", err)
			}
			return t.AppendFileData(data)
		}
	}

	return t.AppendFileData(data)
}

// Defrag compacts the data partition by removing gaps left by deleted files.
//
// Algorithm:
//  1. Locate holeStart = minimum StartBlock across all available spaces.
//  2. Collect every live file whose extents lie at or after holeStart; those
//     blocks will be destroyed by the truncation in step 4.
//  3. If total staged bytes > sizeLimit, return an error.
//  4. Read each to-be-moved file's data and write it to stagingDir/<FileUID>.
//  5. Truncate the data partition at holeStart (erasing holes + following data).
//  6. Rewrite each staged file sequentially via AppendFileData and update its
//     extent in idx.
//  7. Clear idx.AvailableSpaces.
//
// The staging directory is removed on success.
func (t *TapeFS) Defrag(idx *ltfs.Index, stagingDir string, sizeLimit int64) error {
	if len(idx.AvailableSpaces) == 0 {
		return nil
	}

	// 1. Find the first hole.
	holeStart := idx.AvailableSpaces[0].StartBlock
	for _, sp := range idx.AvailableSpaces[1:] {
		if sp.StartBlock < holeStart {
			holeStart = sp.StartBlock
		}
	}

	// 2. Collect live files that need to be moved.
	type entry struct {
		file    *ltfs.File
		origMin int64 // minimum StartBlock across all extents (for ordering)
	}
	var toMove []entry
	var totalBytes int64
	for _, f := range ltfs.AllFiles(idx.Root) {
		needsMove := false
		var minBlock int64 = -1
		for _, ext := range f.ExtentInfo.Extents {
			if ext.StartBlock >= holeStart {
				needsMove = true
			}
			if minBlock < 0 || ext.StartBlock < minBlock {
				minBlock = ext.StartBlock
			}
		}
		if needsMove {
			totalBytes += f.Length
			toMove = append(toMove, entry{file: f, origMin: minBlock})
		}
	}

	// 3. Check staging size limit.
	if sizeLimit > 0 && totalBytes > sizeLimit {
		return fmt.Errorf(
			"live data after first hole (%d bytes) exceeds staging limit (%d bytes); "+
				"increase the size limit or free more space",
			totalBytes, sizeLimit,
		)
	}

	if len(toMove) == 0 {
		// All holes are at or past the last live file; just truncate.
		if err := t.DataPartition.TruncateAt(uint64(holeStart)); err != nil {
			return fmt.Errorf("truncate data partition: %w", err)
		}
		idx.AvailableSpaces = nil
		return t.DataPartition.Sync()
	}

	// Sort by original tape position so relative order is preserved.
	sort.Slice(toMove, func(i, j int) bool {
		return toMove[i].origMin < toMove[j].origMin
	})

	// 4. Stage files to disk.
	if err := os.MkdirAll(stagingDir, 0o755); err != nil {
		return fmt.Errorf("create staging dir: %w", err)
	}

	type stageFile struct {
		file *ltfs.File
		path string
	}
	staged := make([]stageFile, 0, len(toMove))
	for _, e := range toMove {
		data, err := t.ReadFileData(e.file)
		if err != nil {
			_ = os.RemoveAll(stagingDir)
			return fmt.Errorf("read %q for staging: %w", e.file.Name, err)
		}
		p := filepath.Join(stagingDir, fmt.Sprintf("%d", e.file.FileUID))
		if err := os.WriteFile(p, data, 0o600); err != nil {
			_ = os.RemoveAll(stagingDir)
			return fmt.Errorf("write staging file: %w", err)
		}
		staged = append(staged, stageFile{file: e.file, path: p})
	}

	// 5. Truncate at the first hole.
	if err := t.DataPartition.TruncateAt(uint64(holeStart)); err != nil {
		_ = os.RemoveAll(stagingDir)
		return fmt.Errorf("truncate data partition: %w", err)
	}

	// 6. Rewrite staged files and update extents.
	for _, sf := range staged {
		data, err := os.ReadFile(sf.path)
		if err != nil {
			return fmt.Errorf("read staged file for %q: %w", sf.file.Name, err)
		}
		extent, err := t.AppendFileData(data)
		if err != nil {
			return fmt.Errorf("rewrite %q: %w", sf.file.Name, err)
		}
		sf.file.ExtentInfo.Extents = []ltfs.Extent{extent}
	}

	// 7. Clear available spaces and clean up staging area.
	idx.AvailableSpaces = nil
	_ = os.RemoveAll(stagingDir)
	return t.DataPartition.Sync()
}

func (t *TapeFS) partitionFor(partition string) tape.Tape {
	switch partition {
	case "a":
		return t.IndexPartition
	case "b":
		return t.DataPartition
	default:
		return nil
	}
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

func min64(a, b int64) int64 {
	if a < b {
		return a
	}
	return b
}

func max64(a, b int64) int64 {
	if a > b {
		return a
	}
	return b
}
