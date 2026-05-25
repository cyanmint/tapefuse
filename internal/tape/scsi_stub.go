//go:build !linux

package tape

import "fmt"

func OpenSCSI(device string) (Tape, Tape, error) {
	return nil, nil, fmt.Errorf("SCSI tape devices are not supported on this platform: %s", device)
}

func CreateSCSI(device string) (Tape, Tape, error) {
	return nil, nil, fmt.Errorf("SCSI tape devices are not supported on this platform: %s", device)
}

func SwallowSCSI(device string) error {
	return fmt.Errorf("SCSI tape devices are not supported on this platform: %s", device)
}
