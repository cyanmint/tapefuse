package daemon

const (
	SocketPath = "/tmp/ltape/ltaped.sock"
	IndexDir   = "/tmp/ltape/index"
	IndexExt   = ".index"
)

type Request struct {
	Cmd  string   `json:"cmd"`
	Args []string `json:"args"`
}

type Response struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
}

// IndexPath returns the disk cache path for a letter.
func IndexPath(letter string) string {
	return IndexDir + "/" + letter + IndexExt
}
