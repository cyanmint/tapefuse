package daemon

import (
	"fmt"
	"sort"
	"strings"
)

// KnownCmds lists every tape command handled by the daemon.  It is exported
// so that the ltape client binary can build the full top-level command list
// (which also includes "daemon") for prefix resolution.
var KnownCmds = []string{
	"assign", "unassign",
	"init", "load", "commit", "discard",
	"mount", "umount",
	"eject", "swallow",
	"list",
	"defrag",
}

// ResolveCmd returns the canonical command name for input using unambiguous
// prefix matching against known.  An exact match always wins.  If the prefix
// matches exactly one candidate it is returned.  If it matches more than one,
// an error is returned listing every match so the caller can report it.
func ResolveCmd(input string, known []string) (string, error) {
	// Exact match takes priority over prefix matching.
	for _, cmd := range known {
		if cmd == input {
			return cmd, nil
		}
	}
	var matches []string
	for _, cmd := range known {
		if strings.HasPrefix(cmd, input) {
			matches = append(matches, cmd)
		}
	}
	switch len(matches) {
	case 0:
		return "", fmt.Errorf("unknown command %q", input)
	case 1:
		return matches[0], nil
	default:
		sort.Strings(matches)
		return "", fmt.Errorf("ambiguous: %q matches %s",
			input, strings.Join(matches, ", "))
	}
}
