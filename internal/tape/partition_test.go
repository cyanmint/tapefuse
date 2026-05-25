package tape_test

import (
	"os"
	"testing"

	"github.com/cyanmint/tapefuse/internal/tape"
)

func TestWriteReadRoundtrip(t *testing.T) {
	f, err := os.CreateTemp("", "tape-*.dat")
	if err != nil {
		t.Fatal(err)
	}
	path := f.Name()
	f.Close()
	defer os.Remove(path)

	p, err := tape.Create(path)
	if err != nil {
		t.Fatal(err)
	}

	want := []byte("hello world")
	bn, err := p.WriteRecord(tape.Record{Type: tape.RecordData, Data: want})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := p.WriteFilemark(); err != nil {
		t.Fatal(err)
	}
	if err := p.Close(); err != nil {
		t.Fatal(err)
	}

	p2, err := tape.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p2.Close()

	rec, err := p2.ReadAt(bn)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Type != tape.RecordData {
		t.Fatalf("expected RecordData, got %v", rec.Type)
	}
	if string(rec.Data) != string(want) {
		t.Fatalf("data mismatch: got %q want %q", rec.Data, want)
	}
}

func TestTruncateAt(t *testing.T) {
	f, err := os.CreateTemp("", "tape-*.dat")
	if err != nil {
		t.Fatal(err)
	}
	path := f.Name()
	f.Close()
	defer os.Remove(path)

	p, err := tape.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	for i := 0; i < 5; i++ {
		if _, err := p.WriteRecord(tape.Record{Type: tape.RecordData, Data: []byte("block")}); err != nil {
			t.Fatal(err)
		}
	}
	if p.BlockCount() != 5 {
		t.Fatalf("expected 5 blocks, got %d", p.BlockCount())
	}
	if err := p.TruncateAt(3); err != nil {
		t.Fatal(err)
	}
	if p.BlockCount() != 3 {
		t.Fatalf("expected 3 blocks after truncate, got %d", p.BlockCount())
	}
	bn, err := p.WriteRecord(tape.Record{Type: tape.RecordData, Data: []byte("new")})
	if err != nil {
		t.Fatal(err)
	}
	if bn != 3 {
		t.Fatalf("expected block 3, got %d", bn)
	}
}

func TestFilemark(t *testing.T) {
	f, err := os.CreateTemp("", "tape-*.dat")
	if err != nil {
		t.Fatal(err)
	}
	path := f.Name()
	f.Close()
	defer os.Remove(path)

	p, err := tape.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	defer p.Close()

	fmBn, err := p.WriteFilemark()
	if err != nil {
		t.Fatal(err)
	}
	rec, err := p.ReadAt(fmBn)
	if err != nil {
		t.Fatal(err)
	}
	if rec.Type != tape.RecordFilemark {
		t.Fatalf("expected RecordFilemark, got %v", rec.Type)
	}
}
