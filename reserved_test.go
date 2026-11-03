package unionfs

import (
	"errors"
	"os"
	"syscall"
	"testing"
	"time"

	"github.com/absfs/absfs"
)

// reservedLower fills lower with ordinary names and names in the reserved
// ".wh." space, as a container-image test fixture might have them:
//
//	/a, /d/b, /d/.wh.b (a whiteout of /d/b, if read as one),
//	/.wh.x, /.wh.d/f, /d/.wh..wh.meta.b, /d/sub/.wh.__dir_opaque,
//	/d/sub/c, and a link /.wh.l -> a
func reservedLower(t *testing.T, lower absfs.SymlinkFileSystem) {
	t.Helper()
	mustMkdirAll(t, lower, "/d/sub")
	mustMkdirAll(t, lower, "/.wh.d")
	put(t, lower, "/a", "a")
	put(t, lower, "/d/b", "b")
	put(t, lower, "/d/.wh.b", "not a whiteout")
	put(t, lower, "/.wh.x", "x")
	put(t, lower, "/.wh.d/f", "f")
	put(t, lower, "/d/.wh..wh.meta.b", "")
	must(t, lower.Chmod("/d/.wh..wh.meta.b", 0700))
	put(t, lower, "/d/sub/.wh.__dir_opaque", "")
	put(t, lower, "/d/sub/c", "c")
	symlink(t, lower, "a", "/.wh.l")
}

func TestReservedNames(t *testing.T) {
	for _, kind := range layerKinds {
		t.Run(kind, func(t *testing.T) {
			_, _, lower := metaUnion(t, kind)
			must(t, lower.RemoveAll("/d"))
			reservedLower(t, lower)
			symlink(t, lower, "/.wh.d", "/linkdir") // not followed
			got, err := ReservedNames(lower, "/")
			must(t, err)
			wantNames(t, got, "/.wh.d", "/.wh.l", "/.wh.x", "/d/.wh..wh.meta.b", "/d/.wh.b", "/d/sub/.wh.__dir_opaque")
			got, err = ReservedNames(lower, "d/sub/")
			must(t, err)
			wantNames(t, got, "d/sub/.wh.__dir_opaque")
			got, err = ReservedNames(lower, "/d/sub/c/..") // cleaned lexically
			must(t, err)
			wantNames(t, got, "/d/sub/.wh.__dir_opaque")
			mustMkdirAll(t, lower, "/clean/x")
			got, err = ReservedNames(lower, "/clean")
			must(t, err)
			if got != nil {
				t.Fatalf("clean tree: %q", got)
			}
			if _, err := ReservedNames(lower, "/missing"); !os.IsNotExist(err) {
				t.Fatalf("missing root: %v", err)
			}
		})
	}
	faulty := newFaultFS(t)
	mustMkdirAll(t, faulty, "/a/b")
	faulty.failOn("readdir", "/a/b")
	_, err := ReservedNames(faulty, "/")
	wantInjected(t, "ReadDir below the root", err)
}

// By default every ".wh." name in a lower is a marker: it never shows, and
// its whiteouts and opaque markers count, against the layers below it.
func TestReservedNamesInLowersByDefault(t *testing.T) {
	ufs, _, lower := newTestUnion(t)
	reservedLower(t, lower)
	wantNames(t, list(t, ufs, "/"), "a", "d")
	wantNames(t, list(t, ufs, "/d"), "b", "sub") // the bottom layer's whiteout hides nothing
	wantNames(t, list(t, ufs, "/d/sub"), "c")
	wantNotExist(t, ufs, "/.wh.x")
	wantNotExist(t, ufs, "/.wh.d/f")

	bottom := mustNewMemFS()
	put(t, bottom, "/b", "b")
	stacked := New(WithReadOnlyLayer(lower), WithReadOnlyLayer(bottom))
	if _, err := stacked.Stat("/b"); err != nil {
		t.Fatal(err)
	}
	put(t, lower, "/.wh.b", "")
	wantNotExist(t, stacked, "/b")
}

// With WithMarkersOnlyInWritableLayer, lower ".wh." names are ordinary,
// read-only entries; the writable layer's markers work as before.
func TestMarkersOnlyInWritableLayer(t *testing.T) {
	for _, kind := range layerKinds {
		for _, cached := range []bool{false, true} {
			name := kind
			if cached {
				name += "/LookupCache"
			}
			t.Run(name, func(t *testing.T) {
				_, upper, lower := metaUnion(t, kind)
				must(t, lower.RemoveAll("/d"))
				reservedLower(t, lower)
				opts := []Option{WithWritableLayer(upper), WithReadOnlyLayer(lower), WithMarkersOnlyInWritableLayer()}
				if cached {
					opts = append(opts, WithLookupCache(time.Hour, 0))
				}
				ufs := New(opts...)

				wantNames(t, list(t, ufs, "/"), ".wh.d", ".wh.l", ".wh.x", "a", "d", "top")
				wantNames(t, list(t, ufs, "/d"), ".wh..wh.meta.b", ".wh.b", "b", "sub")
				wantNames(t, list(t, ufs, "/d/sub"), ".wh.__dir_opaque", "c")
				wantNames(t, list(t, ufs, "/.wh.d"), "f")
				for p, want := range map[string]string{
					"/.wh.x": "x", "/.wh.d/f": "f", "/d/b": "b", "/d/.wh.b": "not a whiteout", "/.wh.l": "a",
				} {
					if got := get(t, ufs, p); got != want {
						t.Fatalf("%s = %q, want %q", p, got, want)
					}
				}
				if !isSymlink(lstat(t, ufs, "/.wh.l")) {
					t.Fatal("link")
				}
				target, err := ufs.Readlink("/.wh.l")
				must(t, err)
				if target != "a" {
					t.Fatalf("readlink %q", target)
				}
				// A lower metadata marker is an ordinary file, and applies
				// to nothing.
				wantMode(t, "lower marker not applied", stat(t, ufs, "/d/b"), 0644)

				// The writable layer's markers still count.
				must(t, ufs.Chmod("/d/b", 0750))
				wantMode(t, "upper marker", stat(t, ufs, "/d/b"), 0750)
				must(t, ufs.Remove("/a"))
				wantNotExist(t, ufs, "/a")
				must(t, ufs.RemoveAll("/d/sub"))
				wantNotExist(t, ufs, "/d/sub/c")
				must(t, ufs.Mkdir("/d/sub", 0755))
				wantNames(t, list(t, ufs, "/d/sub"))
				for _, p := range []string{"/.wh.a", "/d/sub/.wh.__dir_opaque", "/d/.wh..wh.meta.b"} {
					if !exists(upper, p) {
						t.Fatalf("upper lacks %s", p)
					}
				}
				wantNames(t, list(t, ufs, "/d"), ".wh..wh.meta.b", ".wh.b", "b", "sub")
				wantNames(t, list(t, ufs, "/"), ".wh.d", ".wh.l", ".wh.x", "d", "top")

				// Changing a reserved lower name, or anything under one, is
				// refused: the writable layer could not represent it.
				eperm := func(what string, err error) {
					t.Helper()
					if !errors.Is(err, syscall.EPERM) {
						t.Fatalf("%s: err = %v, want EPERM", what, err)
					}
				}
				eperm("Chmod", ufs.Chmod("/.wh.x", 0600))
				eperm("Chtimes", ufs.Chtimes("/.wh.x", changedTime, changedTime))
				eperm("Chown", ufs.Chown("/.wh.x", os.Getuid(), os.Getgid()))
				eperm("Lchown", ufs.Lchown("/.wh.l", os.Getuid(), os.Getgid()))
				eperm("Chmod dir", ufs.Chmod("/.wh.d", 0700))
				eperm("Chmod under", ufs.Chmod("/.wh.d/f", 0600))
				eperm("Truncate", ufs.Truncate("/.wh.x", 0))
				_, err = ufs.OpenFile("/.wh.x", os.O_WRONLY, 0)
				eperm("OpenFile", err)
				_, err = ufs.Create("/.wh.d/new")
				eperm("Create under", err)
				eperm("Mkdir under", ufs.Mkdir("/.wh.d/new", 0755))
				eperm("MkdirAll under", ufs.MkdirAll("/.wh.d/new/deeper", 0755))
				eperm("Symlink under", ufs.Symlink("a", "/.wh.d/ln"))
				eperm("Remove", ufs.Remove("/.wh.x"))
				eperm("Remove under", ufs.Remove("/.wh.d/f"))
				eperm("RemoveAll", ufs.RemoveAll("/.wh.d"))
				eperm("Rename", ufs.Rename("/.wh.x", "/y"))
				eperm("Rename onto", ufs.Rename("/top", "/.wh.x"))
				eperm("Rename a directory holding one", ufs.Rename("/d", "/d2"))
				if get(t, ufs, "/.wh.x") != "x" || get(t, ufs, "/.wh.d/f") != "f" {
					t.Fatal("a refused change changed something")
				}
				for _, p := range []string{"/.wh.x", "/.wh.d", "/.wh..wh.x", "/.wh..wh.meta..wh.x"} {
					if exists(upper, p) {
						t.Fatalf("upper has %s", p)
					}
				}
				// Removing the directory that holds them hides them, with
				// one whiteout.
				must(t, ufs.RemoveAll("/d"))
				wantNotExist(t, ufs, "/d/.wh.b")
				wantNames(t, list(t, ufs, "/"), ".wh.d", ".wh.l", ".wh.x", "top")
			})
		}
	}
}

// Under the option, a read-only union honours no markers at all, and a
// lower whiteout no longer hides the name below it.
func TestMarkersOnlyInWritableLayerStacked(t *testing.T) {
	mid, bottom := mustNewMemFS().(absfs.SymlinkFileSystem), mustNewMemFS().(absfs.SymlinkFileSystem)
	put(t, bottom, "/gone", "b")
	mustMkdirAll(t, bottom, "/o")
	put(t, bottom, "/o/below", "b")
	put(t, mid, "/.wh.gone", "")
	mustMkdirAll(t, mid, "/o")
	put(t, mid, "/o/"+OpaqueWhiteout, "")
	put(t, mid, "/"+OpaqueWhiteout, "")

	stacked := New(WithReadOnlyLayer(mid), WithReadOnlyLayer(bottom))
	wantNames(t, list(t, stacked, "/"), "o")
	wantNames(t, list(t, stacked, "/o"))

	plain := New(WithReadOnlyLayer(mid), WithReadOnlyLayer(bottom), WithMarkersOnlyInWritableLayer())
	wantNames(t, list(t, plain, "/"), ".wh.__dir_opaque", ".wh.gone", "gone", "o")
	wantNames(t, list(t, plain, "/o"), ".wh.__dir_opaque", "below")
	if get(t, plain, "/gone") != "b" {
		t.Fatal("whited-out name")
	}
}
