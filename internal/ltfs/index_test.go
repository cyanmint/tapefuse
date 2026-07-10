package ltfs_test

import (
	"os"
	"testing"

	"github.com/cyanmint/tapefuse/internal/ltfs"
)

func TestIndexMarshalUnmarshal(t *testing.T) {
	idx := ltfs.NewEmptyIndex("test-uuid-1234")
	data, err := idx.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	idx2, err := ltfs.ParseIndex(data)
	if err != nil {
		t.Fatalf("parse: %v (data: %s)", err, data)
	}
	if idx2.VolumeUUID != idx.VolumeUUID {
		t.Fatalf("uuid mismatch: %s vs %s", idx2.VolumeUUID, idx.VolumeUUID)
	}
	if idx2.Root == nil {
		t.Fatal("root directory is nil")
	}
	if idx2.Root.Name != "/" {
		t.Fatalf("root name: %q", idx2.Root.Name)
	}
}

func TestIndexFileHelpers(t *testing.T) {
	f, err := os.CreateTemp("", "index-*.xml")
	if err != nil {
		t.Fatal(err)
	}
	path := f.Name()
	f.Close()
	defer os.Remove(path)

	idx := ltfs.NewEmptyIndex("vol-abc")
	if err := ltfs.SaveIndexToFile(path, idx); err != nil {
		t.Fatal(err)
	}
	idx2, err := ltfs.LoadIndexFromFile(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if idx2.VolumeUUID != "vol-abc" {
		t.Fatalf("uuid: %s", idx2.VolumeUUID)
	}
}

func TestDirectoryOperations(t *testing.T) {
	idx := ltfs.NewEmptyIndex("test-uuid")
	root := idx.Root

	file := ltfs.NewFile("test.txt", 1)
	root.Contents.Files = append(root.Contents.Files, file)

	found := root.FindFile("test.txt")
	if found == nil {
		t.Fatal("FindFile: not found")
	}
	if found.Name != "test.txt" {
		t.Fatalf("FindFile: name %q", found.Name)
	}

	sub := ltfs.NewDirectory("subdir")
	root.Contents.Directories = append(root.Contents.Directories, sub)

	found2 := root.FindDirectory("subdir")
	if found2 == nil {
		t.Fatal("FindDirectory: not found")
	}

	dir := idx.FindDirectory("/")
	if dir == nil {
		t.Fatal("FindDirectory(/): nil")
	}
}

func TestNextFileUID(t *testing.T) {
	idx := ltfs.NewEmptyIndex("uid-test")
	uid1 := idx.NextFileUID()
	uid2 := idx.NextFileUID()
	if uid1 != 1 {
		t.Fatalf("expected uid=1, got %d", uid1)
	}
	if uid2 != 2 {
		t.Fatalf("expected uid=2, got %d", uid2)
	}
	if idx.HighestFileUID != 2 {
		t.Fatalf("HighestFileUID: %d", idx.HighestFileUID)
	}
}

func TestMergeAvailableSpaces(t *testing.T) {
	// Two adjacent freed files (data + filemark = 2 blocks apart) merge into
	// one entry; a distant one stays separate; duplicates keep the larger cap.
	in := []ltfs.AvailableSpace{
		{Partition: "b", StartBlock: 6, ByteCount: 200},
		{Partition: "b", StartBlock: 4, ByteCount: 100},
		{Partition: "b", StartBlock: 20, ByteCount: 50},
		{Partition: "b", StartBlock: 4, ByteCount: 80}, // duplicate of block 4
	}
	out := ltfs.MergeAvailableSpaces(in)
	if len(out) != 2 {
		t.Fatalf("expected 2 merged spaces, got %d: %+v", len(out), out)
	}
	if out[0].StartBlock != 4 || out[0].ByteCount != 300 {
		t.Errorf("first merged space = %+v, want {b 4 300}", out[0])
	}
	if out[1].StartBlock != 20 || out[1].ByteCount != 50 {
		t.Errorf("second space = %+v, want {b 20 50}", out[1])
	}
}

func TestFreeFileExtents(t *testing.T) {
	idx := ltfs.NewEmptyIndex("free-test")
	f := ltfs.NewFile("a.bin", 1)
	f.Length = 30
	f.ExtentInfo.Extents = []ltfs.Extent{
		{Partition: "b", StartBlock: 4, ByteOffset: 0, ByteCount: 30},
	}
	idx.FreeFileExtents(&f, nil)
	if len(idx.AvailableSpaces) != 1 {
		t.Fatalf("expected 1 available space, got %d", len(idx.AvailableSpaces))
	}
	if idx.AvailableSpaces[0].StartBlock != 4 || idx.AvailableSpaces[0].ByteCount != 30 {
		t.Errorf("available space = %+v", idx.AvailableSpaces[0])
	}
}

func TestMkdirAll(t *testing.T) {
	idx := ltfs.NewEmptyIndex("mkdir-test")
	dir, err := idx.MkdirAll("/a/b/c")
	if err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}
	if dir.Name != "c" {
		t.Fatalf("deepest dir name = %q, want c", dir.Name)
	}
	if idx.FindDirectory("/a/b/c") == nil {
		t.Fatal("FindDirectory(/a/b/c) returned nil")
	}
	// Idempotent: creating again returns the same tree.
	if _, err := idx.MkdirAll("/a/b/c"); err != nil {
		t.Fatalf("MkdirAll (second call): %v", err)
	}
	// A file blocks directory creation through it.
	f := ltfs.NewFile("file", 1)
	idx.Root.Contents.Files = append(idx.Root.Contents.Files, f)
	if _, err := idx.MkdirAll("/file/sub"); err == nil {
		t.Fatal("expected error creating dir under a file")
	}
}
