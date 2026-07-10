package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"

	"github.com/cyanmint/tapefuse/internal/daemon"
)

func verbosef(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "ltape: "+format+"\n", args...)
}

// clientCmds are handled locally by the ltape client and are not forwarded to
// the daemon.
var clientCmds = []string{"startdaemon", "killdaemon"}

func main() {
	// When invoked as "ltaped" (e.g. via a symlink) run the daemon directly
	// in the foreground.
	if filepath.Base(os.Args[0]) == "ltaped" {
		runDaemon()
		return
	}

	if len(os.Args) < 2 {
		usage()
	}

	cmd := os.Args[1]
	args := os.Args[2:]

	// Resolve the top-level command via unambiguous prefix matching.  The
	// daemon-lifecycle commands are handled locally; all others are forwarded
	// to ltaped.
	allCmds := append(append([]string{}, clientCmds...), daemon.KnownCmds...)
	resolvedCmd, err := daemon.ResolveCmd(cmd, allCmds)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	cmd = resolvedCmd

	switch cmd {
	case "startdaemon":
		startDaemon()
		return
	case "killdaemon":
		killDaemon()
		return
	}

	// Translate the user-facing arguments into the normalised protocol
	// arguments expected by the daemon.
	protoArgs, err := buildArgs(cmd, args)
	if err != nil {
		fmt.Fprintf(os.Stderr, "%s: %s\n", cmd, err)
		os.Exit(1)
	}

	verbosef("connecting to %s", daemon.SocketPath)
	conn, err := net.Dial("unix", daemon.SocketPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cannot connect to ltaped: %v\n", err)
		os.Exit(1)
	}
	defer conn.Close()

	req := daemon.Request{Cmd: cmd, Args: protoArgs}
	verbosef("sending command %q with args [%s]", cmd, strings.Join(protoArgs, " "))
	enc := json.NewEncoder(conn)
	if err := enc.Encode(req); err != nil {
		fmt.Fprintf(os.Stderr, "send: %v\n", err)
		os.Exit(1)
	}

	scanner := bufio.NewScanner(conn)
	scanner.Buffer(make([]byte, 1024*1024), 16*1024*1024)
	if !scanner.Scan() {
		fmt.Fprintln(os.Stderr, "no response from ltaped")
		os.Exit(1)
	}
	var resp daemon.Response
	if err := json.Unmarshal(scanner.Bytes(), &resp); err != nil {
		fmt.Fprintf(os.Stderr, "bad response: %v\n", err)
		os.Exit(1)
	}
	if !resp.OK {
		fmt.Fprintf(os.Stderr, "error: %s\n", resp.Error)
		os.Exit(1)
	}

	if resp.Output != "" {
		fmt.Print(resp.Output)
	} else {
		fmt.Println("ok")
	}
	verbosef("command %q completed successfully", cmd)
}

// parseAddr splits a "letter:/path" tape address into its letter and path
// components.  The path defaults to "/" when omitted.
func parseAddr(s string) (letter, path string, err error) {
	i := strings.IndexByte(s, ':')
	if i < 0 {
		return "", "", fmt.Errorf("invalid address %q (want letter:/path)", s)
	}
	letter = s[:i]
	path = s[i+1:]
	if letter == "" {
		return "", "", fmt.Errorf("invalid address %q (missing letter)", s)
	}
	if path == "" {
		path = "/"
	}
	return letter, path, nil
}

func abspath(p string) (string, error) {
	return filepath.Abs(p)
}

// buildArgs converts the user-supplied CLI arguments for cmd into the
// normalised argument list understood by the daemon protocol.
func buildArgs(cmd string, args []string) ([]string, error) {
	switch cmd {
	case "assign":
		if len(args) != 2 {
			return nil, fmt.Errorf("usage: ltape assign <device> <letter>")
		}
		return []string{args[0], args[1]}, nil

	case "indexread":
		mode := "file"
		var pos []string
		for _, a := range args {
			switch a {
			case "-m", "--memory":
				mode = "memory"
			case "-f", "--file":
				mode = "file"
			default:
				pos = append(pos, a)
			}
		}
		if len(pos) != 1 {
			return nil, fmt.Errorf("usage: ltape indexread [-m|-f] <letter>")
		}
		return []string{pos[0], mode}, nil

	case "ls":
		if len(args) != 1 {
			return nil, fmt.Errorf("usage: ltape ls <letter>:/path")
		}
		letter, path, err := parseAddr(args[0])
		if err != nil {
			return nil, err
		}
		return []string{letter, path}, nil

	case "rm":
		recursive := false
		var pos []string
		for _, a := range args {
			switch a {
			case "-r", "-R", "--recursive":
				recursive = true
			default:
				pos = append(pos, a)
			}
		}
		if len(pos) != 1 {
			return nil, fmt.Errorf("usage: ltape rm [-r] <letter>:/path")
		}
		letter, path, err := parseAddr(pos[0])
		if err != nil {
			return nil, err
		}
		return []string{letter, path, strconv.FormatBool(recursive)}, nil

	case "get":
		if len(args) < 1 || len(args) > 2 {
			return nil, fmt.Errorf("usage: ltape get <letter>:/path [dest]")
		}
		letter, path, err := parseAddr(args[0])
		if err != nil {
			return nil, err
		}
		dest := "."
		if len(args) == 2 {
			dest = args[1]
		}
		destAbs, err := abspath(dest)
		if err != nil {
			return nil, err
		}
		return []string{letter, path, destAbs}, nil

	case "push":
		if len(args) != 2 {
			return nil, fmt.Errorf("usage: ltape push <local> <letter>:/path")
		}
		srcAbs, err := abspath(args[0])
		if err != nil {
			return nil, err
		}
		letter, path, err := parseAddr(args[1])
		if err != nil {
			return nil, err
		}
		return []string{letter, path, srcAbs}, nil

	case "mount":
		kind := "file"
		var pos []string
		for _, a := range args {
			switch a {
			case "-m", "--memory":
				kind = "memory"
			case "-f", "--file":
				kind = "file"
			default:
				pos = append(pos, a)
			}
		}
		if len(pos) != 2 {
			return nil, fmt.Errorf("usage: ltape mount [-m|-f] <letter> <mountpoint>")
		}
		mp, err := abspath(pos[1])
		if err != nil {
			return nil, err
		}
		return []string{pos[0], mp, kind}, nil

	case "umount", "flush", "commitindex", "discardindex", "defrag":
		if len(args) != 1 {
			return nil, fmt.Errorf("usage: ltape %s <letter>", cmd)
		}
		return []string{args[0]}, nil
	}
	return nil, fmt.Errorf("unknown command %q", cmd)
}

// runDaemon runs the tape daemon in the foreground (used when invoked as
// "ltaped" or forked by startDaemon).
func runDaemon() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	log.Printf("ltaped: starting up")
	if err := os.MkdirAll("/tmp/ltape", 0o755); err != nil {
		log.Fatalf("ltaped: create /tmp/ltape: %v", err)
	}

	// Write PID file so "ltape killdaemon" can find and terminate us.
	pidData := fmt.Sprintf("%d\n", os.Getpid())
	if err := os.WriteFile(daemon.PidPath, []byte(pidData), 0o644); err != nil {
		log.Fatalf("ltaped: write pid file: %v", err)
	}
	defer os.Remove(daemon.PidPath)

	log.Printf("ltaped: removing stale socket %s", daemon.SocketPath)
	_ = os.Remove(daemon.SocketPath)

	log.Printf("ltaped: listening on unix socket %s", daemon.SocketPath)
	ln, err := net.Listen("unix", daemon.SocketPath)
	if err != nil {
		log.Fatalf("ltaped: listen: %v", err)
	}
	defer ln.Close()

	d := daemon.New()
	go d.Serve(ln)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	sig := <-sigCh
	log.Printf("ltaped: received signal %s, shutting down", sig)
}

// startDaemon forks a background daemon process (this binary invoked as
// "ltaped" so the child immediately calls runDaemon).
func startDaemon() {
	// Check if already running.
	if data, err := os.ReadFile(daemon.PidPath); err == nil {
		pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
		if err == nil {
			if proc, err := os.FindProcess(pid); err == nil {
				if proc.Signal(syscall.Signal(0)) == nil {
					fmt.Fprintf(os.Stderr, "ltaped already running (pid %d)\n", pid)
					os.Exit(1)
				}
			}
		}
	}

	self, err := os.Executable()
	if err != nil {
		fmt.Fprintf(os.Stderr, "cannot find executable: %v\n", err)
		os.Exit(1)
	}

	// Relaunch self with the name "ltaped" so runDaemon() is triggered.
	attr := &os.ProcAttr{
		Dir:   "/",
		Env:   os.Environ(),
		Files: []*os.File{nil, nil, nil}, // detach stdin/stdout/stderr
		Sys: &syscall.SysProcAttr{
			Setsid: true, // new session — fully detached from terminal
		},
	}
	proc, err := os.StartProcess(self, []string{"ltaped"}, attr)
	if err != nil {
		fmt.Fprintf(os.Stderr, "failed to start ltaped: %v\n", err)
		os.Exit(1)
	}
	if err := proc.Release(); err != nil {
		fmt.Fprintf(os.Stderr, "release: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("ltaped started (pid %d)\n", proc.Pid)
}

// killDaemon reads the PID file and sends SIGTERM to the daemon.
func killDaemon() {
	data, err := os.ReadFile(daemon.PidPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cannot read pid file %s: %v\n", daemon.PidPath, err)
		os.Exit(1)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		fmt.Fprintf(os.Stderr, "invalid pid in %s: %v\n", daemon.PidPath, err)
		os.Exit(1)
	}
	proc, err := os.FindProcess(pid)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cannot find process %d: %v\n", pid, err)
		os.Exit(1)
	}
	if err := proc.Signal(syscall.SIGTERM); err != nil {
		fmt.Fprintf(os.Stderr, "cannot signal process %d: %v\n", pid, err)
		os.Exit(1)
	}
	fmt.Printf("ltaped (pid %d) signalled to stop\n", pid)
}

func usage() {
	fmt.Fprintln(os.Stderr, `Usage: ltape <command> [args]

Daemon management:
  startdaemon                 start the tape daemon in the background
  killdaemon                  stop the running tape daemon

  (Alternatively: symlink or copy ltape to ltaped; running ltaped starts
   the daemon in the foreground.)

Tape commands:
  assign <device> <letter>        assign a letter to a tape device
  indexread [-m|-f] <letter>      read the index from tape
                                    -m  keep the index in memory
                                    -f  cache the index on disk (default)
  ls <letter>:/path               list a directory (or file) on tape
  rm [-r] <letter>:/path          remove a file, or a directory tree with -r
  get <letter>:/path [dest]       copy a file/dir from tape to local disk
  push <local> <letter>:/path     copy a local file/dir onto tape (overwrite)
  mount [-m|-f] <letter> <dir>    mount the tape as a FUSE filesystem
                                    -m  in-memory temp dir
                                    -f  file-backed temp dir (default)
  umount <letter>                 unmount the tape filesystem
  flush <letter>                  flush the mount temp dir to tape
  commitindex <letter>            write the working index back to tape
  discardindex <letter>           discard the working index
  defrag <letter>                 defragment the tape (currently a no-op stub)`)
	os.Exit(1)
}
