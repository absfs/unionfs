package unionfs

import (
	"errors"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/absfs/absfs"
	"github.com/absfs/basefs"
	"github.com/absfs/memfs"
	"github.com/absfs/osfs"
)

// The union's own bookkeeping (whiteouts, opaque markers, metadata markers,
// copy-up targets and the directories it keeps) must never act through a
// symbolic link in the writable layer: a host follows a link on open,
// chmod, chown and chtimes, so a link at such a path would carry the change
// to wherever it points, outside the union's view and, over a host
// directory, outside the layer. These tests plant links where the union
// writes, directly in the writable layer, and check that what they point at
// is left alone.

const canaryData = "canary"

var canaryTime = time.Date(2002, 3, 4, 5, 6, 7, 0, time.UTC)

// putCanary writes the canary file p to fs: canaryData, mode 0600,
// modification time canaryTime.
func putCanary(t *testing.T, fs absfs.FileSystem, p string) {
	t.Helper()
	put(t, fs, p, canaryData)
	must(t, fs.Chmod(p, 0600))
	must(t, fs.Chtimes(p, canaryTime, canaryTime))
}

// wantCanary fails unless the canary file p in fs is as putCanary left it,
// with owner uid:gid when ok.
func wantCanary(t *testing.T, fs absfs.SymlinkFileSystem, p string, uid, gid int, ok bool) {
	t.Helper()
	info, err := fs.Lstat(p)
	if err != nil {
		t.Fatalf("canary %s: %v", p, err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0600 || !info.ModTime().Equal(canaryTime) {
		t.Errorf("canary %s changed: %v %v", p, info.Mode(), info.ModTime())
	}
	if got := get(t, fs, p); got != canaryData {
		t.Errorf("canary %s contents %q", p, got)
	}
	if u, g, has := ownerOf(info); ok && has && (u != uid || g != gid) {
		t.Errorf("canary %s owner %d:%d, want %d:%d", p, u, g, uid, gid)
	}
}

func canaryOwner(t *testing.T, fs absfs.SymlinkFileSystem, p string) (int, int, bool) {
	t.Helper()
	info, err := fs.Lstat(p)
	must(t, err)
	return ownerOf(info)
}

// wantExist fails unless err reports an existing entry.
func wantExist(t *testing.T, what string, err error) {
	t.Helper()
	if !errors.Is(err, os.ErrExist) {
		t.Fatalf("%s: err = %v, want EEXIST", what, err)
	}
}

// A symbolic link at a metadata marker's path is not a marker: the file
// keeps its own metadata, and a change to it fails rather than reaching the
// link's target.
func TestMetaMarkerLinkNotFollowed(t *testing.T) {
	for _, kind := range layerKinds {
		for _, op := range []string{"chmod", "chown", "chtimes", "lchown"} {
			t.Run(kind+"/"+op, func(t *testing.T) {
				ufs, upper, _ := metaUnion(t, kind)
				putCanary(t, upper, "/canary")
				uid, gid, ok := canaryOwner(t, upper, "/canary")
				must(t, upper.Mkdir("/d", 0755))
				symlink(t, upper, "/canary", MetaMarkerName("/d/a"))

				if info := lstat(t, ufs, "/d/a"); info.Mode() != 0644 || !info.ModTime().Equal(lowerTime) {
					t.Fatalf("Lstat took the link for a marker: %v %v", info.Mode(), info.ModTime())
				}
				if info := listInfo(t, ufs, "/d", "a"); info.Mode() != 0644 {
					t.Fatalf("ReadDir took the link for a marker: %v", info.Mode())
				}
				var err error
				switch op {
				case "chmod":
					err = ufs.Chmod("/d/a", 0777)
				case "chown":
					err = ufs.Chown("/d/a", os.Getuid(), os.Getgid())
				case "chtimes":
					err = ufs.Chtimes("/d/a", time.Now(), time.Now())
				case "lchown":
					err = ufs.Lchown("/d/a", os.Getuid(), os.Getgid())
				}
				wantExist(t, op, err)
				wantCanary(t, upper, "/canary", uid, gid, ok)
				if info := lstat(t, upper, MetaMarkerName("/d/a")); !isSymlink(info) {
					t.Fatalf("the link was replaced: %v", info.Mode())
				}
			})
		}
	}
}

// A whiteout or opaque marker that a link already occupies counts as the
// marker: Remove and Rename leave the link as it is, and never open it.
func TestMarkerLinkNotOpened(t *testing.T) {
	for _, kind := range layerKinds {
		t.Run(kind+"/remove", func(t *testing.T) {
			ufs, upper, _ := metaUnion(t, kind)
			putCanary(t, upper, "/canary")
			must(t, ufs.Truncate("/d/a", 1)) // copy /d/a up
			symlink(t, upper, "/canary", WhiteoutName("/d/a"))
			must(t, ufs.Remove("/d/a"))
			wantNotExist(t, ufs, "/d/a")
			wantCanary(t, upper, "/canary", 0, 0, false)
		})
		t.Run(kind+"/rename", func(t *testing.T) {
			ufs, upper, _ := metaUnion(t, kind)
			putCanary(t, upper, "/canary")
			must(t, ufs.Truncate("/d/a", 1))
			symlink(t, upper, "/canary", WhiteoutName("/d/a"))
			must(t, ufs.Rename("/d/a", "/d/z"))
			wantNotExist(t, ufs, "/d/a")
			if get(t, ufs, "/d/z") != "l" {
				t.Fatal("renamed contents")
			}
			wantCanary(t, upper, "/canary", 0, 0, false)
		})
		t.Run(kind+"/opaque", func(t *testing.T) {
			ufs, upper, _ := metaUnion(t, kind)
			putCanary(t, upper, "/canary")
			must(t, ufs.Remove("/top"))
			must(t, ufs.Mkdir("/s", 0755))
			put(t, ufs, "/s/f", "sf")
			symlink(t, upper, "/canary", opaquePath("/s"))
			// A lower layer has /top, so the moved directory is made
			// opaque, where the link already is.
			must(t, ufs.Rename("/s", "/top"))
			wantNames(t, list(t, ufs, "/top"), "f")
			wantCanary(t, upper, "/canary", 0, 0, false)
		})
	}
}

// Links that appear in the writable layer under paths the union has cached
// as something else are refused, not written through. With the lookup cache
// on, the union resolves cached directories without asking the layer again.
func TestStaleLinkNotFollowed(t *testing.T) {
	for _, kind := range layerKinds {
		cached := func(t *testing.T) (*UnionFS, absfs.SymlinkFileSystem) {
			_, upper, lower := metaUnion(t, kind)
			putCanary(t, upper, "/canary")
			must(t, upper.Mkdir("/cdir", 0700))
			return New(WithWritableLayer(upper), WithReadOnlyLayer(lower), WithLookupCache(time.Hour, 0)), upper
		}
		wantCdirEmpty := func(t *testing.T, upper absfs.SymlinkFileSystem) {
			t.Helper()
			ents, err := upper.ReadDir("/cdir")
			must(t, err)
			if len(ents) != 0 {
				t.Fatalf("written through the link: %d entries in /cdir", len(ents))
			}
		}

		// A directory only a lower layer had, cached so, that the
		// writable layer now has as a link: copying it up must not join
		// the link.
		t.Run(kind+"/lower-dir", func(t *testing.T) {
			ufs, upper := cached(t)
			lstat(t, ufs, "/d/a")
			symlink(t, upper, "/cdir", "/d")
			if err := ufs.Chmod("/d/a", 0600); !errors.Is(err, syscall.ENOTDIR) {
				t.Fatalf("Chmod: %v, want ENOTDIR", err)
			}
			wantCdirEmpty(t, upper)
		})
		// A directory the writable layer had, cached so, that it now has
		// as a link: bookkeeping in it must not go through the link.
		t.Run(kind+"/upper-dir", func(t *testing.T) {
			ufs, upper := cached(t)
			must(t, ufs.Chmod("/d/b", 0600)) // copies /d up
			lstat(t, ufs, "/d/a")
			must(t, upper.RemoveAll("/d"))
			symlink(t, upper, "/cdir", "/d")
			if err := ufs.Chmod("/d/a", 0600); !errors.Is(err, syscall.ENOTDIR) {
				t.Fatalf("Chmod: %v, want ENOTDIR", err)
			}
			wantCdirEmpty(t, upper)
		})
		// A file only a lower layer had, cached so, that the writable
		// layer now has as a link: copying it up must not open the link.
		// The cached file is no longer what the layers have there.
		t.Run(kind+"/copy-up", func(t *testing.T) {
			ufs, upper := cached(t)
			must(t, ufs.Chmod("/d/b", 0600))
			lstat(t, ufs, "/d/a")
			symlink(t, upper, "/canary", "/d/a")
			if err := ufs.Truncate("/d/a", 0); !errors.Is(err, syscall.ESTALE) {
				t.Fatalf("Truncate: %v, want ESTALE", err)
			}
			wantCanary(t, upper, "/canary", 0, 0, false)
		})
		// A metadata marker, cached as the file's, that is now a link:
		// it is no longer the file's marker.
		t.Run(kind+"/marker", func(t *testing.T) {
			ufs, upper := cached(t)
			must(t, ufs.Chmod("/d/a", 0640))
			if info := lstat(t, ufs, "/d/a"); info.Mode() != 0640 {
				t.Fatalf("marker mode %v", info.Mode())
			}
			must(t, upper.Remove(MetaMarkerName("/d/a")))
			symlink(t, upper, "/canary", MetaMarkerName("/d/a"))
			if info := lstat(t, ufs, "/d/a"); info.Mode() != 0644 {
				t.Fatalf("a link taken for the marker: %v", info.Mode())
			}
			wantExist(t, "Chmod", ufs.Chmod("/d/a", 0777))
			wantCanary(t, upper, "/canary", 0, 0, false)
		})
	}
}

// noLchownFS is a writable layer with symbolic links but without Lchown.
type noLchownFS struct {
	absfs.FileSystem
	links absfs.SymlinkFileSystem
}

func (f noLchownFS) Lstat(p string) (os.FileInfo, error) { return f.links.Lstat(p) }
func (f noLchownFS) Readlink(p string) (string, error)   { return f.links.Readlink(p) }
func (f noLchownFS) Symlink(o, n string) error           { return f.links.Symlink(o, n) }

// Without Lchown in the writable layer, Lchown of a link fails: Chown
// would follow the link inside the layer, not through the union.
func TestLchownWithoutLayerLchown(t *testing.T) {
	upperMem, err := memfs.NewFS()
	must(t, err)
	lowerMem, err := memfs.NewFS()
	must(t, err)
	lowerTree(t, lowerMem)
	putCanary(t, upperMem, "/canary")
	must(t, upperMem.Chown("/canary", 11, 22))
	symlink(t, lowerMem, "/canary", "/ln")
	ufs := New(WithWritableLayer(noLchownFS{upperMem, upperMem}), WithReadOnlyLayer(lowerMem))
	must(t, ufs.Symlink("/canary", "/l"))

	for _, p := range []string{"/l", "/ln"} {
		if err := ufs.Lchown(p, 33, 44); !errors.Is(err, errors.ErrUnsupported) {
			t.Fatalf("Lchown %s: %v, want ErrUnsupported", p, err)
		}
	}
	wantCanary(t, upperMem, "/canary", 11, 22, true)
	// Anything else still gets Chown.
	must(t, ufs.Lchown("/top", 33, 44))
	if uid, gid, _ := ownerOf(lstat(t, ufs, "/top")); uid != 33 || gid != 44 {
		t.Fatalf("Lchown of a file: %d:%d", uid, gid)
	}
}

// caseFolds reports whether the host directory dir folds the case of
// names.
func caseFolds(t *testing.T, dir string) bool {
	t.Helper()
	p := filepath.Join(dir, "casefold-probe")
	must(t, os.WriteFile(p, nil, 0600))
	defer os.Remove(p)
	_, err := os.Lstat(filepath.Join(dir, "CASEFOLD-PROBE"))
	return err == nil
}

// On a host that folds case (macOS and Windows by default), ".WH.x" is a
// name the host takes for the marker ".wh.x". The union refuses to create
// such a name, but one can already be there: made by an older version that
// allowed it, or by a client of the layer's directory. A link there whose
// relative target climbs out of the layer (which basefs documents it cannot
// prevent) must not get the union's bookkeeping to follow it outside the
// layer.
//
// Host-directory layers are exercised on Unix, so of the hosts that fold
// case this runs only on macOS; Windows is covered by the same code paths
// and by the memfs tests above, not by a test of its own.
func TestCaseFoldedMarkerLinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("host-directory layers are exercised on Unix")
	}
	gids, _ := os.Getgroups()
	for _, tc := range []struct {
		name   string
		marker string // the marker the planted link stands for
		run    func(ufs *UnionFS) error
	}{
		{"chmod", MetaMarkerName("/a"), func(ufs *UnionFS) error { return ufs.Chmod("/a", 0777) }},
		{"chtimes", MetaMarkerName("/a"), func(ufs *UnionFS) error { return ufs.Chtimes("/a", time.Now(), time.Now()) }},
		{"chown", MetaMarkerName("/a"), func(ufs *UnionFS) error {
			for _, g := range gids {
				if err := ufs.Chown("/a", -1, g); err != nil {
					return err
				}
			}
			return nil
		}},
		{"remove", WhiteoutName("/b"), func(ufs *UnionFS) error { return ufs.Remove("/b") }},
		{"rename", WhiteoutName("/b"), func(ufs *UnionFS) error { return ufs.Rename("/b", "/c") }},
		{"rename-dir", opaquePath("/s"), func(ufs *UnionFS) error { return ufs.Rename("/s", "/ldir") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, err := filepath.EvalSymlinks(t.TempDir())
			must(t, err)
			if !caseFolds(t, root) {
				t.Skip("the host does not fold case")
			}
			dirs := map[string]string{}
			for _, d := range []string{"upper", "lower", "outside"} {
				dirs[d] = filepath.Join(root, d)
				must(t, os.Mkdir(dirs[d], 0755))
			}
			canary := filepath.Join(dirs["outside"], "canary")
			must(t, os.WriteFile(canary, []byte(canaryData), 0600))
			must(t, os.Chtimes(canary, canaryTime, canaryTime))
			before, err := os.Stat(canary)
			must(t, err)
			must(t, os.WriteFile(filepath.Join(dirs["lower"], "a"), []byte("la"), 0644))
			must(t, os.WriteFile(filepath.Join(dirs["lower"], "b"), []byte("lb"), 0644))
			must(t, os.Mkdir(filepath.Join(dirs["lower"], "ldir"), 0755))
			host, err := osfs.NewFS()
			must(t, err)
			upper, err := basefs.NewFS(host, dirs["upper"])
			must(t, err)
			lower, err := basefs.NewFS(host, dirs["lower"])
			must(t, err)
			ufs := New(WithWritableLayer(upper), WithReadOnlyLayer(lower))

			must(t, ufs.Truncate("/b", 1)) // copy /b up
			must(t, ufs.Mkdir("/s", 0755))
			// The link's name: the marker's, with its prefix upper-cased.
			dir, base := path.Split(tc.marker)
			name := dir + strings.ToUpper(WhiteoutPrefix) + strings.TrimPrefix(base, WhiteoutPrefix)
			// The link's target climbs out of the layer from where it is.
			depth := strings.Count(path.Clean(dir), "/")
			if dir == "/" {
				depth = 0
			}
			target := strings.Repeat("../", depth+1) + "outside/canary"
			if err := ufs.Symlink(target, name); !errors.Is(err, syscall.EPERM) {
				t.Fatalf("Symlink at %s: %v, want EPERM", name, err)
			}
			must(t, os.Symlink(target, filepath.Join(dirs["upper"], filepath.FromSlash(name))))
			if real, err := filepath.EvalSymlinks(filepath.Join(dirs["upper"], name)); err != nil || real != canary {
				t.Fatalf("planted link resolves to %q, %v", real, err)
			}

			err = tc.run(ufs)
			t.Logf("%s: %v", tc.name, err)
			after, err := os.Stat(canary)
			must(t, err)
			data, err := os.ReadFile(canary)
			must(t, err)
			if string(data) != canaryData || after.Mode() != before.Mode() || !after.ModTime().Equal(before.ModTime()) {
				t.Fatalf("canary outside the layer changed: %q %v %v", data, after.Mode(), after.ModTime())
			}
			bu, bg, _ := hostOwner(before.Sys())
			au, ag, _ := hostOwner(after.Sys())
			if bu != au || bg != ag {
				t.Fatalf("canary owner changed: %d:%d -> %d:%d", bu, bg, au, ag)
			}
		})
	}
}
