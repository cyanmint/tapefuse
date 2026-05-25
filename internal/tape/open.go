package tape

import "os"

// IsCharDevice returns true if path is a character device (e.g. /dev/nst0).
func IsCharDevice(path string) (bool, error) {
	fi, err := os.Stat(path)
	if err != nil {
		return false, err
	}
	return fi.Mode()&os.ModeCharDevice != 0, nil
}
