package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// setupTape assigns a fresh file-backed tape to letter "a" and reads its index
// into the given mode ("file" or "memory").
func setupTape(t *testing.T, mode string) (*Daemon, string) {
	t.Helper()
	dir := t.TempDir()
	// Keep the on-disk index cache inside the test temp dir would require
	// overriding IndexDir; instead just ensure a clean cache path for "a".
	_ = os.Remove(IndexPath("a"))

	d := New()
	dev := filepath.Join(dir, "tape")
	if r := d.cmdAssign(dev, "a"); !r.OK {
		t.Fatalf("assign: %s", r.Error)
	}
	if r := d.cmdIndexRead("a", mode); !r.OK {
		t.Fatalf("indexread: %s", r.Error)
	}
	t.Cleanup(func() { _ = os.Remove(IndexPath("a")) })
	return d, dir
}

func runPushGetRm(t *testing.T, d *Daemon, dir string) {
	t.Helper()

	// push a local file into a subdirectory on tape.
	src := filepath.Join(dir, "hello.txt")
	content := []byte("hello tape world")
	if err := os.WriteFile(src, content, 0o644); err != nil {
		t.Fatal(err)
	}
	if r := d.cmdPush("a", "/docs/hello.txt", src); !r.OK {
		t.Fatalf("push: %s", r.Error)
	}

	// ls should list the file with its size.
	r := d.cmdLs("a", "/docs")
	if !r.OK {
		t.Fatalf("ls: %s", r.Error)
	}
	if !strings.Contains(r.Output, "hello.txt") {
		t.Fatalf("ls output missing hello.txt: %q", r.Output)
	}

	// get the file back to a new location and compare bytes.
	dest := filepath.Join(dir, "out.txt")
	if r := d.cmdGet("a", "/docs/hello.txt", dest); !r.OK {
		t.Fatalf("get: %s", r.Error)
	}
	got, err := os.ReadFile(dest)
	if err != nil {
		t.Fatal(err)
	}
	if string(got) != string(content) {
		t.Fatalf("get content mismatch: got %q want %q", got, content)
	}

	// overwrite with different (smaller) content, then read back.
	if err := os.WriteFile(src, []byte("small"), 0o644); err != nil {
		t.Fatal(err)
	}
	if r := d.cmdPush("a", "/docs/hello.txt", src); !r.OK {
		t.Fatalf("push overwrite: %s", r.Error)
	}
	dest2 := filepath.Join(dir, "out2.txt")
	if r := d.cmdGet("a", "/docs/hello.txt", dest2); !r.OK {
		t.Fatalf("get after overwrite: %s", r.Error)
	}
	got2, _ := os.ReadFile(dest2)
	if string(got2) != "small" {
		t.Fatalf("overwrite mismatch: got %q", got2)
	}

	// rm the file; it should disappear and free its extent.
	if r := d.cmdRm("a", "/docs/hello.txt", false); !r.OK {
		t.Fatalf("rm: %s", r.Error)
	}
	idx, err := d.loadIndex("a", d.entries["a"])
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := idx.FindFile("/docs/hello.txt"); err == nil {
		t.Fatal("file still present after rm")
	}
	if len(idx.AvailableSpaces) == 0 {
		t.Fatal("rm did not free any available space")
	}

	// rm on a non-empty dir without -r must fail; with -r must succeed.
	src2 := filepath.Join(dir, "b.txt")
	_ = os.WriteFile(src2, []byte("bbb"), 0o644)
	if r := d.cmdPush("a", "/docs/b.txt", src2); !r.OK {
		t.Fatalf("push b: %s", r.Error)
	}
	if r := d.cmdRm("a", "/docs", false); r.OK {
		t.Fatal("rm of non-empty dir without -r should fail")
	}
	if r := d.cmdRm("a", "/docs", true); !r.OK {
		t.Fatalf("rm -r: %s", r.Error)
	}

	// commit the index back to tape.
	if r := d.cmdCommitIndex("a"); !r.OK {
		t.Fatalf("commitindex: %s", r.Error)
	}
}

func TestFileModeFlow(t *testing.T) {
	d, dir := setupTape(t, "file")
	runPushGetRm(t, d, dir)
}

func TestMemoryModeFlow(t *testing.T) {
	d, dir := setupTape(t, "memory")
	if !d.entries["a"].memMode {
		t.Fatal("expected memMode after indexread -m")
	}
	runPushGetRm(t, d, dir)
}

func TestPushGetDirectoryTree(t *testing.T) {
	d, dir := setupTape(t, "file")

	// Build a small local tree.
	tree := filepath.Join(dir, "tree")
	if err := os.MkdirAll(filepath.Join(tree, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	_ = os.WriteFile(filepath.Join(tree, "a.txt"), []byte("aaa"), 0o644)
	_ = os.WriteFile(filepath.Join(tree, "sub", "b.txt"), []byte("bbbb"), 0o644)

	if r := d.cmdPush("a", "/backup", tree); !r.OK {
		t.Fatalf("push dir: %s", r.Error)
	}

	// get the whole tree back.
	out := filepath.Join(dir, "restored")
	if r := d.cmdGet("a", "/backup", out); !r.OK {
		t.Fatalf("get dir: %s", r.Error)
	}
	if b, err := os.ReadFile(filepath.Join(out, "a.txt")); err != nil || string(b) != "aaa" {
		t.Fatalf("restored a.txt mismatch: %v %q", err, b)
	}
	if b, err := os.ReadFile(filepath.Join(out, "sub", "b.txt")); err != nil || string(b) != "bbbb" {
		t.Fatalf("restored sub/b.txt mismatch: %v %q", err, b)
	}
}

func TestDefragStub(t *testing.T) {
	d, _ := setupTape(t, "file")
	if r := d.cmdDefrag("a"); !r.OK {
		t.Fatalf("defrag stub: %s", r.Error)
	}
}
