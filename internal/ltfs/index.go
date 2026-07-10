package ltfs

import (
	"encoding/xml"
	"errors"
	"fmt"
	"os"
	"path"
	"sort"
	"strings"
	"time"
)

const (
	FormatVersion = "2.4.0"
	CreatorString = "tapefuse 1.0.0 - Linux"
)

type Index struct {
	XMLName           xml.Name   `xml:"ltfsindex"`
	Version           string     `xml:"version,attr"`
	Creator           string     `xml:"creator"`
	VolumeUUID        string     `xml:"volumeuuid"`
	GenerationNumber  uint64     `xml:"generationnumber"`
	UpdateTime        string     `xml:"updatetime"`
	Location          Location   `xml:"location"`
	PrevGenLocation   Location   `xml:"previousgenerationlocation"`
	AllowPolicyUpdate bool       `xml:"allowpolicyupdate"`
	HighestFileUID    int64      `xml:"highestfileuid"`
	Root              *Directory `xml:"directory"`
	// AvailableSpaces lists deleted-file blocks that may be reused.
	// This is a tapefuse-specific extension stored inside the LTFS index XML.
	AvailableSpaces []AvailableSpace `xml:"availablespace"`
}

type Directory struct {
	XMLName      xml.Name `xml:"directory"`
	Name         string   `xml:"name"`
	CreationTime string   `xml:"creationtime"`
	ChangeTime   string   `xml:"changetime"`
	ModifyTime   string   `xml:"modifytime"`
	AccessTime   string   `xml:"accesstime"`
	BackupTime   string   `xml:"backuptime"`
	Contents     Contents `xml:"contents"`
}

type Contents struct {
	Files       []File      `xml:"file"`
	Directories []Directory `xml:"directory"`
}

type File struct {
	XMLName      xml.Name   `xml:"file"`
	Name         string     `xml:"name"`
	Length       int64      `xml:"length"`
	ReadOnly     bool       `xml:"readonly"`
	OpenForWrite bool       `xml:"openforwrite"`
	CreationTime string     `xml:"creationtime"`
	ChangeTime   string     `xml:"changetime"`
	ModifyTime   string     `xml:"modifytime"`
	AccessTime   string     `xml:"accesstime"`
	BackupTime   string     `xml:"backuptime"`
	FileUID      int64      `xml:"fileuid"`
	ExtentInfo   ExtentInfo `xml:"extentinfo"`
	XAttrs       []XAttr    `xml:"extendedattributes>xattr,omitempty"`
}

type ExtentInfo struct {
	Extents []Extent `xml:"extent"`
}

type Extent struct {
	FileOffset int64  `xml:"fileoffset"`
	Partition  string `xml:"partition"`
	StartBlock int64  `xml:"startblock"`
	ByteOffset int64  `xml:"byteoffset"`
	ByteCount  int64  `xml:"bytecount"`
}

type Location struct {
	Partition  string `xml:"partition"`
	StartBlock uint64 `xml:"startblock,omitempty"`
}

type XAttr struct {
	Key   string `xml:"key"`
	Value string `xml:"value"`
}

// AvailableSpace records a data-partition block whose file has been deleted and
// whose physical storage can be reused for a new file of equal or smaller size.
// This is a tapefuse-specific extension; standard LTFS clients ignore it.
type AvailableSpace struct {
	Partition  string `xml:"partition"`
	StartBlock int64  `xml:"startblock"`
	// ByteCount is the physical data capacity of the block (the original
	// file's byte length when it was first written to this block).
	ByteCount int64 `xml:"bytecount"`
}

func Now() string {
	return time.Now().UTC().Format(time.RFC3339Nano)
}

func ParseTime(value string) time.Time {
	if value == "" {
		return time.Unix(0, 0).UTC()
	}
	t, err := time.Parse(time.RFC3339Nano, value)
	if err != nil {
		return time.Unix(0, 0).UTC()
	}
	return t.UTC()
}

func NewDirectory(name string) Directory {
	now := Now()
	return Directory{
		Name:         name,
		CreationTime: now,
		ChangeTime:   now,
		ModifyTime:   now,
		AccessTime:   now,
		BackupTime:   now,
	}
}

func NewFile(name string, uid int64) File {
	now := Now()
	return File{
		Name:         name,
		Length:       0,
		ReadOnly:     false,
		OpenForWrite: false,
		CreationTime: now,
		ChangeTime:   now,
		ModifyTime:   now,
		AccessTime:   now,
		BackupTime:   now,
		FileUID:      uid,
		ExtentInfo:   ExtentInfo{Extents: nil},
	}
}

func NewEmptyIndex(volumeUUID string) *Index {
	now := Now()
	return &Index{
		Version:           FormatVersion,
		Creator:           CreatorString,
		VolumeUUID:        volumeUUID,
		GenerationNumber:  1,
		UpdateTime:        now,
		Location:          Location{Partition: "a", StartBlock: 3},
		PrevGenLocation:   Location{Partition: "a", StartBlock: 0},
		AllowPolicyUpdate: false,
		HighestFileUID:    0,
		Root: &Directory{
			Name:         "/",
			CreationTime: now,
			ChangeTime:   now,
			ModifyTime:   now,
			AccessTime:   now,
			BackupTime:   now,
		},
	}
}

func ParseIndex(data []byte) (*Index, error) {
	var idx Index
	if err := xml.Unmarshal(data, &idx); err != nil {
		return nil, err
	}
	if idx.Root == nil {
		return nil, errors.New("ltfs index missing root directory")
	}
	if idx.Root.Name == "" {
		idx.Root.Name = "/"
	}
	return &idx, nil
}

func (i *Index) Marshal() ([]byte, error) {
	if i == nil || i.Root == nil {
		return nil, errors.New("ltfs index missing root directory")
	}
	i.XMLName = xml.Name{Local: "ltfsindex"}
	i.Version = FormatVersion
	if i.Creator == "" {
		i.Creator = CreatorString
	}
	body, err := xml.MarshalIndent(i, "", "  ")
	if err != nil {
		return nil, err
	}
	out := append([]byte(xml.Header), body...)
	out = append(out, '\n')
	return out, nil
}

func CleanPath(p string) string {
	if p == "" {
		return "/"
	}
	cleaned := path.Clean("/" + p)
	if cleaned == "." {
		return "/"
	}
	return cleaned
}

func pathParts(p string) []string {
	cleaned := CleanPath(p)
	if cleaned == "/" {
		return nil
	}
	return strings.Split(strings.TrimPrefix(cleaned, "/"), "/")
}

func (d *Directory) FindDirectory(name string) *Directory {
	for i := range d.Contents.Directories {
		if d.Contents.Directories[i].Name == name {
			return &d.Contents.Directories[i]
		}
	}
	return nil
}

func (d *Directory) FindFile(name string) *File {
	for i := range d.Contents.Files {
		if d.Contents.Files[i].Name == name {
			return &d.Contents.Files[i]
		}
	}
	return nil
}

func (i *Index) FindDirectory(p string) *Directory {
	if i == nil || i.Root == nil {
		return nil
	}
	dir := i.Root
	for _, part := range pathParts(p) {
		dir = dir.FindDirectory(part)
		if dir == nil {
			return nil
		}
	}
	return dir
}

func (i *Index) ParentDirectory(p string) (*Directory, string, error) {
	cleaned := CleanPath(p)
	if cleaned == "/" {
		return nil, "", errors.New("root has no parent")
	}
	parentPath, base := path.Split(cleaned)
	parent := i.FindDirectory(parentPath)
	if parent == nil {
		return nil, "", fmt.Errorf("directory not found: %s", parentPath)
	}
	return parent, strings.TrimSuffix(base, "/"), nil
}

func (i *Index) FindFile(p string) (*File, *Directory, error) {
	parent, name, err := i.ParentDirectory(p)
	if err != nil {
		return nil, nil, err
	}
	file := parent.FindFile(name)
	if file == nil {
		return nil, parent, fmt.Errorf("file not found: %s", CleanPath(p))
	}
	return file, parent, nil
}

func (i *Index) NextFileUID() int64 {
	i.HighestFileUID++
	return i.HighestFileUID
}

// MkdirAll ensures that every directory component of p exists, creating any
// missing directories, and returns the deepest directory.  It fails if any
// component along the path already exists as a file.
func (i *Index) MkdirAll(p string) (*Directory, error) {
	if i == nil || i.Root == nil {
		return nil, errors.New("ltfs index missing root directory")
	}
	dir := i.Root
	now := Now()
	for _, part := range pathParts(p) {
		if dir.FindFile(part) != nil {
			return nil, fmt.Errorf("%s is a file, not a directory", part)
		}
		child := dir.FindDirectory(part)
		if child == nil {
			dir.Contents.Directories = append(dir.Contents.Directories, NewDirectory(part))
			child = &dir.Contents.Directories[len(dir.Contents.Directories)-1]
			TouchDirectory(dir, now)
		}
		dir = child
	}
	return dir, nil
}

func TouchDirectory(dir *Directory, ts string) {
	dir.ChangeTime = ts
	dir.ModifyTime = ts
	dir.AccessTime = ts
}

func TouchFile(file *File, ts string) {
	file.ChangeTime = ts
	file.ModifyTime = ts
	file.AccessTime = ts
}

// AllFiles returns pointers to all File entries reachable from dir.
// The pointers remain valid as long as no elements are added to or removed
// from any directory's Contents.Files slice in the tree.
func AllFiles(dir *Directory) []*File {
	var out []*File
	for i := range dir.Contents.Files {
		out = append(out, &dir.Contents.Files[i])
	}
	for i := range dir.Contents.Directories {
		out = append(out, AllFiles(&dir.Contents.Directories[i])...)
	}
	return out
}

// MergeAvailableSpaces sorts the available-space pool by partition and start
// block and merges entries that are physically contiguous on tape so that a
// single large hole can satisfy a large write.  Two entries are considered
// contiguous when the next block starts within the block span occupied by the
// previous entry; each freed file occupies its data record plus a trailing
// filemark (two blocks), so a gap of up to two blocks is tolerated.  Merged
// entries keep the lowest start block and the sum of the freed capacities.
func MergeAvailableSpaces(spaces []AvailableSpace) []AvailableSpace {
	if len(spaces) < 2 {
		return spaces
	}
	// Copy so callers that reuse the input slice are not surprised by the
	// in-place sort below.
	sorted := make([]AvailableSpace, len(spaces))
	copy(sorted, spaces)
	sort.Slice(sorted, func(i, j int) bool {
		if sorted[i].Partition != sorted[j].Partition {
			return sorted[i].Partition < sorted[j].Partition
		}
		return sorted[i].StartBlock < sorted[j].StartBlock
	})

	// blockSpan is the number of tape blocks a freed file occupies (one data
	// record plus one filemark).
	const blockSpan = 2

	out := make([]AvailableSpace, 0, len(sorted))
	var runEnd int64 // first block after the current merge run
	for _, sp := range sorted {
		if n := len(out); n > 0 {
			last := &out[n-1]
			if last.Partition == sp.Partition && sp.StartBlock <= runEnd {
				if sp.StartBlock != last.StartBlock {
					// Distinct adjacent block: accumulate its capacity.
					last.ByteCount += sp.ByteCount
				} else if sp.ByteCount > last.ByteCount {
					// Duplicate free of the same block: keep the larger cap.
					last.ByteCount = sp.ByteCount
				}
				if end := sp.StartBlock + blockSpan; end > runEnd {
					runEnd = end
				}
				continue
			}
		}
		out = append(out, sp)
		runEnd = sp.StartBlock + blockSpan
	}
	return out
}

// FreeFileExtents returns the data-partition (partition "b") extents of file to
// the index's available-space pool and merges contiguous entries.  physCap, if
// non-nil, returns the true physical block capacity for a given start block so
// that reused slots report their real size; when it returns <= 0 (or is nil)
// the extent's logical byte count is used instead.
func (i *Index) FreeFileExtents(file *File, physCap func(startBlock int64) int64) {
	for _, ext := range file.ExtentInfo.Extents {
		if ext.Partition != "b" || ext.ByteCount <= 0 {
			continue
		}
		capacity := ext.ByteCount
		if physCap != nil {
			if c := physCap(ext.StartBlock); c > 0 {
				capacity = c
			}
		}
		i.AvailableSpaces = append(i.AvailableSpaces, AvailableSpace{
			Partition:  ext.Partition,
			StartBlock: ext.StartBlock,
			ByteCount:  capacity,
		})
	}
	i.AvailableSpaces = MergeAvailableSpaces(i.AvailableSpaces)
}

// LoadIndexFromFile reads an LTFS index from a disk cache file.
func LoadIndexFromFile(path string) (*Index, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return ParseIndex(data)
}

// SaveIndexToFile writes an LTFS index to a disk cache file atomically.
func SaveIndexToFile(path string, idx *Index) error {
	data, err := idx.Marshal()
	if err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}
