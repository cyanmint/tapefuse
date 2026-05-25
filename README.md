# tapefuse

`tapefused` is a Go LTFS emulator daemon that mounts a FUSE filesystem backed by two binary files representing LTFS index (`.p0.dat`) and data (`.p1.dat`) tape partitions.

## Features

- Formats a new LTFS-style tape image automatically on first run
- Mounts the image through `bazil.org/fuse`
- Loads LTFS index XML into memory on startup
- Reads existing file extents from the emulated tape data partition
- Writes files by appending data records and updating LTFS metadata
- Flushes the in-memory LTFS index back to the index partition on `SIGUSR1`, unmount, or by writing to `/.tapefuse_flush`

## Build

```bash
go mod tidy
go build ./...
```

## Run

```bash
go run ./cmd/tapefused -device ./tape -mount ./mnt
```

This creates or reuses:

- `./tape.p0.dat` for the LTFS index partition
- `./tape.p1.dat` for the LTFS data partition

## Flush the LTFS index

Send `SIGUSR1` to the daemon, or write any content to `./mnt/.tapefuse_flush`.
