package main

import (
	"bufio"
	"encoding/json"
	"fmt"
	"net"
	"os"

	"github.com/cyanmint/tapefuse/internal/daemon"
)

func main() {
	if len(os.Args) < 2 {
		usage()
	}

	cmd := os.Args[1]
	args := os.Args[2:]

	conn, err := net.Dial("unix", daemon.SocketPath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "cannot connect to ltaped: %v\n", err)
		os.Exit(1)
	}
	defer conn.Close()

	req := daemon.Request{Cmd: cmd, Args: args}
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
	fmt.Println("ok")
}

func usage() {
	fmt.Fprintln(os.Stderr, `Usage: ltape <command> [args]

Commands:
  assign <device> <letter>    assign letter to tape device
  unassign <letter>           free letter
  init <letter>               initialize tape
  load <letter>               read index from tape to disk cache
  commit <letter>             write index from disk cache to tape
  discard <letter>            discard disk cache index
  mount <letter> <mountpoint> mount tape filesystem
  umount <letter>             unmount tape filesystem
  eject <letter>              eject tape`)
	os.Exit(1)
}
