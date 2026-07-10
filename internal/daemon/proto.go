package daemon

const (
	SocketPath = "/tmp/ltape/ltaped.sock"
	IndexDir   = "/tmp/ltape/index"
	IndexExt   = ".index"
	PidPath    = "/tmp/ltape/ltaped.pid"
)

type Request struct {
	Cmd  string   `json:"cmd"`
	Args []string `json:"args"`
}

type Response struct {
	OK    bool   `json:"ok"`
	Error string `json:"error,omitempty"`
	// Output holds human-readable text (e.g. an "ls" listing) for the client
	// to print verbatim.
	Output string `json:"output,omitempty"`
}

// IndexPath returns the disk cache path for a letter.
func IndexPath(letter string) string {
	return IndexDir + "/" + letter + IndexExt
}
