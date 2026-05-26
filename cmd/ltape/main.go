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

	// Resolve the top-level command via unambiguous prefix matching.
	// "daemon" is handled locally; all other commands are forwarded to ltaped.
	allCmds := append([]string{"daemon"}, daemon.KnownCmds...)
	resolvedCmd, err := daemon.ResolveCmd(cmd, allCmds)
	if err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
	cmd = resolvedCmd

	// Handle daemon lifecycle commands locally — they do not talk to the
	// running daemon process.
	if cmd == "daemon" {
		if len(args) < 1 {
			fmt.Fprintln(os.Stderr, "usage: ltape daemon <on|off>")
			os.Exit(1)
		}
		sub, err := daemon.ResolveCmd(args[0], []string{"on", "off"})
		if err != nil {
			fmt.Fprintf(os.Stderr, "daemon: %s\n", err)
			os.Exit(1)
		}
		switch sub {
		case "on":
			daemonOn()
		case "off":
			daemonOff()
		}
		return
	}

	verbosef("connecting to %s", daemon.SocketPath)
	conn, err := net.Dial("unix", daemon.SocketPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cannot connect to ltaped: %v\n", err)
		os.Exit(1)
	}
	defer conn.Close()

	req := daemon.Request{Cmd: cmd, Args: args}
	verbosef("sending command %q with args [%s]", cmd, strings.Join(args, " "))
	enc := json.NewEncoder(conn)
	if err := enc.Encode(req); err != nil {
		fmt.Fprintf(os.Stderr, "send: %v\n", err)
		os.Exit(1)
	}

	scanner := bufio.NewScanner(conn)
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

	if cmd == "list" {
		verbosef("received %d tape assignment(s)", len(resp.Entries))
		if len(resp.Entries) == 0 {
			fmt.Println("no tapes assigned")
			return
		}
		for _, e := range resp.Entries {
			status := "not loaded"
			if e.Loaded {
				status = "loaded"
			}
			mount := "not mounted"
			if e.MountPoint != "" {
				mount = "mounted at " + e.MountPoint
			}
			fmt.Printf("%s: %s  [%s, %s]\n", e.Letter, e.Device, status, mount)
		}
		return
	}

	verbosef("command %q completed successfully", cmd)
	fmt.Println("ok")
}

// runDaemon runs the tape daemon in the foreground (used when invoked as
// "ltaped" or forked by daemonOn).
func runDaemon() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	log.Printf("ltaped: starting up")
	if err := os.MkdirAll("/tmp/ltape", 0o755); err != nil {
		log.Fatalf("ltaped: create /tmp/ltape: %v", err)
	}

	// Write PID file so "ltape daemon off" can find and terminate us.
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

// daemonOn forks a background daemon process (this binary with LTAPE_DAEMON=1
// set so the child immediately calls runDaemon).
func daemonOn() {
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

// daemonOff reads the PID file and sends SIGTERM to the daemon.
func daemonOff() {
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
  daemon on               start the tape daemon in the background
  daemon off              stop the running tape daemon

  (Alternatively: symlink or copy ltape to ltaped; running ltaped starts
   the daemon in the foreground.)

Tape commands:
  assign <device> <letter>    assign letter to tape device
  unassign <letter>           free letter
  init <letter>               initialize tape
  load <letter>               read index from tape to disk cache
  commit <letter>             write index from disk cache to tape
  discard <letter>            discard disk cache index
  mount <letter> <mountpoint> mount tape filesystem
  umount <letter>             unmount tape filesystem
  eject <letter>              eject tape (assignment is kept; use swallow to reload)
  swallow <letter>            load previously ejected tape
  list                        list assigned tapes and their status
  defrag <letter> [size]      compact tape by removing deleted-file gaps
                              (default size limit for staging area: 10G)`)
	os.Exit(1)
}
