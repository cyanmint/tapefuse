package main

import (
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"bazil.org/fuse"
	bazilfs "bazil.org/fuse/fs"

	tapefs "github.com/cyanmint/tapefuse/fs"
)

func main() {
	var device string
	var mountPoint string
	var debug bool

	flag.StringVar(&device, "device", "tape", "Path to tape device/image")
	flag.StringVar(&mountPoint, "mount", "/mnt/ltfs", "Mount point")
	flag.BoolVar(&debug, "debug", false, "Enable FUSE debug logging")
	flag.Parse()

	if err := os.MkdirAll(mountPoint, 0o755); err != nil {
		log.Fatalf("create mount point: %v", err)
	}

	if debug {
		fuse.Debug = func(msg interface{}) {
			log.Printf("fuse: %v", msg)
		}
	}

	tapeFS, err := tapefs.OpenTape(device)
	if err != nil {
		log.Fatalf("open tape: %v", err)
	}

	filesystem := tapefs.New(tapeFS)
	conn, err := fuse.Mount(mountPoint, fuse.FSName("tapefuse"), fuse.Subtype("ltfs"))
	if err != nil {
		_ = filesystem.Close()
		log.Fatalf("mount fuse: %v", err)
	}
	defer conn.Close()

	serveErr := make(chan error, 1)
	go func() {
		serveErr <- bazilfs.Serve(conn, filesystem)
	}()

	log.Printf("mounted %s at %s", device, mountPoint)

	sigCh := make(chan os.Signal, 2)
	signal.Notify(sigCh, syscall.SIGINT, syscall.SIGTERM, syscall.SIGUSR1)
	defer signal.Stop(sigCh)

	for {
		select {
		case err := <-serveErr:
			if err != nil {
				log.Printf("fuse serve stopped: %v", err)
			}
			if flushErr := filesystem.FlushIndex(); flushErr != nil {
				log.Printf("flush on shutdown failed: %v", flushErr)
			}
			if closeErr := filesystem.Close(); closeErr != nil {
				log.Printf("close failed: %v", closeErr)
			}
			return
		case sig := <-sigCh:
			switch sig {
			case syscall.SIGUSR1:
				if err := filesystem.FlushIndex(); err != nil {
					log.Printf("flush failed: %v", err)
				} else {
					log.Printf("ltfs index flushed")
				}
			default:
				if err := filesystem.FlushIndex(); err != nil {
					log.Printf("flush before unmount failed: %v", err)
				}
				if err := fuse.Unmount(mountPoint); err != nil {
					log.Printf("unmount failed: %v", err)
				}
				if err := <-serveErr; err != nil {
					log.Printf("fuse serve stopped: %v", err)
				}
				if err := filesystem.Close(); err != nil {
					log.Printf("close failed: %v", err)
				}
				return
			}
		}
	}
}
