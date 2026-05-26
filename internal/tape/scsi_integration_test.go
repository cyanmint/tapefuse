//go:build linux

package tape_test

import (
	"os"
	"testing"

	"github.com/cyanmint/tapefuse/internal/tape"
)

// TestMHVTL tests real tape I/O against a SCSI tape device.
// Set TAPEFUSE_TEST_DEVICE to the path of the tape device (e.g. /dev/nst0).
// This test is skipped if TAPEFUSE_TEST_DEVICE is not set.
func TestMHVTL(t *testing.T) {
	device := os.Getenv("TAPEFUSE_TEST_DEVICE")
	if device == "" {
		t.Skip("TAPEFUSE_TEST_DEVICE not set, skipping real tape test")
	}

	idxTape, dataTape, err := tape.CreateSCSI(device)
	if err != nil {
		t.Fatalf("CreateSCSI: %v", err)
	}
	defer idxTape.Close()
	defer dataTape.Close()

	// Read the index block from the index partition.  This moves the physical
	// tape head away from the write position; the subsequent WriteRecord must
	// re-position correctly via MTEOM.
	rec, err := idxTape.ReadAt(3)
	if err != nil {
		t.Fatalf("ReadAt(3): %v", err)
	}
	if rec.Type != tape.RecordData {
		t.Fatalf("expected data record at block 3, got type %v", rec.Type)
	}
	t.Logf("Index block data length: %d bytes", len(rec.Data))

	// --- First file write ---
	testData1 := []byte("hello tape world")
	bn1, err := dataTape.WriteRecord(tape.Record{Type: tape.RecordData, Data: testData1})
	if err != nil {
		t.Fatalf("WriteRecord (file 1) on data tape: %v", err)
	}
	t.Logf("Wrote file 1 at data partition block %d", bn1)
	if _, err := dataTape.WriteFilemark(); err != nil {
		t.Fatalf("WriteFilemark (file 1): %v", err)
	}
	if _, err := dataTape.WriteEOD(); err != nil {
		t.Fatalf("WriteEOD (file 1): %v", err)
	}

	// Read back file 1.
	rec1, err := dataTape.ReadAt(bn1)
	if err != nil {
		t.Fatalf("ReadAt (file 1): %v", err)
	}
	if string(rec1.Data) != string(testData1) {
		t.Fatalf("file 1 data mismatch: got %q, want %q", rec1.Data, testData1)
	}
	t.Log("File 1 read back OK")

	// --- Second file write (exercises AppendFileData removing the EOD before
	//     writing, which is the scenario that previously caused EIO) ---
	testData2 := []byte("second file on tape")
	bn2, err := dataTape.WriteRecord(tape.Record{Type: tape.RecordData, Data: testData2})
	if err != nil {
		t.Fatalf("WriteRecord (file 2) on data tape: %v", err)
	}
	t.Logf("Wrote file 2 at data partition block %d", bn2)
	if _, err := dataTape.WriteFilemark(); err != nil {
		t.Fatalf("WriteFilemark (file 2): %v", err)
	}
	if _, err := dataTape.WriteEOD(); err != nil {
		t.Fatalf("WriteEOD (file 2): %v", err)
	}

	// Read back both files.
	rec1again, err := dataTape.ReadAt(bn1)
	if err != nil {
		t.Fatalf("ReadAt (file 1 again): %v", err)
	}
	if string(rec1again.Data) != string(testData1) {
		t.Fatalf("file 1 re-read mismatch: got %q, want %q", rec1again.Data, testData1)
	}
	rec2, err := dataTape.ReadAt(bn2)
	if err != nil {
		t.Fatalf("ReadAt (file 2): %v", err)
	}
	if string(rec2.Data) != string(testData2) {
		t.Fatalf("file 2 data mismatch: got %q, want %q", rec2.Data, testData2)
	}
	t.Log("MHVTL real tape test passed (both files verified)")
}
