package unionfs

import (
	"errors"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/absfs/absfs"
	"github.com/absfs/memfs"
)

// newTestUnion returns a union of a fresh writable memfs upper over a fresh
// memfs lower, plus both layers so tests can inspect them directly.
func newTestUnion(t *testing.T) (*UnionFS, absfs.SymlinkFileSystem, absfs.SymlinkFileSystem) {
	t.Helper()
	upper, err := memfs.NewFS()
	if err != nil {
		t.Fatal(err)
	}
	lower, err := memfs.NewFS()
	if err != nil {
		t.Fatal(err)
	}
	return New(WithWritableLayer(upper), WithReadOnlyLayer(lower)), upper, lower
}

// put writes data to name in fs, creating it.
func put(t *testing.T, fs absfs.FileSystem, name, data string) {
	t.Helper()
	f, err := fs.OpenFile(name, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		t.Fatalf("create %s: %v", name, err)
	}
	if _, err := f.Write([]byte(data)); err != nil {
		t.Fatalf("write %s: %v", name, err)
	}
	if err := f.Close(); err != nil {
		t.Fatalf("close %s: %v", name, err)
	}
}

func mustMkdirAll(t *testing.T, fs absfs.FileSystem, name string) {
	t.Helper()
	if err := fs.MkdirAll(name, 0755); err != nil {
		t.Fatalf("mkdirall %s: %v", name, err)
	}
}

// exists reports whether name exists in fs without following symlinks.
func exists(fs absfs.SymlinkFileSystem, name string) bool {
	_, err := fs.Lstat(name)
	return err == nil
}

func TestRemoveNonEmptyMergedDir(t *testing.T) {
	ufs, _, lower := newTestUnion(t)
	mustMkdirAll(t, lower, "/d")
	put(t, lower, "/d/f", "x")

	err := ufs.Remove("/d")
	if !errors.Is(err, syscall.ENOTEMPTY) {
		t.Fatalf("Remove of non-empty merged dir: got %v, want ENOTEMPTY", err)
	}
	if _, err := ufs.Stat("/d/f"); err != nil {
		t.Fatalf("child vanished after failed Remove: %v", err)
	}
}

func TestRemoveDirClearsItsWhiteouts(t *testing.T) {
	ufs, upper, lower := newTestUnion(t)
	mustMkdirAll(t, lower, "/d")
	put(t, lower, "/d/a", "a")
	put(t, lower, "/d/b", "b")

	// Copy /d up so the upper holds the whiteouts for its children.
	if err := ufs.Chtimes("/d", time.Now(), time.Now()); err != nil {
		t.Fatal(err)
	}
	for _, n := range []string{"/d/a", "/d/b"} {
		if err := ufs.Remove(n); err != nil {
			t.Fatalf("Remove %s: %v", n, err)
		}
	}
	if !exists(upper, "/d/.wh.a") || !exists(upper, "/d/.wh.b") {
		t.Fatal("expected whiteouts for lower children in the upper")
	}

	if err := ufs.Remove("/d"); err != nil {
		t.Fatalf("Remove of emptied dir: %v", err)
	}
	if _, err := ufs.Stat("/d"); !os.IsNotExist(err) {
		t.Fatalf("Stat after Remove: got %v, want not exist", err)
	}
	if exists(upper, "/d/.wh.a") {
		t.Fatal("whiteouts inside the removed directory survived")
	}
	if !exists(upper, "/.wh.d") {
		t.Fatal("expected a whiteout for the lower directory")
	}
	if !exists(lower, "/d/a") {
		t.Fatal("lower was modified")
	}
}

func TestRemoveUpperOnlyLeavesNoWhiteout(t *testing.T) {
	ufs, upper, _ := newTestUnion(t)
	put(t, ufs.FileSystem(), "/new.txt", "n")
	if err := ufs.Remove("/new.txt"); err != nil {
		t.Fatal(err)
	}
	if exists(upper, "/.wh.new.txt") {
		t.Fatal("whiteout written for an upper-only file")
	}

	if err := ufs.Mkdir("/newdir", 0755); err != nil {
		t.Fatal(err)
	}
	if err := ufs.Remove("/newdir"); err != nil {
		t.Fatal(err)
	}
	if exists(upper, "/.wh.newdir") {
		t.Fatal("whiteout written for an upper-only directory")
	}
}

func TestRemoveShadowedFileWritesWhiteout(t *testing.T) {
	ufs, upper, lower := newTestUnion(t)
	put(t, lower, "/f", "lower")
	put(t, ufs.FileSystem(), "/f", "upper") // copy-up
	if err := ufs.Remove("/f"); err != nil {
		t.Fatal(err)
	}
	if !exists(upper, "/.wh.f") {
		t.Fatal("no whiteout for a name the lower still has")
	}
	if _, err := ufs.Stat("/f"); !os.IsNotExist(err) {
		t.Fatalf("lower file reappeared: %v", err)
	}
}

// The sequence from the C4 prototype report: mkdir d; touch d/f; rm d/f;
// rmdir d, all in the writable layer, with and without a lower d.
func TestRemoveReportedSequence(t *testing.T) {
	for _, lowerHasD := range []bool{false, true} {
		ufs, upper, lower := newTestUnion(t)
		if lowerHasD {
			mustMkdirAll(t, lower, "/d")
			put(t, lower, "/d/g", "g")
			if err := ufs.Remove("/d/g"); err != nil {
				t.Fatal(err)
			}
		} else if err := ufs.Mkdir("/d", 0755); err != nil {
			t.Fatal(err)
		}
		put(t, ufs, "/d/f", "f")
		if err := ufs.Remove("/d/f"); err != nil {
			t.Fatal(err)
		}
		if exists(upper, "/d/.wh.f") {
			t.Fatal("whiteout for an upper-only file")
		}
		if err := ufs.Remove("/d"); err != nil {
			t.Fatalf("rmdir (lower has d: %v): %v", lowerHasD, err)
		}
		if _, err := ufs.Stat("/d"); !os.IsNotExist(err) {
			t.Fatalf("d still visible: %v", err)
		}
		if exists(upper, "/.wh.d") != lowerHasD {
			t.Fatalf("whiteout for d = %v, want %v", !lowerHasD, lowerHasD)
		}
	}
}
