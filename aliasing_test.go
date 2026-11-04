package unionfs

import (
	"errors"
	"io/fs"
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

// foldFS is a layer over memfs that folds the case of names and preserves
// it, as a macOS or Windows host does: a name resolves to the entry whose
// name equals it in any case (and, as on macOS, in either Unicode form of
// "é"), and a new entry keeps the spelling it was created with. It fails the test whenever the union hands it a path that a
// host would resolve through a symbolic link: one with a link among its
// directory elements, or, for an operation that follows a final link, a
// link at its end.
type foldFS struct {
	absfs.SymlinkFileSystem
	t *testing.T
	// short makes every entry whose name is not 8.3 reachable by a short
	// name too, as NTFS does where 8.3 names are on (see shortOf).
	short bool
}

func newFoldFS(t *testing.T) *foldFS {
	t.Helper()
	m, err := memfs.NewFS()
	must(t, err)
	return &foldFS{SymlinkFileSystem: m, t: t}
}

// newShortFS returns a foldFS that also resolves 8.3 short names.
func newShortFS(t *testing.T) *foldFS {
	f := newFoldFS(t)
	f.short = true
	return f
}

// shortOf returns the (lowercase) short name NTFS would first give an entry
// named name, or "" when name is 8.3 already: the name without leading
// dots, its base's first six bytes without dots, "~1", and the first three
// bytes after its last dot.
func shortOf(name string) string {
	if b, e, _ := strings.Cut(name, "."); b != "" && len(b) <= 8 && len(e) <= 3 && !strings.Contains(e, ".") {
		return ""
	}
	n := strings.TrimLeft(name, ".")
	base, ext := n, ""
	if i := strings.LastIndexByte(n, '.'); i >= 0 {
		base, ext = n[:i], n[i+1:]
	}
	base = strings.ReplaceAll(base, ".", "")
	if len(base) > 6 {
		base = base[:6]
	}
	if len(ext) > 3 {
		ext = ext[:3]
	}
	s := base + "~1"
	if ext != "" {
		s += "." + ext
	}
	return s
}

// decompose stands in for Unicode normalisation in foldFS: it decomposes
// the one letter the tests spell both ways.
var decompose = strings.NewReplacer("\u00e9", "e\u0301", "\u00c9", "E\u0301").Replace

// stored returns the name of the entry in directory dir that element e
// names: e itself when an entry has exactly that name, else the entry whose
// name, or short name where they are on, equals e in any case and either
// form of "é", as on macOS, else e (a new name).
func (f *foldFS) stored(dir, e string) string {
	if _, err := f.SymlinkFileSystem.Lstat(path.Join(dir, e)); err == nil {
		return e
	}
	ents, _ := f.SymlinkFileSystem.ReadDir(dir)
	for _, ent := range ents {
		if strings.EqualFold(decompose(ent.Name()), decompose(e)) {
			return ent.Name()
		}
	}
	if f.short {
		for _, ent := range ents {
			if strings.EqualFold(shortOf(ent.Name()), e) {
				return ent.Name()
			}
		}
	}
	return e
}

// fold returns p as the layer stores it, after checking that resolving it
// passes through no link, and ends at none when follows is set.
func (f *foldFS) fold(op, p string, follows bool) string {
	f.t.Helper()
	elems := strings.Split(strings.Trim(path.Clean(p), "/"), "/")
	cur := "/"
	for i, e := range elems {
		if e == "" {
			break
		}
		e = f.stored(cur, e)
		elems[i] = e
		p = "/" + strings.Join(elems, "/")
		cur = path.Join(cur, e)
		if i == len(elems)-1 && !follows {
			break
		}
		if info, err := f.SymlinkFileSystem.Lstat(cur); err == nil && isSymlink(info) {
			f.t.Errorf("%s %s goes through the link %s", op, p, cur)
		}
	}
	return p
}

func (f *foldFS) Stat(p string) (os.FileInfo, error) {
	return f.SymlinkFileSystem.Stat(f.fold("stat", p, true))
}

func (f *foldFS) Lstat(p string) (os.FileInfo, error) {
	return f.SymlinkFileSystem.Lstat(f.fold("lstat", p, false))
}

func (f *foldFS) Open(p string) (absfs.File, error) {
	return f.SymlinkFileSystem.Open(f.fold("open", p, true))
}

func (f *foldFS) Create(p string) (absfs.File, error) {
	return f.SymlinkFileSystem.Create(f.fold("create", p, true))
}

func (f *foldFS) OpenFile(p string, flag int, perm os.FileMode) (absfs.File, error) {
	return f.SymlinkFileSystem.OpenFile(f.fold("openfile", p, true), flag, perm)
}

func (f *foldFS) Mkdir(p string, perm os.FileMode) error {
	return f.SymlinkFileSystem.Mkdir(f.fold("mkdir", p, false), perm)
}

func (f *foldFS) MkdirAll(p string, perm os.FileMode) error {
	return f.SymlinkFileSystem.MkdirAll(f.fold("mkdirall", p, true), perm)
}

func (f *foldFS) Remove(p string) error {
	return f.SymlinkFileSystem.Remove(f.fold("remove", p, false))
}

func (f *foldFS) RemoveAll(p string) error {
	return f.SymlinkFileSystem.RemoveAll(f.fold("removeall", p, false))
}

// Rename of an entry to another spelling of its own name changes the
// name's spelling, as on a host. Rename onto another entry, a file or an
// empty directory, replaces it and keeps its spelling, as APFS does (memfs
// itself refuses to replace an entry).
func (f *foldFS) Rename(o, n string) error {
	fo, fn := f.fold("rename", o, false), f.fold("rename", n, false)
	if fo == fn {
		fn = path.Join(path.Dir(fn), path.Base(n))
	}
	src, serr := f.SymlinkFileSystem.Lstat(fo)
	dst, derr := f.SymlinkFileSystem.Lstat(fn)
	if serr == nil && derr == nil && fo != fn && src.IsDir() == dst.IsDir() {
		if err := f.SymlinkFileSystem.Remove(fn); err != nil {
			return err
		}
	}
	return f.SymlinkFileSystem.Rename(fo, fn)
}

func (f *foldFS) Chmod(p string, m os.FileMode) error {
	return f.SymlinkFileSystem.Chmod(f.fold("chmod", p, true), m)
}

func (f *foldFS) Chown(p string, uid, gid int) error {
	return f.SymlinkFileSystem.Chown(f.fold("chown", p, true), uid, gid)
}

func (f *foldFS) Lchown(p string, uid, gid int) error {
	return f.SymlinkFileSystem.Lchown(f.fold("lchown", p, false), uid, gid)
}

func (f *foldFS) Chtimes(p string, a, m time.Time) error {
	return f.SymlinkFileSystem.Chtimes(f.fold("chtimes", p, true), a, m)
}

func (f *foldFS) Truncate(p string, size int64) error {
	return f.SymlinkFileSystem.Truncate(f.fold("truncate", p, true), size)
}

func (f *foldFS) ReadDir(p string) ([]fs.DirEntry, error) {
	return f.SymlinkFileSystem.ReadDir(f.fold("readdir", p, true))
}

func (f *foldFS) ReadFile(p string) ([]byte, error) {
	return f.SymlinkFileSystem.ReadFile(f.fold("readfile", p, true))
}

func (f *foldFS) Readlink(p string) (string, error) {
	return f.SymlinkFileSystem.Readlink(f.fold("readlink", p, false))
}

func (f *foldFS) Symlink(o, n string) error {
	return f.SymlinkFileSystem.Symlink(o, f.fold("symlink", n, false))
}

// The lookup cache keys a name by the union's spelling of it, and a host
// that folds case takes "/d" and "/D" for one entry. A directory cached as
// "/d" that becomes a link through the spelling "/D" must not be walked
// through as the directory it was: the host would follow the link.
func TestLookupCacheAliasedDirBecomesLink(t *testing.T) {
	t.Run("removed-and-replaced", func(t *testing.T) {
		upper := newFoldFS(t)
		mustMkdirAll(t, upper, "/outside")
		putCanary(t, upper, "/outside/canary")
		lower, err := memfs.NewFS()
		must(t, err)
		ufs := New(WithWritableLayer(upper), WithReadOnlyLayer(lower), WithLookupCache(time.Minute, 0))

		must(t, ufs.Mkdir("/d", 0755))
		if _, err := ufs.Stat("/d/nothing"); !os.IsNotExist(err) {
			t.Fatalf("Stat /d/nothing: %v", err)
		}
		must(t, ufs.Remove("/D"))
		must(t, ufs.Mkdir("/stage", 0755))
		must(t, ufs.Symlink("/outside", "/stage/l"))
		must(t, ufs.Rename("/stage/l", "/D"))

		// Through the link, resolved against the union: /outside.
		if f, err := ufs.OpenFile("/d/canary", os.O_WRONLY|os.O_TRUNC, 0); err == nil {
			f.Close()
		}
		_ = ufs.Chmod("/d/canary", 0777)
		if f, err := ufs.Create("/d/planted"); err == nil {
			f.Close()
		}
	})
	// A directory only a lower layer has, cached with what is under it, and
	// a link made at another spelling of its name in a writable layer that
	// folds case where the lower does not: nothing is removed.
	t.Run("shadowed-by-link", func(t *testing.T) {
		upper := newFoldFS(t)
		mustMkdirAll(t, upper, "/outside/sub")
		lower, err := memfs.NewFS()
		must(t, err)
		mustMkdirAll(t, lower, "/d/sub")
		ufs := New(WithWritableLayer(upper), WithReadOnlyLayer(lower), WithLookupCache(time.Minute, 0))

		if _, err := ufs.Stat("/d/sub/nothing"); !os.IsNotExist(err) {
			t.Fatalf("Stat /d/sub/nothing: %v", err)
		}
		must(t, ufs.Symlink("/outside", "/D"))
		if f, err := ufs.Create("/d/sub/planted"); err == nil {
			f.Close()
		}
		// The union sees the link's target, as without the cache.
		if _, err := ufs.Lstat("/outside/sub/planted"); err != nil {
			t.Fatalf("Lstat /outside/sub/planted: %v", err)
		}
	})
}

// The same on a host directory, where the host aliases names for real:
// by case on macOS and Windows, and on macOS also by Unicode normalisation
// ("é" precomposed and decomposed), even on a case-sensitive volume.
func TestLookupCacheHostAliasedDirBecomesLink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("host-directory layers are exercised on Unix")
	}
	for _, tc := range []struct{ name, cached, alias string }{
		{"case", "/d", "/D"},
		{"nfd", "/é", "/é"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			root, err := filepath.EvalSymlinks(t.TempDir())
			must(t, err)
			for _, d := range []string{"upper", "lower", "outside"} {
				must(t, os.Mkdir(filepath.Join(root, d), 0755))
			}
			canary := filepath.Join(root, "outside", "canary")
			must(t, os.WriteFile(canary, []byte(canaryData), 0600))
			host, err := osfs.NewFS()
			must(t, err)
			upper, err := basefs.NewFS(host, filepath.Join(root, "upper"))
			must(t, err)
			lower, err := basefs.NewFS(host, filepath.Join(root, "lower"))
			must(t, err)
			ufs := New(WithWritableLayer(upper), WithReadOnlyLayer(lower), WithLookupCache(time.Minute, 0))

			must(t, ufs.Mkdir(tc.cached, 0755))
			if _, err := ufs.Stat(tc.cached + "/nothing"); !os.IsNotExist(err) {
				t.Fatalf("Stat %s/nothing: %v", tc.cached, err)
			}
			if err := ufs.Remove(tc.alias); err != nil {
				t.Skipf("the host does not take %q for %q: %v", tc.alias, tc.cached, err)
			}
			// The union cannot put a link that climbs out at the alias:
			// basefs, confined to its root, refuses to move one there
			// (it judges the link where it lands), so its confinement is
			// the guard here. The union's own cache guard is covered on
			// a folding memfs above, and a link made around the union in
			// TestLookupCacheOutOfBandLink (issue #12).
			must(t, ufs.Mkdir("/stage", 0755))
			must(t, ufs.Symlink("../outside", "/stage/l"))
			if err := ufs.Rename("/stage/l", tc.alias); err == nil {
				t.Fatalf("Rename /stage/l %s: a link climbing out of the layer was moved in", tc.alias)
			}
			if _, err := os.Lstat(filepath.Join(root, "upper", tc.alias[1:])); !os.IsNotExist(err) {
				t.Fatalf("something is at %s on the host: %v", tc.alias, err)
			}
			must(t, ufs.RemoveAll("/stage"))

			if f, err := ufs.OpenFile(tc.cached+"/canary", os.O_WRONLY|os.O_TRUNC, 0); err == nil {
				f.Write([]byte("changed"))
				f.Close()
			}
			_ = ufs.Chmod(tc.cached+"/canary", 0777)
			if f, err := ufs.Create(tc.cached + "/planted"); err == nil {
				f.Close()
			}
			_ = ufs.Mkdir(tc.cached+"/newdir", 0755)

			data, err := os.ReadFile(canary)
			must(t, err)
			info, err := os.Stat(canary)
			must(t, err)
			if string(data) != canaryData || info.Mode().Perm() != 0600 {
				t.Fatalf("canary outside the layer changed: %q %v", data, info.Mode())
			}
			ents, err := os.ReadDir(filepath.Join(root, "outside"))
			must(t, err)
			if len(ents) != 1 {
				t.Fatalf("outside the layer now holds %d entries", len(ents))
			}
		})
	}
}

// A client cannot create a name that starts with the whiteout prefix in
// any ASCII case: a host that folds case would take it for the union's own
// marker. Names that only look alike are ordinary.
func TestCaseFoldedReservedNamesRefused(t *testing.T) {
	refused := []string{".WH.x", ".Wh.x", ".wH.x", ".WH.", ".WH..wh.meta.y", ".wh..WH.META.y", ".WH.__dir_opaque", ".WH..WH..OPQ"}
	allowed := []string{".WHx", ".WH", "x.WH.y", "..wh.x", ". wh.x", ".w h.x", ".Ẇh.x"}
	for _, kind := range layerKinds {
		t.Run(kind, func(t *testing.T) {
			ufs, upper, _ := metaUnion(t, kind)
			for _, name := range refused {
				for _, dir := range []string{"/", "/d/"} {
					p := dir + name
					wantPerm := func(op string, err error) {
						t.Helper()
						if !errors.Is(err, syscall.EPERM) {
							t.Errorf("%s %s: err = %v, want EPERM", op, p, err)
						}
					}
					_, err := ufs.Create(p)
					wantPerm("Create", err)
					_, err = ufs.OpenFile(p, os.O_CREATE|os.O_WRONLY, 0644)
					wantPerm("OpenFile", err)
					wantPerm("Mkdir", ufs.Mkdir(p, 0755))
					wantPerm("MkdirAll", ufs.MkdirAll(p+"/sub", 0755))
					wantPerm("MkdirAll", ufs.MkdirAll(dir+"new/"+name, 0755))
					wantPerm("Symlink", ufs.Symlink("/d/a", p))
					wantPerm("Rename", ufs.Rename("/d/a", p))
				}
			}
			for _, name := range allowed {
				p := "/d/" + name
				put(t, ufs, p, "client")
				if get(t, ufs, p) != "client" {
					t.Fatalf("%s: contents", p)
				}
				must(t, ufs.Remove(p))
			}
			// An entry already at such a name, made around the union, is
			// hidden as the marker it could be on a host that folds case:
			// not listed, read, changed, renamed or removed.
			put(t, upper, "/d/.WH.old", "old")
			wantNotExist(t, ufs, "/d/.WH.old")
			for _, n := range list(t, ufs, "/d") {
				if strings.EqualFold(n, ".wh.old") {
					t.Fatalf("listed %s", n)
				}
			}
			wantGone := func(op string, err error) {
				t.Helper()
				if !os.IsNotExist(err) {
					t.Errorf("%s /d/.WH.old: err = %v, want not exist", op, err)
				}
			}
			_, err := ufs.ReadFile("/d/.WH.old")
			wantGone("ReadFile", err)
			_, err = ufs.OpenFile("/d/.WH.old", os.O_WRONLY|os.O_APPEND, 0)
			wantGone("OpenFile", err)
			wantGone("Chmod", ufs.Chmod("/d/.WH.old", 0600))
			wantGone("Rename", ufs.Rename("/d/.WH.old", "/d/kept"))
			wantGone("Remove", ufs.Remove("/d/.WH.old"))
			if get(t, upper, "/d/.WH.old") != "old" {
				t.Fatal("hidden entry changed")
			}
			// A directory that holds nothing else is empty, and goes with
			// it.
			must(t, ufs.Mkdir("/e", 0755))
			put(t, upper, "/e/.WH.old", "old")
			wantNames(t, list(t, ufs, "/e"))
			must(t, ufs.Remove("/e"))
			if exists(upper, "/e") {
				t.Fatal("upper still has /e")
			}
			for _, n := range []string{"/d/a", "/d/b"} {
				if _, err := ufs.Lstat(n); err != nil {
					t.Fatalf("Lstat %s: %v", n, err)
				}
			}
		})
	}
}
