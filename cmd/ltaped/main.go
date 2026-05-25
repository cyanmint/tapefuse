package main

import (
	"log"
	"net"
	"os"
	"os/signal"
	"syscall"

	"github.com/cyanmint/tapefuse/internal/daemon"
)

func main() {
	log.SetFlags(log.LstdFlags | log.Lmicroseconds)
	log.Printf("ltaped: starting up")
	if err := os.MkdirAll("/tmp/ltape", 0o755); err != nil {
		log.Fatalf("ltaped: create /tmp/ltape: %v", err)
	}
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
