package unionfs

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"sync"
	"testing"
	"time"

	"github.com/absfs/absfs"
	"github.com/absfs/basefs"
	"github.com/absfs/inode"
	"github.com/absfs/memfs"
	"github.com/absfs/osfs"
)

// layerKinds are the layer stacks the metadata-marker tests run on: memfs
// layers, and host directories (basefs over osfs) as C4's NFS workspace
// prototype composes them.
var layerKinds = []string{"memfs", "osfs"}

// metaUnion returns a union of a fresh upper over a fresh lower of the given
// kind, both layers, and a lower holding /d/a ("la", 0644), /d/b ("lb") and
// /d/sub/c, all with modification time lowerTime.
func metaUnion(t *testing.T, kind string) (*UnionFS, absfs.SymlinkFileSystem, absfs.SymlinkFileSystem) {
	t.Helper()
	var upper, lower absfs.SymlinkFileSystem
	switch kind {
	case "memfs":
		u, err := memfs.NewFS()
		must(t, err)
		l, err := memfs.NewFS()
		must(t, err)
		upper, lower = u, l
	case "osfs":
		if runtime.GOOS == "windows" {
			t.Skip("host-directory layers are exercised on Unix")
		}
		host, err := osfs.NewFS()
		must(t, err)
		root, err := filepath.EvalSymlinks(t.TempDir())
		must(t, err)
		for _, d := range []string{"upper", "lower"} {
			must(t, os.Mkdir(filepath.Join(root, d), 0755))
		}
		u, err := basefs.NewFS(host, filepath.Join(root, "upper"))
		must(t, err)
		l, err := basefs.NewFS(host, filepath.Join(root, "lower"))
		must(t, err)
		upper, lower = u, l
	}
	lowerTree(t, lower)
	for _, p := range []string{"/d/a", "/d/b", "/d/sub/c", "/d/sub", "/d"} {
		if !isDirIn(lower, p) {
			must(t, lower.Chmod(p, 0644))
		}
		must(t, lower.Chtimes(p, lowerTime, lowerTime))
	}
	return New(WithWritableLayer(upper), WithReadOnlyLayer(lower)), upper, lower
}

// lowerTime is the modification time of everything metaUnion's lower has.
var lowerTime = time.Date(2021, 3, 4, 5, 6, 7, 0, time.UTC)

func isDirIn(fs absfs.SymlinkFileSystem, p string) bool {
	info, err := fs.Lstat(p)
	return err == nil && info.IsDir()
}

// upperContents lists every path in the upper ("/" after directories),
// sorted, and the total size of its regular files.
func upperContents(t *testing.T, upper absfs.FileSystem) ([]string, int64) {
	t.Helper()
	var out []string
	var size int64
	var walk func(dir string)
	walk = func(dir string) {
		ents, err := upper.ReadDir(dir)
		must(t, err)
		for _, e := range ents {
			p := path.Join(dir, e.Name())
			info, err := e.Info()
			must(t, err)
			switch {
			case e.IsDir():
				out = append(out, p+"/")
				walk(p)
			default:
				out = append(out, p)
				if info.Mode().IsRegular() {
					size += info.Size()
				}
			}
		}
	}
	walk("/")
	sort.Strings(out)
	if out == nil {
		out = []string{}
	}
	return out, size
}

func wantMode(t *testing.T, what string, info os.FileInfo, mode os.FileMode) {
	t.Helper()
	if info.Mode() != mode {
		t.Fatalf("%s: mode %v, want %v", what, info.Mode(), mode)
	}
}

func lstat(t *testing.T, fs absfs.SymlinkFileSystem, p string) os.FileInfo {
	t.Helper()
	info, err := fs.Lstat(p)
	must(t, err)
	return info
}

func listInfo(t *testing.T, ufs *UnionFS, dir, name string) os.FileInfo {
	t.Helper()
	ents, err := ufs.ReadDir(dir)
	must(t, err)
	for _, e := range ents {
		if e.Name() == name {
			info, err := e.Info()
			must(t, err)
			return info
		}
	}
	t.Fatalf("%s not listed in %s", name, dir)
	return nil
}

func TestMetaNames(t *testing.T) {
	for _, tc := range []struct {
		name, target string
		ok           bool
	}{
		{".wh..wh.meta.c", "c", true},
		{"a/b/.wh..wh.meta.c", "a/b/c", true},
		{"/a/.wh..wh.meta..hidden", "/a/.hidden", true},
		{".wh..wh.meta.", "", false},
		{".wh.c", "", false},
		{".wh..wh.unionfs.1", "", false},
		{"c", "", false},
		{"a/.wh..wh.meta.c/d", "", false},
	} {
		got, ok := MetaTarget(tc.name)
		if got != tc.target || ok != tc.ok {
			t.Errorf("MetaTarget(%q) = %q, %v; want %q, %v", tc.name, got, ok, tc.target, tc.ok)
		}
		if IsMetaMarker(tc.name) != tc.ok {
			t.Errorf("IsMetaMarker(%q) = %v", tc.name, !tc.ok)
		}
		if tc.ok {
			if MetaMarkerName(tc.target) != tc.name {
				t.Errorf("MetaMarkerName(%q) = %q, want %q", tc.target, MetaMarkerName(tc.target), tc.name)
			}
			if !IsWhiteout(tc.name) {
				t.Errorf("IsWhiteout(%q) = false: metadata markers are reserved names", tc.name)
			}
			if _, ok := WhiteoutTarget(tc.name); ok {
				t.Errorf("WhiteoutTarget(%q) ok: a metadata marker deletes nothing", tc.name)
			}
		}
	}
}

// chmod +x and -x of a lower file leave its data in the lower: the upper
// gets one marker whose own mode is the file's, and the union reports it.
func TestMetaChmod(t *testing.T) {
	for _, kind := range layerKinds {
		t.Run(kind, func(t *testing.T) {
			ufs, upper, lower := metaUnion(t, kind)
			dirM := lstat(t, lower, "/d").ModTime()
			must(t, ufs.Chmod("/d/a", 0755))
			names, size := upperContents(t, upper)
			wantNames(t, names, "/d/", "/d/.wh..wh.meta.a")
			if size != 0 {
				t.Fatalf("upper holds %d bytes", size)
			}
			wantMode(t, "marker", lstat(t, upper, "/d/.wh..wh.meta.a"), 0755)
			if !lstat(t, upper, "/d").ModTime().Equal(dirM) {
				t.Fatal("writing the marker changed the directory's mtime")
			}
			for _, info := range []os.FileInfo{lstat(t, ufs, "/d/a"), stat(t, ufs, "/d/a"), listInfo(t, ufs, "/d", "a")} {
				wantMode(t, "union", info, 0755)
				if info.Size() != 2 || !info.ModTime().Equal(lowerTime) || info.Name() != "a" {
					t.Fatalf("union info %v %v %v", info.Size(), info.ModTime(), info.Name())
				}
			}
			if get(t, ufs, "/d/a") != "la" {
				t.Fatal("contents")
			}
			wantNames(t, list(t, ufs, "/d"), "a", "b", "sub")
			if _, err := ufs.Lstat("/d/.wh..wh.meta.a"); !os.IsNotExist(err) {
				t.Fatalf("marker visible: %v", err)
			}

			must(t, ufs.Chmod("/d/a", 0644))
			wantNames(t, names, "/d/", "/d/.wh..wh.meta.a")
			wantMode(t, "marker", lstat(t, upper, "/d/.wh..wh.meta.a"), 0644)
			wantMode(t, "union", stat(t, ufs, "/d/a"), 0644)
			wantMode(t, "lower", lstat(t, lower, "/d/a"), 0644)

			// The full permission bits, setuid and sticky too, and a
			// mode that leaves the owner no access.
			for _, m := range []os.FileMode{0700, 0000, 0751 | os.ModeSetuid, 0640 | os.ModeSticky} {
				if m&os.ModeSticky != 0 && kind == "osfs" {
					continue // hosts may refuse or drop the sticky bit on files
				}
				must(t, ufs.Chmod("/d/a", m))
				wantMode(t, "union", stat(t, ufs, "/d/a"), m)
			}
		})
	}
}

func stat(t *testing.T, fs absfs.FileSystem, p string) os.FileInfo {
	t.Helper()
	info, err := fs.Stat(p)
	must(t, err)
	return info
}

// Chtimes with a new mtime writes the marker only; repeated changes of
// every kind update the one marker.
func TestMetaChtimesAndRepeats(t *testing.T) {
	for _, kind := range layerKinds {
		t.Run(kind, func(t *testing.T) {
			ufs, upper, _ := metaUnion(t, kind)
			m1 := time.Date(2022, 1, 1, 0, 0, 0, 0, time.UTC)
			must(t, ufs.Chtimes("/d/sub/c", m1, m1))
			names, size := upperContents(t, upper)
			wantNames(t, names, "/d/", "/d/sub/", "/d/sub/.wh..wh.meta.c")
			if size != 0 {
				t.Fatalf("upper holds %d bytes", size)
			}
			if !lstat(t, upper, "/d/sub/.wh..wh.meta.c").ModTime().Equal(m1) {
				t.Fatal("marker mtime")
			}
			wantMode(t, "marker keeps the lower mode", lstat(t, upper, "/d/sub/.wh..wh.meta.c"), 0644)

			// An access-time-only change of the marked file writes nothing.
			must(t, ufs.Chtimes("/d/sub/c", time.Now(), m1))
			must(t, ufs.Chtimes("/d/sub/c", time.Now(), time.Time{}))

			m2 := m1.Add(time.Hour)
			must(t, ufs.Chmod("/d/sub/c", 0600))
			must(t, ufs.Chtimes("/d/sub/c", m2, m2))
			must(t, ufs.Chmod("/d/sub/c", 0755))
			names, size = upperContents(t, upper)
			wantNames(t, names, "/d/", "/d/sub/", "/d/sub/.wh..wh.meta.c")
			if size != 0 {
				t.Fatalf("upper holds %d bytes", size)
			}
			info := stat(t, ufs, "/d/sub/c")
			wantMode(t, "union", info, 0755)
			if !info.ModTime().Equal(m2) {
				t.Fatalf("mtime %v, want %v", info.ModTime(), m2)
			}
		})
	}
}

// The first write, truncate or open for writing copies the data up, applies
// the marker's metadata to the copy and removes the marker.
func TestMetaThenCopyUp(t *testing.T) {
	when := time.Date(2022, 6, 7, 8, 9, 10, 0, time.UTC)
	for _, kind := range layerKinds {
		for _, tc := range []struct {
			name     string
			change   func(*UnionFS) error
			contents string
			keepTime bool // the change leaves the mtime alone
		}{
			{"Write", func(u *UnionFS) error {
				f, err := u.OpenFile("/d/a", os.O_WRONLY, 0)
				if err != nil {
					return err
				}
				if _, err := f.Write([]byte("X")); err != nil {
					return err
				}
				return f.Close()
			}, "Xa", false},
			{"Truncate", func(u *UnionFS) error { return u.Truncate("/d/a", 1) }, "l", false},
			{"TruncateZero", func(u *UnionFS) error { return u.Truncate("/d/a", 0) }, "", false},
			{"OpenTrunc", func(u *UnionFS) error {
				f, err := u.OpenFile("/d/a", os.O_WRONLY|os.O_TRUNC, 0)
				if err != nil {
					return err
				}
				return f.Close()
			}, "", false},
			// Opening for writing copies up even without a write, as it
			// always has: the handle writes to the copy.
			{"OpenRDWRNoWrite", func(u *UnionFS) error {
				f, err := u.OpenFile("/d/a", os.O_RDWR, 0)
				if err != nil {
					return err
				}
				return f.Close()
			}, "la", true},
		} {
			t.Run(kind+"/"+tc.name, func(t *testing.T) {
				ufs, upper, lower := metaUnion(t, kind)
				must(t, ufs.Chmod("/d/a", 0751))
				must(t, ufs.Chtimes("/d/a", when, when))
				must(t, tc.change(ufs))
				names, _ := upperContents(t, upper)
				wantNames(t, names, "/d/", "/d/a")
				info := lstat(t, upper, "/d/a")
				wantMode(t, "copy", info, 0751)
				if tc.keepTime && !info.ModTime().Equal(when) {
					t.Fatalf("copy mtime %v, want the marker's %v", info.ModTime(), when)
				}
				if s := get(t, ufs, "/d/a"); s != tc.contents {
					t.Fatalf("contents %q, want %q", s, tc.contents)
				}
				if get(t, lower, "/d/a") != "la" {
					t.Fatal("lower changed")
				}
				wantMode(t, "union", stat(t, ufs, "/d/a"), 0751)
			})
		}
	}
}

// Remove deletes the marker and writes the whiteout; Rename copies the data
// up first and leaves no marker at either name.
func TestMetaRemoveAndRename(t *testing.T) {
	when := time.Date(2022, 6, 7, 8, 9, 10, 0, time.UTC)
	for _, kind := range layerKinds {
		t.Run(kind+"/Remove", func(t *testing.T) {
			ufs, upper, _ := metaUnion(t, kind)
			must(t, ufs.Chmod("/d/a", 0755))
			must(t, ufs.Remove("/d/a"))
			names, _ := upperContents(t, upper)
			wantNames(t, names, "/d/", "/d/.wh.a")
			wantNotExist(t, ufs, "/d/a")
			// Recreating it starts from nothing.
			put(t, ufs, "/d/a", "new")
			wantMode(t, "recreated", stat(t, ufs, "/d/a"), 0644)
		})
		t.Run(kind+"/RemoveAll", func(t *testing.T) {
			ufs, upper, _ := metaUnion(t, kind)
			must(t, ufs.Chmod("/d/sub/c", 0755))
			must(t, ufs.RemoveAll("/d/sub"))
			names, _ := upperContents(t, upper)
			wantNames(t, names, "/d/", "/d/.wh.sub")
		})
		t.Run(kind+"/Rename", func(t *testing.T) {
			ufs, upper, _ := metaUnion(t, kind)
			must(t, ufs.Chmod("/d/a", 0755))
			must(t, ufs.Chtimes("/d/a", when, when))
			must(t, ufs.Rename("/d/a", "/d/moved"))
			names, _ := upperContents(t, upper)
			wantNames(t, names, "/d/", "/d/.wh.a", "/d/moved")
			info := stat(t, ufs, "/d/moved")
			wantMode(t, "moved", info, 0755)
			if !info.ModTime().Equal(when) {
				t.Fatalf("moved mtime %v", info.ModTime())
			}
			if get(t, ufs, "/d/moved") != "la" {
				t.Fatal("contents")
			}
		})
		t.Run(kind+"/RenameOverMarked", func(t *testing.T) {
			ufs, upper, _ := metaUnion(t, kind)
			must(t, ufs.Chmod("/d/b", 0755))
			must(t, ufs.Rename("/d/a", "/d/b"))
			names, _ := upperContents(t, upper)
			wantNames(t, names, "/d/", "/d/.wh.a", "/d/b")
			wantMode(t, "replaced", stat(t, ufs, "/d/b"), 0644)
			if get(t, ufs, "/d/b") != "la" {
				t.Fatal("contents")
			}
		})
		t.Run(kind+"/RenameDirWithMarked", func(t *testing.T) {
			ufs, upper, _ := metaUnion(t, kind)
			must(t, ufs.Chmod("/d/sub/c", 0755))
			must(t, ufs.Rename("/d/sub", "/d/sub2"))
			names, _ := upperContents(t, upper)
			wantNames(t, names, "/d/", "/d/.wh.sub", "/d/sub2/", "/d/sub2/c")
			wantMode(t, "moved", stat(t, ufs, "/d/sub2/c"), 0755)
		})
	}
}

// The marker keeps applying when the lower file changes underneath it: the
// union reports the lower's new contents and size with the marker's
// metadata.
func TestMetaLowerChangesUnderneath(t *testing.T) {
	for _, kind := range layerKinds {
		t.Run(kind, func(t *testing.T) {
			ufs, _, lower := metaUnion(t, kind)
			must(t, ufs.Chmod("/d/a", 0700))
			put(t, lower, "/d/a", "a longer lower")
			info := stat(t, ufs, "/d/a")
			wantMode(t, "union", info, 0700)
			if info.Size() != int64(len("a longer lower")) {
				t.Fatalf("size %d", info.Size())
			}
			if get(t, ufs, "/d/a") != "a longer lower" {
				t.Fatal("contents")
			}
		})
	}
}

// With the Stat cache on, every metadata change and the copy-up after it
// show at once.
func TestMetaStatCacheCoherence(t *testing.T) {
	for _, kind := range layerKinds {
		t.Run(kind, func(t *testing.T) {
			_, upper, lower := metaUnion(t, kind)
			ufs := New(WithWritableLayer(upper), WithReadOnlyLayer(lower), WithStatCache(true, time.Hour))
			symlink(t, lower, "d/a", "/link")
			for _, p := range []string{"/d/a", "/link"} {
				wantMode(t, p, stat(t, ufs, p), 0644)
				_ = lstat(t, ufs, p)
			}
			must(t, ufs.Chmod("/d/a", 0755))
			wantMode(t, "Stat", stat(t, ufs, "/d/a"), 0755)
			wantMode(t, "Lstat", lstat(t, ufs, "/d/a"), 0755)
			when := time.Date(2023, 1, 1, 0, 0, 0, 0, time.UTC)
			must(t, ufs.Chtimes("/link", when, when))
			if !stat(t, ufs, "/link").ModTime().Equal(when) || !stat(t, ufs, "/d/a").ModTime().Equal(when) {
				t.Fatal("stale mtime after Chtimes")
			}
			must(t, ufs.Truncate("/d/a", 1))
			if info := stat(t, ufs, "/d/a"); info.Size() != 1 || info.Mode() != 0755 {
				t.Fatalf("after copy-up: %v %v", info.Size(), info.Mode())
			}
		})
	}
}

// git "freshens" a pack it would otherwise write by setting its times to
// now: on a lower pack that leaves one marker and no data in the upper.
func TestMetaGitFreshen(t *testing.T) {
	for _, kind := range layerKinds {
		t.Run(kind, func(t *testing.T) {
			ufs, upper, lower := metaUnion(t, kind)
			mustMkdirAll(t, lower, "/.git/objects/pack")
			pack := make([]byte, 1<<20)
			put(t, lower, "/.git/objects/pack/pack-1.pack", string(pack))
			must(t, lower.Chmod("/.git/objects/pack/pack-1.pack", 0444))
			now := time.Now().Truncate(time.Second).Add(time.Hour)
			must(t, ufs.Chtimes("/.git/objects/pack/pack-1.pack", now, now))
			names, size := upperContents(t, upper)
			wantNames(t, names, "/.git/", "/.git/objects/", "/.git/objects/pack/",
				"/.git/objects/pack/.wh..wh.meta.pack-1.pack")
			if size != 0 {
				t.Fatalf("upper holds %d bytes", size)
			}
			info := stat(t, ufs, "/.git/objects/pack/pack-1.pack")
			if !info.ModTime().Equal(now) || info.Size() != 1<<20 || info.Mode() != 0444 {
				t.Fatalf("freshened pack: %v %v %v", info.ModTime(), info.Size(), info.Mode())
			}
			// Freshening it again changes the same marker.
			must(t, ufs.Chtimes("/.git/objects/pack/pack-1.pack", now.Add(time.Hour), now.Add(time.Hour)))
			names, size = upperContents(t, upper)
			if len(names) != 4 || size != 0 {
				t.Fatalf("after a second freshen: %q, %d bytes", names, size)
			}
		})
	}
}

// The owner a Chown gives a lower file is the marker's, reported through
// Sys where the layer has one, and given to the copy.
func TestMetaChownMemfs(t *testing.T) {
	ufs, upper, _ := metaUnion(t, "memfs")
	must(t, ufs.Chown("/d/a", 1234, 5678))
	if exists(upper, "/d/a") {
		t.Fatal("Chown copied the file up")
	}
	info := stat(t, ufs, "/d/a")
	if uid, gid, ok := ownerOf(info); !ok || uid != 1234 || gid != 5678 {
		t.Fatalf("owner %d:%d %v", uid, gid, ok)
	}
	n := info.Sys().(*inode.Inode)
	if n.Size != 2 || n.Mode != 0644 || !n.Mtime().Equal(lowerTime) {
		t.Fatalf("Sys %v %v %v", n.Size, n.Mode, n.Mtime())
	}
	if uid, _, _ := ownerOf(listInfo(t, ufs, "/d", "a")); uid != 1234 {
		t.Fatalf("listed owner %d", uid)
	}
	must(t, ufs.Lchown("/d/a", 4321, 8765))
	if uid, gid, _ := ownerOf(lstat(t, ufs, "/d/a")); uid != 4321 || gid != 8765 {
		t.Fatalf("Lchown owner %d:%d", uid, gid)
	}
	must(t, ufs.Truncate("/d/a", 0))
	if uid, gid, _ := ownerOf(lstat(t, upper, "/d/a")); uid != 4321 || gid != 8765 {
		t.Fatalf("copy owner %d:%d", uid, gid)
	}
}

func TestMetaChownOsfs(t *testing.T) {
	ufs, upper, _ := metaUnion(t, "osfs")
	gid := os.Getgid()
	if groups, err := os.Getgroups(); err == nil {
		for _, g := range groups {
			if g != gid {
				gid = g
				break
			}
		}
	}
	must(t, ufs.Chown("/d/a", os.Getuid(), gid))
	names, size := upperContents(t, upper)
	wantNames(t, names, "/d/", "/d/.wh..wh.meta.a")
	if size != 0 {
		t.Fatalf("upper holds %d bytes", size)
	}
	for _, info := range []os.FileInfo{stat(t, ufs, "/d/a"), lstat(t, upper, "/d/.wh..wh.meta.a")} {
		if u, g, ok := ownerOf(info); !ok || u != os.Getuid() || g != gid {
			t.Fatalf("owner %d:%d %v, want %d:%d", u, g, ok, os.Getuid(), gid)
		}
	}
	wantMode(t, "Sys mode", stat(t, ufs, "/d/a"), 0644)
	must(t, ufs.Chmod("/d/a", 0750))
	if st, ok := hostMode(stat(t, ufs, "/d/a")); ok && st&0o7777 != 0o750 {
		t.Fatalf("Sys mode %o", st)
	}
	must(t, ufs.Truncate("/d/a", 0))
	if _, g, _ := ownerOf(lstat(t, upper, "/d/a")); g != gid {
		t.Fatalf("copy group %d, want %d", g, gid)
	}
}

// A file opened for reading reports the marker's metadata through its
// handle too.
func TestMetaOpenStat(t *testing.T) {
	for _, kind := range layerKinds {
		t.Run(kind, func(t *testing.T) {
			ufs, _, _ := metaUnion(t, kind)
			must(t, ufs.Chmod("/d/a", 0711))
			f, err := ufs.Open("/d/a")
			must(t, err)
			defer f.Close()
			info, err := f.Stat()
			must(t, err)
			wantMode(t, "handle", info, 0711)
			b, err := io.ReadAll(f)
			must(t, err)
			if string(b) != "la" {
				t.Fatalf("read %q", b)
			}
			must(t, f.Close())
			if _, err := f.Stat(); err == nil {
				t.Fatal("Stat of a closed handle succeeded")
			}
		})
	}
}

// Directories and symbolic links are copied up as before: copying them
// costs no data.
func TestMetaNotForDirsAndLinks(t *testing.T) {
	ufs, upper, lower := metaUnion(t, "memfs")
	symlink(t, lower, "a", "/d/ln")
	must(t, ufs.Chmod("/d/sub", 0700))
	must(t, ufs.Lchown("/d/ln", 0, 0))
	names, _ := upperContents(t, upper)
	wantNames(t, names, "/d/", "/d/ln", "/d/sub/")
}

// A marker in a read-only layer, as when a captured upper is stacked as a
// lower, applies to the file in the layers below it. A change through the
// union starts its own marker from that metadata.
func TestMetaStackedMarker(t *testing.T) {
	upper, mid, bottom := mustNewMemFS().(absfs.SymlinkFileSystem), mustNewMemFS().(absfs.SymlinkFileSystem), mustNewMemFS().(absfs.SymlinkFileSystem)
	lowerTree(t, bottom)
	mustMkdirAll(t, mid, "/d")
	put(t, mid, "/d/.wh..wh.meta.a", "")
	must(t, mid.Chmod("/d/.wh..wh.meta.a", 0700))
	when := time.Date(2020, 2, 2, 0, 0, 0, 0, time.UTC)
	must(t, mid.Chtimes("/d/.wh..wh.meta.a", when, when))
	ufs := New(WithWritableLayer(upper), WithReadOnlyLayer(mid), WithReadOnlyLayer(bottom))
	info := stat(t, ufs, "/d/a")
	wantMode(t, "stacked", info, 0700)
	if !info.ModTime().Equal(when) {
		t.Fatalf("mtime %v", info.ModTime())
	}
	wantMode(t, "listed", listInfo(t, ufs, "/d", "a"), 0700)
	must(t, ufs.Chmod("/d/a", 0750))
	info = lstat(t, upper, "/d/.wh..wh.meta.a")
	if info.Mode() != 0750 || !info.ModTime().Equal(when) {
		t.Fatalf("upper marker %v %v", info.Mode(), info.ModTime())
	}
	// A copy-up applies the stacked marker and has no marker to remove.
	ufs2 := New(WithWritableLayer(mustNewMemFS()), WithReadOnlyLayer(mid), WithReadOnlyLayer(bottom))
	must(t, ufs2.Truncate("/d/a", 1))
	wantMode(t, "copy", stat(t, ufs2, "/d/a"), 0700)
}

// Failures writing or removing a marker leave the union as it was.
func TestMetaFailures(t *testing.T) {
	m := "/d/.wh..wh.meta.a"

	ufs, upper, _ := faultUnion(t)
	upper.failOn("openfile", m)
	wantInjected(t, "marker create", ufs.Chmod("/d/a", 0755))
	if exists(upper, m) {
		t.Fatal("marker left behind")
	}
	upper.clear()

	upper.failOn("chmod", m)
	wantInjected(t, "marker chmod", ufs.Chmod("/d/a", 0755))
	if exists(upper, m) {
		t.Fatal("marker left behind")
	}
	wantInjected(t, "marker chmod on create", ufs.Chtimes("/d/a", changedTime, changedTime))
	if exists(upper, m) {
		t.Fatal("marker left behind")
	}
	upper.clear()

	// The change itself fails on a new marker: the marker goes.
	upper.failOn("chown", m)
	wantInjected(t, "marker chown", ufs.Chown("/d/a", 1, 1))
	if exists(upper, m) {
		t.Fatal("marker left behind")
	}
	upper.clear()

	ufs, upper, _ = faultUnion(t)
	upper.failOn("mkdir", "/d")
	wantInjected(t, "marker parent", ufs.Chmod("/d/a", 0755))
	upper.clear()

	// The copy-up after a marker cannot remove it: the copy goes instead.
	must(t, ufs.Chmod("/d/a", 0755))
	upper.failOn("remove", m)
	wantInjected(t, "marker removal on copy-up", ufs.Truncate("/d/a", 0))
	if exists(upper, "/d/a") || !exists(upper, m) {
		t.Fatal("copy kept or marker lost")
	}
	if get(t, ufs, "/d/a") != "la" {
		t.Fatal("contents")
	}

	// Remove cannot remove it: the whiteout goes too.
	wantInjected(t, "marker removal on Remove", ufs.Remove("/d/a"))
	if exists(upper, "/d/.wh.a") || get(t, ufs, "/d/a") != "la" {
		t.Fatal("Remove half done")
	}

	// Rename over it cannot remove it: reported.
	wantInjected(t, "marker removal on Rename", ufs.Rename("/d/b", "/d/a"))
	upper.clear()

	// A marker that cannot be checked fails the lookup.
	ufs, upper, _ = faultUnion(t)
	must(t, ufs.Mkdir("/d/new", 0755)) // the upper has /d now
	upper.failOn("lstat", m)
	_, err := ufs.Stat("/d/a")
	wantInjected(t, "marker lookup", err)
	upper.clear()
}

// A host lower under a memfs upper: the marker's record is not a host one,
// so Sys reports the lower file's own, and the mode still comes from the
// marker.
func TestMetaMixedLayers(t *testing.T) {
	_, _, lower := metaUnion(t, "osfs")
	upper := mustNewMemFS()
	ufs := New(WithWritableLayer(upper), WithReadOnlyLayer(lower))
	must(t, ufs.Chmod("/d/a", 0700))
	info := stat(t, ufs, "/d/a")
	wantMode(t, "mixed", info, 0700)
	if mode, ok := hostMode(info); ok && mode&0o777 != 0o644 {
		t.Fatalf("Sys mode %o, want the lower's", mode)
	}
}

// metaInfo.Sys falls back to the file's own record when the two layers'
// records differ in kind, and metaDirEntry and metaFile report errors.
func TestMetaInfoFallbacks(t *testing.T) {
	mem := mustNewMemFS()
	put(t, mem, "/f", "x")
	memInfo := stat(t, mem, "/f")
	mi := metaInfo{FileInfo: memInfo, meta: fakeInfo{}}
	if mi.Sys() != memInfo.Sys() {
		t.Fatal("memfs file with a non-memfs marker")
	}
	mi = metaInfo{FileInfo: fakeInfo{}, meta: memInfo}
	if mi.Sys() != nil {
		t.Fatal("file without a record")
	}
	if _, _, ok := ownerOf(fakeInfo{}); ok {
		t.Fatal("owner of a record-less info")
	}

	ents, err := mem.ReadDir("/")
	must(t, err)
	bad := errDirEntry{}
	if _, err := (metaDirEntry{DirEntry: bad, meta: ents[0]}).Info(); err == nil {
		t.Fatal("file info error lost")
	}
	if _, err := (metaDirEntry{DirEntry: ents[0], meta: bad}).Info(); err == nil {
		t.Fatal("marker info error lost")
	}
}

type fakeInfo struct{}

func (fakeInfo) Name() string       { return "fake" }
func (fakeInfo) Size() int64        { return 0 }
func (fakeInfo) Mode() os.FileMode  { return 0644 }
func (fakeInfo) ModTime() time.Time { return time.Time{} }
func (fakeInfo) IsDir() bool        { return false }
func (fakeInfo) Sys() any           { return nil }

type errDirEntry struct{}

func (errDirEntry) Name() string               { return "gone" }
func (errDirEntry) IsDir() bool                { return false }
func (errDirEntry) Type() fs.FileMode          { return 0 }
func (errDirEntry) Info() (fs.FileInfo, error) { return nil, errors.New("gone") }

// Concurrent metadata changes, reads, listings and a copy-up of the same
// file leave either a marker or a copy, never both, and never a torn view.
func TestMetaConcurrent(t *testing.T) {
	for _, kind := range layerKinds {
		t.Run(kind, func(t *testing.T) {
			ufs, upper, _ := metaUnion(t, kind)
			var wg sync.WaitGroup
			for g := 0; g < 8; g++ {
				wg.Add(1)
				go func(g int) {
					defer wg.Done()
					for i := 0; i < 50; i++ {
						name := fmt.Sprintf("/d/%c", "ab"[i%2])
						switch (g + i) % 5 {
						case 0:
							_ = ufs.Chmod(name, os.FileMode(0600|i%2*0111))
						case 1:
							when := lowerTime.Add(time.Duration(i) * time.Second)
							_ = ufs.Chtimes(name, when, when)
						case 2:
							if info, err := ufs.Lstat(name); err != nil || info.Size() == 0 {
								t.Errorf("Lstat %s: %v", name, err)
							}
						case 3:
							_, _ = ufs.ReadDir("/d")
						case 4:
							if g == 0 && i == 49 {
								_ = ufs.Truncate("/d/b", 1)
							} else if _, err := ufs.ReadFile(name); err != nil {
								t.Errorf("read %s: %v", name, err)
							}
						}
					}
				}(g)
			}
			wg.Wait()
			for _, n := range []string{"a", "b"} {
				if exists(upper, "/d/"+n) && exists(upper, "/d/.wh..wh.meta."+n) {
					t.Fatalf("both a copy and a marker of %s", n)
				}
			}
		})
	}
}
