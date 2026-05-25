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
