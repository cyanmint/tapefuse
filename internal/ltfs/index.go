package ltfs

import (
	"encoding/xml"
	"errors"
	"fmt"
	"path"
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
