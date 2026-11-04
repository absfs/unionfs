package unionfs

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/absfs/absfs"
	"github.com/absfs/basefs"
	"github.com/absfs/osfs"
)

// foldedKinds are the layer kinds foldedLayers makes.
var foldedKinds = []string{"memfs", "osfs"}

// foldedLayers returns a writable and a read-only layer that both fold the
// case of names: foldFS over memfs, or host directories where the host
// folds case (macOS and Windows by default). It skips the test where the
// host does not.
func foldedLayers(t *testing.T, kind string) (upper, lower absfs.SymlinkFileSystem) {
	t.Helper()
	if kind == "memfs" {
		return newFoldFS(t), newFoldFS(t)
	}
	if runtime.GOOS == "windows" {
		t.Skip("host-directory layers are exercised on Unix")
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	must(t, err)
	must(t, os.WriteFile(filepath.Join(root, "probe"), nil, 0644))
	if _, err := os.Lstat(filepath.Join(root, "PROBE")); err != nil {
		t.Skip("the host does not fold case here")
	}
	host, err := osfs.NewFS()
	must(t, err)
	var layers [2]absfs.SymlinkFileSystem
	for i, d := range []string{"upper", "lower"} {
		must(t, os.Mkdir(filepath.Join(root, d), 0755))
		l, err := basefs.NewFS(host, filepath.Join(root, d))
		must(t, err)
		layers[i] = l
	}
	return layers[0], layers[1]
}

// withCache runs f with and without the lookup cache.
func withCache(t *testing.T, kind string, f func(t *testing.T, cache []Option)) {
	for _, cached := range []bool{false, true} {
		name := kind
		var opts []Option
		if cached {
			name += "/LookupCache"
			opts = append(opts, WithLookupCache(time.Hour, 0))
		}
		t.Run(name, func(t *testing.T) { f(t, opts) })
	}
}

func wantGone(t *testing.T, what string, err error) {
	t.Helper()
	if !os.IsNotExist(err) {
		t.Errorf("%s: err = %v, want not exist", what, err)
	}
}

// On a writable layer that folds case, the union's markers are reachable
// by spellings of their names other than the union's own: ".WH.x" is the
// whiteout ".wh.x" there. None of them is an entry of the merged view, and
// no change through one reaches the marker.
func TestFoldedMarkerSpellingsUnreachable(t *testing.T) {
	for _, kind := range foldedKinds {
		withCache(t, kind, func(t *testing.T, cache []Option) {
			upper, lower := foldedLayers(t, kind)
			for _, p := range []string{"/x", "/y", "/z", "/dir/inner"} {
				mustMkdirAll(t, lower, "/dir")
				put(t, lower, p, "lower "+p)
			}
			ufs := New(append([]Option{WithWritableLayer(upper), WithReadOnlyLayer(lower)}, cache...)...)

			must(t, ufs.Remove("/x"))
			must(t, ufs.Remove("/z"))
			must(t, ufs.RemoveAll("/dir"))
			must(t, ufs.Mkdir("/dir", 0755))
			must(t, ufs.Chmod("/y", 0600))
			markers := []string{"/.wh.x", "/.wh.z", "/dir/.wh.__dir_opaque", "/.wh..wh.meta.y"}
			for _, m := range markers {
				if !exists(upper, m) {
					t.Fatalf("upper lacks %s", m)
				}
			}

			for _, p := range []string{"/.WH.x", "/.Wh.z", "/dir/.WH.__DIR_OPAQUE", "/dir/.wH.__dir_opaque", "/.WH..WH.META.y", "/.wh..WH.meta.y"} {
				wantNotExist(t, ufs, p)
				_, err := ufs.ReadFile(p)
				wantGone(t, "ReadFile "+p, err)
				_, err = ufs.OpenFile(p, os.O_WRONLY|os.O_APPEND, 0)
				wantGone(t, "OpenFile "+p, err)
				wantGone(t, "Chmod "+p, ufs.Chmod(p, 0604))
				wantGone(t, "Chtimes "+p, ufs.Chtimes(p, changedTime, changedTime))
				wantGone(t, "Chown "+p, ufs.Chown(p, os.Getuid(), os.Getgid()))
				wantGone(t, "Lchown "+p, ufs.Lchown(p, os.Getuid(), os.Getgid()))
				wantGone(t, "Truncate "+p, ufs.Truncate(p, 0))
				wantGone(t, "Readlink "+p, func() error { _, err := ufs.Readlink(p); return err }())
				wantGone(t, "Rename "+p, ufs.Rename(p, "/moved"))
				wantGone(t, "Remove "+p, ufs.Remove(p))
				must(t, ufs.RemoveAll(p)) // nothing there
			}

			ufs.ClearCache()
			for _, m := range markers {
				if !exists(upper, m) {
					t.Fatalf("a change through another spelling removed %s", m)
				}
			}
			wantNotExist(t, ufs, "/x")
			wantNotExist(t, ufs, "/z")
			wantNotExist(t, ufs, "/moved")
			wantNotExist(t, ufs, "/dir/inner")
			wantMode(t, "/y", stat(t, ufs, "/y"), 0600)
			wantNames(t, list(t, ufs, "/"), "dir", "y")
			wantNames(t, list(t, ufs, "/dir"))
		})
	}
}

// By default a read-only layer's names that start with the whiteout prefix
// in another case are reserved as the lowercase ones are: they would be
// markers on a host that folds case. Under WithMarkersOnlyInWritableLayer
// they are ordinary read-only entries, and the writable layer's markers are
// still not reached through them.
func TestFoldedReservedNamesInLowers(t *testing.T) {
	lower := mustNewMemFS().(absfs.SymlinkFileSystem)
	mustMkdirAll(t, lower, "/.Wh.d")
	put(t, lower, "/.WH.a", "a")
	put(t, lower, "/.Wh.d/f", "f")
	put(t, lower, "/.WH.q", "lower .WH.q")
	put(t, lower, "/q", "q")
	put(t, lower, "/plain", "p")

	got, err := ReservedNames(lower, "/")
	must(t, err)
	wantNames(t, got, "/.WH.a", "/.WH.q", "/.Wh.d")

	ro := New(WithReadOnlyLayer(lower))
	wantNames(t, list(t, ro, "/"), "plain", "q")
	wantNotExist(t, ro, "/.WH.a")
	wantNotExist(t, ro, "/.Wh.d/f")

	for _, cached := range []bool{false, true} {
		upper := newFoldFS(t)
		opts := []Option{WithWritableLayer(upper), WithReadOnlyLayer(lower), WithMarkersOnlyInWritableLayer()}
		if cached {
			opts = append(opts, WithLookupCache(time.Hour, 0))
		}
		ufs := New(opts...)
		must(t, ufs.Remove("/q"))
		if !exists(upper, "/.wh.q") {
			t.Fatal("no whiteout for /q")
		}
		wantNames(t, list(t, ufs, "/"), ".WH.a", ".WH.q", ".Wh.d", "plain")
		// The lower's own entry, not the writable layer's whiteout.
		if got := get(t, ufs, "/.WH.q"); got != "lower .WH.q" {
			t.Fatalf("/.WH.q = %q", got)
		}
		if got := get(t, ufs, "/.Wh.d/f"); got != "f" {
			t.Fatalf("/.Wh.d/f = %q", got)
		}
		eperm := func(what string, err error) {
			t.Helper()
			if !errors.Is(err, syscall.EPERM) {
				t.Fatalf("%s: err = %v, want EPERM", what, err)
			}
		}
		eperm("Chmod", ufs.Chmod("/.WH.a", 0600))
		eperm("Remove", ufs.Remove("/.WH.q"))
		eperm("Rename", ufs.Rename("/.WH.a", "/b"))
		eperm("Truncate under", ufs.Truncate("/.Wh.d/f", 0))
		_, err = ufs.Create("/.Wh.d/new")
		eperm("Create under", err)
		if !exists(upper, "/.wh.q") || len(list(t, upper, "/")) != 1 {
			t.Fatalf("writable layer: %v", list(t, upper, "/"))
		}
		wantNotExist(t, ufs, "/q")
	}
}

// staleUnion returns a union with the lookup cache over layers that fold
// case, with lower /d/x, /d/sub/f and /d/m, all of them cached under the
// spelling "/d".
func staleUnion(t *testing.T, kind string) (*UnionFS, absfs.SymlinkFileSystem) {
	t.Helper()
	upper, lower := foldedLayers(t, kind)
	mustMkdirAll(t, lower, "/d/sub")
	put(t, lower, "/d/x", "hidden lower data")
	put(t, lower, "/d/sub/f", "hidden f")
	put(t, lower, "/d/m", "m")
	ufs := New(WithWritableLayer(upper), WithReadOnlyLayer(lower), WithLookupCache(time.Hour, 0))
	for _, p := range []string{"/d/x", "/d/sub/f", "/d/m"} {
		lstat(t, ufs, p)
	}
	return ufs, upper
}

// A change through one spelling of a name leaves what the lookup cache
// holds under another as it was, so the old view may show there until it
// expires. A write through it must not make that view permanent: copying
// up a lower file the change hid would resurrect it.
func TestLookupCacheStaleSpellingNeverPersisted(t *testing.T) {
	for _, kind := range foldedKinds {
		// /D emptied and remade: the new directory is opaque.
		t.Run(kind+"/opaque", func(t *testing.T) {
			writes := map[string]func(ufs *UnionFS) error{
				"append": func(ufs *UnionFS) error {
					f, err := ufs.OpenFile("/d/x", os.O_WRONLY|os.O_APPEND, 0)
					if err == nil {
						f.Close()
					}
					return err
				},
				"truncate": func(ufs *UnionFS) error { return ufs.Truncate("/d/x", 1) },
				"chmod":    func(ufs *UnionFS) error { return ufs.Chmod("/d/x", 0600) },
				"chtimes":  func(ufs *UnionFS) error { return ufs.Chtimes("/d/x", changedTime, changedTime) },
				"rename":   func(ufs *UnionFS) error { return ufs.Rename("/d/x", "/moved") },
				"renamedir": func(ufs *UnionFS) error {
					return ufs.Rename("/d/sub", "/moved")
				},
			}
			for name, write := range writes {
				t.Run(name, func(t *testing.T) {
					ufs, upper := staleUnion(t, kind)
					must(t, ufs.RemoveAll("/D"))
					must(t, ufs.Mkdir("/D", 0755))
					wantNotExist(t, ufs, "/D/x")
					if err := write(ufs); err == nil {
						t.Fatal("a write through the stale spelling succeeded")
					}
					// The refusal emptied the cache.
					wantNotExist(t, ufs, "/d/x")
					wantNotExist(t, ufs, "/moved/f")
					ufs.ClearCache()
					wantNames(t, list(t, ufs, "/d"))
					wantNames(t, list(t, upper, "/d"), OpaqueWhiteout)
				})
			}
		})
		// /D removed: a write through /d must not bring the directory back,
		// with the lower entries under it.
		t.Run(kind+"/whited-out", func(t *testing.T) {
			for _, write := range []func(ufs *UnionFS) error{
				func(ufs *UnionFS) error {
					_, err := ufs.OpenFile("/d/x", os.O_WRONLY|os.O_APPEND, 0)
					return err
				},
				func(ufs *UnionFS) error {
					_, err := ufs.Create("/d/new")
					return err
				},
				func(ufs *UnionFS) error { return ufs.Mkdir("/d/new", 0755) },
				func(ufs *UnionFS) error { return ufs.Symlink("x", "/d/new") },
			} {
				ufs, upper := staleUnion(t, kind)
				must(t, ufs.RemoveAll("/D"))
				wantGone(t, "write", write(ufs))
				ufs.ClearCache()
				wantNotExist(t, ufs, "/d")
				wantNotExist(t, ufs, "/d/x")
				if ents := list(t, upper, "/"); len(ents) != 1 || !strings.EqualFold(ents[0], ".wh.d") {
					t.Fatalf("writable layer: %v", ents)
				}
			}
		})
		// /D copied up through the other spelling: a directory the union
		// itself joined, so the cached /d is still right about it, and
		// writes through it work.
		t.Run(kind+"/joined", func(t *testing.T) {
			ufs, _ := staleUnion(t, kind)
			put(t, ufs, "/D/y", "y")
			put(t, ufs, "/d/x", "new")
			must(t, ufs.Mkdir("/d/sub/deeper", 0755))
			ufs.ClearCache()
			if get(t, ufs, "/D/x") != "new" || get(t, ufs, "/D/sub/f") != "hidden f" {
				t.Fatal("contents")
			}
		})
		// /D emptied and remade, then a file created in it through /d:
		// the cached /d still shows the lower directory, which the new
		// one hides.
		t.Run(kind+"/opaque-create", func(t *testing.T) {
			ufs, upper := staleUnion(t, kind)
			must(t, ufs.RemoveAll("/D"))
			must(t, ufs.Mkdir("/D", 0755))
			_, err := ufs.Create("/d/new")
			if !errors.Is(err, syscall.ESTALE) {
				t.Fatalf("Create: %v, want ESTALE", err)
			}
			put(t, ufs, "/d/new", "new")
			wantNames(t, list(t, ufs, "/d"), "new")
			wantNames(t, list(t, upper, "/d"), OpaqueWhiteout, "new")
		})
		// A file made through the other spelling shadows the cached lower
		// one: a copy-up of that one would fail on it, or replace it.
		t.Run(kind+"/shadowed", func(t *testing.T) {
			ufs, _ := staleUnion(t, kind)
			put(t, ufs, "/D/X", "upper")
			if err := ufs.Truncate("/d/x", 1); !errors.Is(err, syscall.ESTALE) {
				t.Fatalf("Truncate: %v, want ESTALE", err)
			}
			must(t, ufs.Truncate("/d/x", 1))
			if get(t, ufs, "/D/x") != "u" {
				t.Fatal("contents")
			}
		})
		// A metadata marker made through the other spelling: the cached
		// file lacks it, and a copy-up would lose the change.
		t.Run(kind+"/marker", func(t *testing.T) {
			ufs, _ := staleUnion(t, kind)
			must(t, ufs.Chmod("/D/m", 0600))
			if err := ufs.Truncate("/d/m", 1); !errors.Is(err, syscall.ESTALE) {
				t.Fatalf("Truncate: %v, want ESTALE", err)
			}
			// Retried, it acts on the merged view as it is.
			must(t, ufs.Truncate("/d/m", 1))
			wantMode(t, "/d/m", stat(t, ufs, "/d/m"), 0600)
			if get(t, ufs, "/D/m") != "m" {
				t.Fatal("contents")
			}
		})
	}
}

// The check of a cached node against the layers reports a layer that
// fails, and empties the cache.
func TestLookupCacheCurrentFails(t *testing.T) {
	upper := newFaultFS(t)
	lower := mustNewMemFS().(absfs.SymlinkFileSystem)
	mustMkdirAll(t, lower, "/d")
	put(t, lower, "/d/x", "x")
	ufs := New(WithWritableLayer(upper), WithReadOnlyLayer(lower), WithLookupCache(time.Hour, 0))
	lstat(t, ufs, "/d/x")
	upper.failOn("lstat", opaquePath("/"))
	wantInjected(t, "Truncate", ufs.Truncate("/d/x", 0))
	if n := ufs.CacheStats().LookupEntries; n != 0 {
		t.Fatalf("%d entries cached", n)
	}
	upper.clear()
	must(t, ufs.Truncate("/d/x", 0))
}

// copyUpDir joins a directory the writable layer has, and nothing else,
// even for a node that says the layer has nothing there.
func TestCopyUpDirJoinsOnlyDirectories(t *testing.T) {
	ufs, upper, lower := newTestUnion(t)
	mustMkdirAll(t, lower, "/d")
	put(t, upper, "/d", "file")
	r, err := ufs.root()
	must(t, err)
	n := &node{path: "/d", info: stat(t, lower, "/d"), layers: []int{1}, parent: r}
	if err := ufs.copyUpDir(n); !errors.Is(err, syscall.ENOTDIR) {
		t.Fatalf("copyUpDir: %v, want ENOTDIR", err)
	}
}

// TestRenameDirDropsLegacyFoldedNames pins the behaviour change for a
// writable layer from before v1.1.0 that holds a client's own `.WH.` name
// on a host that does not fold case: such a name no longer resolves, and
// renaming a directory lower layers contribute to drops it from the moved
// copy along with the union's markers (as removing a directory does).
// Kept, it would hide a name of the moved directory on a host that folds.
func TestRenameDirDropsLegacyFoldedNames(t *testing.T) {
	ufs, upper, lower := newTestUnion(t)
	mustMkdirAll(t, lower, "/dir")
	put(t, lower, "/dir/a", "a")
	mustMkdirAll(t, upper, "/dir/.Wh.sub")
	put(t, upper, "/dir/.WH.notes", "legacy")
	put(t, upper, "/dir/.Wh.sub/f", "f")
	put(t, upper, "/dir/b", "b")
	wantNotExist(t, ufs, "/dir/.WH.notes")
	wantNames(t, list(t, ufs, "/dir"), "a", "b")

	must(t, ufs.Rename("/dir", "/dir2"))
	wantNames(t, list(t, upper, "/dir2"), "a", "b")
	wantNames(t, list(t, ufs, "/dir2"), "a", "b")
}

func TestShortNameShape(t *testing.T) {
	for name, want := range map[string]bool{
		"WH~1.X": true, "wh~1.x": true, "WH~12": true, "WHA1B2~1.TXT": true,
		"~1": true, "A~1.": true,
		"WH~.X": false, "WH~1A.X": false, "WH1.X": false, "LONGNAME~1.X": false,
		"WH~1.TEXT": false, "WH~1.X.Y": false, ".WH~1": false, "": false,
		"foo.txt~": false, "x": false,
	} {
		if got := shortNameShape(name); got != want {
			t.Errorf("shortNameShape(%q) = %v, want %v", name, got, want)
		}
	}
}

// Where 8.3 names are on (Windows, by default on system volumes), NTFS
// gives ".wh.x" the short name "WH~1.X" too. No such alias of the union's
// markers may reach them; newShortFS simulates the host on memfs, and
// TestShortNameAliasOfMarker runs the same on a Windows host directory.
func TestShortNameAliasesRefused(t *testing.T) {
	withCache(t, "memfs", func(t *testing.T, cache []Option) {
		upper := newShortFS(t)
		lower := mustNewMemFS().(absfs.SymlinkFileSystem)
		for _, n := range []string{"/x", "/y", "/z"} {
			put(t, lower, n, "lower")
		}
		mustMkdirAll(t, lower, "/dir/inner")
		ufs := New(append([]Option{WithWritableLayer(upper), WithReadOnlyLayer(lower)}, cache...)...)
		for _, n := range []string{"/x", "/y", "/z", "/dir"} {
			must(t, ufs.RemoveAll(n))
		}
		must(t, ufs.Mkdir("/dir", 0755)) // opaque
		put(t, ufs, "/ab~1.txt", "literal")
		put(t, ufs, "/new", "new")

		intact := func(what string, err error) {
			t.Helper()
			if !errors.Is(err, syscall.EPERM) {
				t.Errorf("%s: err = %v, want EPERM", what, err)
			}
			for _, n := range []string{"/x", "/y", "/z", "/dir/inner"} {
				wantNotExist(t, ufs, n)
			}
			for _, n := range []string{"/.wh.x", "/.wh.y", "/.wh.z", "/dir/.wh.__dir_opaque"} {
				if info, err := upper.Lstat(n); err != nil || info.Size() != 0 {
					t.Errorf("%s: %v %v", n, err, info)
				}
			}
		}
		_, err := ufs.Lstat("/WH~1.X")
		intact("Lstat", err)
		_, err = ufs.Open("/wh~1.x")
		intact("Open", err)
		intact("Remove", ufs.Remove("/WH~1.X"))
		intact("RemoveAll", ufs.RemoveAll("/WH~1.Y"))
		intact("Remove opaque", ufs.Remove("/dir/WH~1.__D"))
		intact("Rename", ufs.Rename("/WH~1.Y", "/moved"))
		intact("Rename onto", ufs.Rename("/new", "/WH~1.Z"))
		_, err = ufs.OpenFile("/WH~1.Z", os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0644)
		intact("OpenFile O_CREATE", err)
		intact("Mkdir", ufs.Mkdir("/WH~1.Z", 0755))
		intact("Symlink", ufs.Symlink("/new", "/WH~1.Z"))
		intact("Chmod", ufs.Chmod("/WH~1.X", 0600))
		intact("Truncate", ufs.Truncate("/WH~1.X", 3))

		// A name of that shape that the layer has under that name is an
		// ordinary entry; an alias of an ordinary entry is refused too,
		// since the union cannot tell whose alias it is.
		if got := get(t, ufs, "/AB~1.TXT"); got != "literal" {
			t.Fatalf("/AB~1.TXT = %q", got)
		}
		must(t, ufs.Rename("/ab~1.txt", "/cd~2"))
		must(t, ufs.Remove("/cd~2"))
		put(t, ufs, "/anotherlong.txt", "upper")
		_, err = ufs.Lstat("/ANOTHE~1.TXT")
		if !errors.Is(err, syscall.EPERM) {
			t.Errorf("alias of an ordinary entry: %v", err)
		}
	})
}

// Under WithMarkersOnlyInWritableLayer a read-only layer's names are never
// markers, so its short-name aliases resolve as the host resolves them.
func TestShortNameAliasInLowerUnderMarkersOnly(t *testing.T) {
	lower := newShortFS(t)
	put(t, lower, "/longername.txt", "lower")
	ufs := New(WithWritableLayer(mustNewMemFS().(absfs.SymlinkFileSystem)), WithReadOnlyLayer(lower), WithMarkersOnlyInWritableLayer())
	if got := get(t, ufs, "/LONGER~1.TXT"); got != "lower" {
		t.Fatalf("/LONGER~1.TXT = %q", got)
	}
	ro := New(WithReadOnlyLayer(lower))
	if _, err := ro.Lstat("/LONGER~1.TXT"); !errors.Is(err, syscall.EPERM) {
		t.Fatalf("default, read-only layer alias: %v", err)
	}
}

func TestShortNameAliasListFails(t *testing.T) {
	upper := &failReadDir{foldFS: newShortFS(t)}
	put(t, upper, "/longername.txt", "x")
	ufs := New(WithWritableLayer(upper))
	upper.fail = true
	if _, err := ufs.Lstat("/LONGER~1.TXT"); !errors.Is(err, errInjected) {
		t.Fatalf("Lstat: %v, want the ReadDir failure", err)
	}
}

type failReadDir struct {
	*foldFS
	fail bool
}

func (f *failReadDir) ReadDir(p string) ([]fs.DirEntry, error) {
	if f.fail {
		return nil, errInjected
	}
	return f.foldFS.ReadDir(p)
}
