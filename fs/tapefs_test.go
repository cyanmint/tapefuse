package fs_test

import (
	"os"
	"testing"

	tapefs "github.com/cyanmint/tapefuse/fs"
	"github.com/cyanmint/tapefuse/internal/ltfs"
)

func TestMultiFileAppend(t *testing.T) {
dir, err := os.MkdirTemp("", "tapefuse-multi-*")
if err != nil {
t.Fatal(err)
}
defer os.RemoveAll(dir)

device := dir + "/tape"

tfs, err := tapefs.FormatTape(device)
if err != nil {
t.Fatalf("FormatTape: %v", err)
}

idx, err := tfs.ReadIndexFromTape()
if err != nil {
t.Fatalf("ReadIndexFromTape: %v", err)
}

origFiles := []struct {
name string
data []byte
}{
{"file1.txt", []byte("Hello, World! This is file one.")},
{"file2.txt", []byte("Second file content here.")},
{"file3.txt", []byte("Third file with more data.")},
}

for _, f := range origFiles {
uid := idx.NextFileUID()
file := ltfs.NewFile(f.name, uid)
idx.Root.Contents.Files = append(idx.Root.Contents.Files, file)

i := len(idx.Root.Contents.Files) - 1
filePtr := &idx.Root.Contents.Files[i]

extent, err := tfs.AppendFileData(f.data)
if err != nil {
t.Fatalf("AppendFileData(%s): %v", f.name, err)
}
filePtr.Length = int64(len(f.data))
filePtr.ExtentInfo.Extents = []ltfs.Extent{extent}
t.Logf("Wrote %s: length=%d, startBlock=%d", f.name, filePtr.Length, extent.StartBlock)
}

for _, orig := range origFiles {
var found *ltfs.File
for i := range idx.Root.Contents.Files {
if idx.Root.Contents.Files[i].Name == orig.name {
found = &idx.Root.Contents.Files[i]
break
}
}
if found == nil {
t.Errorf("file %s not found in index", orig.name)
continue
}
if found.Length != int64(len(orig.data)) {
t.Errorf("%s: length=%d, want=%d", orig.name, found.Length, len(orig.data))
}
data, err := tfs.ReadFileData(found)
if err != nil {
t.Errorf("ReadFileData(%s): %v", orig.name, err)
continue
}
if string(data) != string(orig.data) {
t.Errorf("%s: data mismatch:\n got %q\nwant %q", orig.name, data, orig.data)
} else {
t.Logf("OK: %s read back correctly (%d bytes)", orig.name, len(data))
}
}
}

// TestStreamingWriteMultipleExtents verifies that streaming-mode appends
// produce multiple extents on tape (one per write chunk) and that all data
// can be read back correctly after the extents are committed to the index.
func TestStreamingWriteMultipleExtents(t *testing.T) {
dir, err := os.MkdirTemp("", "tapefuse-stream-*")
if err != nil {
t.Fatal(err)
}
defer os.RemoveAll(dir)

device := dir + "/tape"
tfs, err := tapefs.FormatTape(device)
if err != nil {
t.Fatalf("FormatTape: %v", err)
}

idx, err := tfs.ReadIndexFromTape()
if err != nil {
t.Fatalf("ReadIndexFromTape: %v", err)
}

// Create a file entry in the index.
uid := idx.NextFileUID()
file := ltfs.NewFile("stream.bin", uid)
idx.Root.Contents.Files = append(idx.Root.Contents.Files, file)
filePtr := &idx.Root.Contents.Files[len(idx.Root.Contents.Files)-1]

// Simulate streaming writes: three separate chunks appended to tape.
chunks := [][]byte{
[]byte("chunk-one:"),
[]byte("chunk-two:"),
[]byte("chunk-three"),
}
var offset int64
for _, chunk := range chunks {
extent, err := tfs.AppendFileData(chunk)
if err != nil {
	t.Fatalf("AppendFileData: %v", err)
}
extent.FileOffset = offset
filePtr.ExtentInfo.Extents = append(filePtr.ExtentInfo.Extents, extent)
offset += int64(len(chunk))
}
filePtr.Length = offset
t.Logf("wrote %d chunks, total length %d, %d extents", len(chunks), filePtr.Length, len(filePtr.ExtentInfo.Extents))

// Verify all extents are distinct blocks.
if len(filePtr.ExtentInfo.Extents) != len(chunks) {
t.Fatalf("got %d extents, want %d", len(filePtr.ExtentInfo.Extents), len(chunks))
}

// Read back the full file via ReadFileData.
data, err := tfs.ReadFileData(filePtr)
if err != nil {
t.Fatalf("ReadFileData: %v", err)
}
want := "chunk-one:chunk-two:chunk-three"
if string(data) != want {
t.Errorf("data mismatch:\n got %q\nwant %q", string(data), want)
} else {
t.Logf("OK: read back %d bytes correctly", len(data))
}
}
