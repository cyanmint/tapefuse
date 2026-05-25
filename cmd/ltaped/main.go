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
	if err := os.MkdirAll("/tmp/ltape", 0o755); err != nil {
		log.Fatalf("create /tmp/ltape: %v", err)
	}
	_ = os.Remove(daemon.SocketPath)

	ln, err := net.Listen("unix", daemon.SocketPath)
	if err != nil {
		log.Fatalf("listen: %v", err)
	}
	defer ln.Close()

	d := daemon.New()
	log.Printf("ltaped listening on %s", daemon.SocketPath)

	go d.Serve(ln)

	sigCh := make(chan os.Signal, 1)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM)
	<-sigCh
	log.Printf("ltaped shutting down")
}
