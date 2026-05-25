package tape

// Tape abstracts both file-based and real SCSI tape backends.
type Tape interface {
	ReadAt(blockNum uint64) (*Record, error)
	WriteRecord(rec Record) (uint64, error)
	WriteFilemark() (uint64, error)
	WriteEOD() (uint64, error)
	TruncateAt(blockNum uint64) error
	BlockCount() uint64
	LastRecordType() (RecordType, bool, error)
	Sync() error
	Close() error
	// Eject physically ejects the tape (no-op for file-based backends).
	Eject() error
	// Swallow loads previously ejected media (no-op for file-based backends).
	Swallow() error
}
