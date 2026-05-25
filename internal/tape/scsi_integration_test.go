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

	testData := []byte("hello tape world")
	rec, err := idxTape.ReadAt(3)
	if err != nil {
		t.Fatalf("ReadAt(3): %v", err)
	}
	if rec.Type != tape.RecordData {
		t.Fatalf("expected data record at block 3, got type %v", rec.Type)
	}
	t.Logf("Index block data length: %d bytes", len(rec.Data))

	bn, err := dataTape.WriteRecord(tape.Record{Type: tape.RecordData, Data: testData})
	if err != nil {
		t.Fatalf("WriteRecord on data tape: %v", err)
	}
	t.Logf("Wrote data at data partition block %d", bn)

	rec2, err := dataTape.ReadAt(bn)
	if err != nil {
		t.Fatalf("ReadAt on data tape: %v", err)
	}
	if string(rec2.Data) != string(testData) {
		t.Fatalf("data mismatch: got %q, want %q", rec2.Data, testData)
	}
	t.Log("MHVTL real tape test passed")
}
