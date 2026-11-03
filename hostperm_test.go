package unionfs

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"testing"
	"time"
)

// skipIfRoot skips tests that rely on the host enforcing permissions.
func skipIfRoot(t *testing.T) {
	t.Helper()
	if os.Geteuid() == 0 {
		t.Skip("permissions are not enforced for root")
	}
}

// chmodHost changes a host path's mode and puts it back when the test ends,
// so t.TempDir can clean up.
func chmodHost(t *testing.T, name string, mode os.FileMode) {
	t.Helper()
	if err := os.Chmod(name, mode); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(name, 0755) })
}

// TestOSLayersReadOnlyLowerAncestor changes files below a read-only lower
// directory. On a host this is allowed, since the read-only directory itself
// is not modified, and the union must allow it too even though copying up
// recreates the read-only directory in the writable layer.
func TestOSLayersReadOnlyLowerAncestor(t *testing.T) {
	skipIfRoot(t)
	ufs, upperDir, lowerDir := osLayers(t)
	hostWrite(t, filepath.Join(lowerDir, "ro", "sub", "f"), "f")
	hostWrite(t, filepath.Join(lowerDir, "ro", "sub", "g"), "g")
	hostWrite(t, filepath.Join(lowerDir, "ro", "sub", "h"), "h")
	hostWrite(t, filepath.Join(lowerDir, "ro", "direct"), "d")
	chmodHost(t, filepath.Join(lowerDir, "ro"), 0555)
	old := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	must(t, os.Chtimes(filepath.Join(lowerDir, "ro"), old, old))
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(upperDir, "ro"), 0755) })

	must(t, ufs.Chmod("/ro/sub/f", 0600))
	f, err := ufs.OpenFile("/ro/sub/g", os.O_WRONLY|os.O_APPEND, 0)
	must(t, err)
	_, err = f.Write([]byte("+"))
	must(t, err)
	must(t, f.Close())
	must(t, ufs.Remove("/ro/sub/h"))
	put(t, ufs, "/ro/sub/new", "n")
	if get(t, ufs, "/ro/sub/g") != "g+" {
		t.Fatal("append through a read-only ancestor")
	}
	wantNames(t, list(t, ufs, "/ro/sub"), "f", "g", "new")

	// A file directly inside the read-only directory can still be changed,
	// as on a host, but not removed or created.
	must(t, ufs.Chmod("/ro/direct", 0600))
	wantErrIs(t, ufs.Remove("/ro/direct"), fs.ErrPermission)
	_, err = ufs.Create("/ro/other")
	wantErrIs(t, err, fs.ErrPermission)

	// The copied-up directory keeps the lower mode and time.
	info, err := ufs.Stat("/ro")
	must(t, err)
	if info.Mode().Perm() != 0555 {
		t.Fatalf("mode of the copied-up read-only directory = %v", info.Mode())
	}
	if !info.ModTime().Equal(old) {
		t.Fatalf("mtime of the copied-up directory = %v, want %v", info.ModTime(), old)
	}

	// Moving a tree that holds a read-only directory copies it up whole.
	hostWrite(t, filepath.Join(lowerDir, "tree", "ro2", "x"), "x")
	chmodHost(t, filepath.Join(lowerDir, "tree", "ro2"), 0555)
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(upperDir, "moved", "ro2"), 0755) })
	must(t, ufs.Rename("/tree", "/moved"))
	if get(t, ufs, "/moved/ro2/x") != "x" {
		t.Fatal("moved read-only directory")
	}
	info, err = ufs.Stat("/moved/ro2")
	must(t, err)
	if info.Mode().Perm() != 0555 {
		t.Fatalf("mode of the moved read-only directory = %v", info.Mode())
	}
}

// TestOSLayersFailedRenameKeepsTarget checks that a rename the host refuses
// for a reason other than the target leaves the target alone.
func TestOSLayersFailedRenameKeepsTarget(t *testing.T) {
	skipIfRoot(t)
	ufs, upperDir, _ := osLayers(t)
	must(t, ufs.Mkdir("/ro", 0755))
	put(t, ufs, "/ro/f", "f")
	put(t, ufs, "/dst", "precious")
	must(t, ufs.Chmod("/ro", 0555))
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(upperDir, "ro"), 0755) })
	// basefs reports a failed host rename as a plain string, so only the
	// failure itself can be checked.
	if err := ufs.Rename("/ro/f", "/dst"); err == nil {
		t.Fatal("rename out of a read-only directory succeeded")
	}
	if get(t, ufs, "/dst") != "precious" {
		t.Fatal("target lost")
	}
	if get(t, ufs, "/ro/f") != "f" {
		t.Fatal("source lost")
	}
}

// TestOSLayersFailuresDoNotResurrect checks that a Remove or Rename the host
// refuses does not bring back lower entries deleted earlier.
func TestOSLayersFailuresDoNotResurrect(t *testing.T) {
	skipIfRoot(t)
	ufs, upperDir, lowerDir := osLayers(t)
	hostWrite(t, filepath.Join(lowerDir, "p", "d", "x"), "x")
	hostWrite(t, filepath.Join(lowerDir, "q", "d", "a"), "a")
	hostWrite(t, filepath.Join(lowerDir, "q", "d", "b"), "b")
	t.Cleanup(func() {
		_ = os.Chmod(filepath.Join(upperDir, "p"), 0755)
		_ = os.Chmod(filepath.Join(upperDir, "q"), 0755)
	})

	must(t, ufs.Remove("/p/d/x"))
	must(t, ufs.Chmod("/p", 0555))
	wantErrIs(t, ufs.Remove("/p/d"), fs.ErrPermission)
	wantNotExist(t, ufs, "/p/d/x")
	wantNames(t, list(t, ufs, "/p/d"))

	must(t, ufs.Remove("/q/d/a"))
	must(t, ufs.Chmod("/q", 0555))
	if err := ufs.Rename("/q/d", "/e"); err == nil {
		t.Fatal("rename out of a read-only directory succeeded")
	}
	wantNotExist(t, ufs, "/q/d/a")
	wantNames(t, list(t, ufs, "/q/d"), "b")
}

// TestFailuresDoNotResurrect injects layer failures part way through Remove
// and Rename and checks that no deleted lower entry comes back.
func TestFailuresDoNotResurrect(t *testing.T) {
	t.Run("RenameOfDirectory", func(t *testing.T) {
		ufs, upper, _ := faultUnion(t)
		must(t, ufs.Remove("/d/a"))
		upper.failOn("rename", "/d")
		wantInjected(t, "rename", ufs.Rename("/d", "/e"))
		upper.clear()
		wantNotExist(t, ufs, "/d/a")
		wantNames(t, list(t, ufs, "/d"), "b", "sub")
		wantNotExist(t, ufs, "/e")
		// It works once the layer does, and leaves no stale markers.
		must(t, ufs.Rename("/d", "/e"))
		wantNames(t, list(t, ufs, "/e"), "b", "sub")
		wantNames(t, upperMarkers(t, upper), "/.wh.d")
	})
	t.Run("RemoveOfDirectory", func(t *testing.T) {
		ufs, upper, _ := faultUnion(t)
		must(t, ufs.Remove("/d/sub/c"))
		upper.failOn("rename", "/d/sub")
		wantInjected(t, "remove", ufs.Remove("/d/sub"))
		upper.clear()
		wantNotExist(t, ufs, "/d/sub/c")
		wantNames(t, list(t, ufs, "/d/sub"))
		wantNames(t, upperMarkers(t, upper), "/d/sub/.wh.c")
	})
	t.Run("RemoveOfModifiedFile", func(t *testing.T) {
		ufs, upper, _ := faultUnion(t)
		put(t, ufs, "/d/a", "edited")
		upper.failOn("remove", "/d/a")
		wantInjected(t, "remove", ufs.Remove("/d/a"))
		upper.clear()
		if get(t, ufs, "/d/a") != "edited" {
			t.Fatal("the edited file must survive a failed Remove")
		}
		wantNames(t, upperMarkers(t, upper))
		must(t, ufs.Remove("/d/a"))
		wantNotExist(t, ufs, "/d/a")
		wantNames(t, upperMarkers(t, upper), "/d/.wh.a")
	})
	t.Run("RemoveAllLeftovers", func(t *testing.T) {
		ufs, upper, _ := faultUnion(t)
		must(t, ufs.Remove("/d/a"))
		put(t, ufs, "/d/new", "n")
		upper.failOn("removeall", "*")
		err := ufs.RemoveAll("/d")
		wantInjected(t, "removeall", err)
		upper.clear()
		// The tree is gone from the merged view; the upper keeps a hidden
		// leftover that is never listed.
		wantNotExist(t, ufs, "/d")
		wantNames(t, list(t, ufs, "/"), "top")
	})
}

// TestCopyUpKeepsParentTimes checks that a metadata-only change to a lower
// file does not change its directory's modification time.
func TestCopyUpKeepsParentTimes(t *testing.T) {
	old := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	check := func(t *testing.T, ufs *UnionFS, lower func(string, time.Time)) {
		lower("/d", old)
		lower("/d/sub", old)
		lower("/", old)
		for _, op := range []func() error{
			func() error { return ufs.Chmod("/d/a", 0600) },
			func() error { return ufs.Chtimes("/d/sub/c", time.Now(), time.Now()) },
			func() error { return ufs.Truncate("/d/b", 1) },
		} {
			must(t, op())
		}
		for _, p := range []string{"/d", "/d/sub"} {
			info, err := ufs.Stat(p)
			must(t, err)
			if !info.ModTime().Equal(old) {
				t.Fatalf("mtime of %s = %v, want %v", p, info.ModTime(), old)
			}
		}
		// A real change to the directory does move its time.
		put(t, ufs, "/d/fresh", "f")
		info, err := ufs.Stat("/d")
		must(t, err)
		if info.ModTime().Equal(old) {
			t.Fatal("creating a file must update its directory's mtime")
		}
	}
	t.Run("memfs", func(t *testing.T) {
		ufs, _, lower := newTestUnion(t)
		lowerTree(t, lower)
		check(t, ufs, func(p string, tm time.Time) { must(t, lower.Chtimes(p, tm, tm)) })
	})
	t.Run("os", func(t *testing.T) {
		ufs, _, lowerDir := osLayers(t)
		hostWrite(t, filepath.Join(lowerDir, "d", "a"), "la")
		hostWrite(t, filepath.Join(lowerDir, "d", "b"), "lb")
		hostWrite(t, filepath.Join(lowerDir, "d", "sub", "c"), "lc")
		check(t, ufs, func(p string, tm time.Time) {
			must(t, os.Chtimes(filepath.Join(lowerDir, p), tm, tm))
		})
	})
}

// TestMkdirThroughLinkInvalidatesCache creates directories through a
// symbolic link to their parent after a cached miss for the same name.
func TestMkdirThroughLinkInvalidatesCache(t *testing.T) {
	upper, lower := mustNewMemFS(), mustNewMemFS()
	mustMkdirAll(t, lower, "/a")
	ufs := New(WithWritableLayer(upper), WithReadOnlyLayer(lower), WithStatCache(true, time.Minute))
	must(t, ufs.Symlink("/a", "/link"))
	for _, p := range []string{"/link/new", "/link/x", "/link/x/y"} {
		if _, err := ufs.Stat(p); !errors.Is(err, fs.ErrNotExist) {
			t.Fatalf("Stat %s before: %v", p, err)
		}
	}
	must(t, ufs.Mkdir("/link/new", 0755))
	must(t, ufs.MkdirAll("/link/x/y", 0755))
	for _, p := range []string{"/link/new", "/link/x", "/link/x/y"} {
		if _, err := ufs.Stat(p); err != nil {
			t.Fatalf("Stat %s after: %v", p, err)
		}
	}
}

// TestOSLayersRemoveReadOnlyDirectory removes an emptied read-only
// directory that still holds whiteouts in the writable layer: rmdir needs
// permission on the parent only, so the union must clear its own markers.
func TestOSLayersRemoveReadOnlyDirectory(t *testing.T) {
	skipIfRoot(t)
	ufs, upperDir, lowerDir := osLayers(t)
	hostWrite(t, filepath.Join(lowerDir, "w", "x"), "x")
	must(t, ufs.Remove("/w/x"))
	must(t, ufs.Chmod("/w", 0555))
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(upperDir, "w"), 0755) })
	must(t, ufs.Remove("/w"))
	wantNotExist(t, ufs, "/w")
	wantNames(t, hostTree(t, upperDir), "/.wh.w")

	// The same for a directory only the writable layer has, holding a
	// stray marker.
	must(t, ufs.Mkdir("/u", 0755))
	hostWrite(t, filepath.Join(upperDir, "u", ".wh.stray"), "")
	must(t, ufs.Chmod("/u", 0555))
	t.Cleanup(func() { _ = os.Chmod(filepath.Join(upperDir, "u"), 0755) })
	must(t, ufs.Remove("/u"))
	wantNames(t, hostTree(t, upperDir), "/.wh.w")
}
