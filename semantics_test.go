package unionfs

import (
	"errors"
	"io"
	iofs "io/fs"
	"os"
	"reflect"
	"sort"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/absfs/absfs"
	"github.com/absfs/memfs"
)

// get returns the contents of name read through fs.
func get(t *testing.T, fs absfs.Filer, name string) string {
	t.Helper()
	f, err := fs.OpenFile(name, os.O_RDONLY, 0)
	if err != nil {
		t.Fatalf("open %s: %v", name, err)
	}
	defer f.Close()
	b, err := io.ReadAll(f)
	if err != nil {
		t.Fatalf("read %s: %v", name, err)
	}
	return string(b)
}

// list returns the sorted names in directory name of fs.
func list(t *testing.T, fs absfs.Filer, name string) []string {
	t.Helper()
	ents, err := fs.ReadDir(name)
	if err != nil {
		t.Fatalf("readdir %s: %v", name, err)
	}
	out := []string{}
	for _, e := range ents {
		out = append(out, e.Name())
	}
	sort.Strings(out)
	return out
}

func wantNames(t *testing.T, got []string, want ...string) {
	t.Helper()
	if want == nil {
		want = []string{}
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("names = %q, want %q", got, want)
	}
}

func wantErrIs(t *testing.T, err, target error) {
	t.Helper()
	if !errors.Is(err, target) {
		t.Fatalf("err = %v, want %v", err, target)
	}
}

func wantNotExist(t *testing.T, ufs *UnionFS, name string) {
	t.Helper()
	if _, err := ufs.Stat(name); !os.IsNotExist(err) {
		t.Fatalf("Stat %s: err = %v, want not exist", name, err)
	}
	if _, err := ufs.Lstat(name); !os.IsNotExist(err) {
		t.Fatalf("Lstat %s: err = %v, want not exist", name, err)
	}
}

func symlink(t *testing.T, fs absfs.SymlinkFileSystem, target, name string) {
	t.Helper()
	if err := fs.Symlink(target, name); err != nil {
		t.Fatalf("symlink %s -> %s: %v", name, target, err)
	}
}

// lowerTree fills lower with a small tree:
//
//	/d/a, /d/b, /d/sub/c, /top
func lowerTree(t *testing.T, lower absfs.FileSystem) {
	t.Helper()
	mustMkdirAll(t, lower, "/d/sub")
	put(t, lower, "/d/a", "la")
	put(t, lower, "/d/b", "lb")
	put(t, lower, "/d/sub/c", "lc")
	put(t, lower, "/top", "lt")
}

// upperMarkers returns every whiteout and opaque marker in the upper, as
// paths, sorted.
func upperMarkers(t *testing.T, upper absfs.FileSystem) []string {
	t.Helper()
	var out []string
	var walk func(dir string)
	walk = func(dir string) {
		ents, err := upper.ReadDir(dir)
		if err != nil {
			t.Fatalf("readdir upper %s: %v", dir, err)
		}
		for _, e := range ents {
			p := strings.TrimSuffix(dir, "/") + "/" + e.Name()
			if IsWhiteout(e.Name()) {
				out = append(out, p)
				continue
			}
			if e.IsDir() {
				walk(p)
			}
		}
	}
	walk("/")
	sort.Strings(out)
	if out == nil {
		out = []string{}
	}
	return out
}

func TestUnionFSIsSymlinkFileSystem(t *testing.T) {
	ufs, _, _ := newTestUnion(t)
	var fs interface{} = ufs
	if _, ok := fs.(absfs.SymlinkFileSystem); !ok {
		t.Fatal("*UnionFS does not implement absfs.SymlinkFileSystem")
	}
}

// --- whiteout helpers ---

func TestWhiteoutHelpers(t *testing.T) {
	for _, tt := range []struct {
		name, target string
		ok           bool
	}{
		{"/dir/.wh.file.txt", "/dir/file.txt", true},
		{".wh.c", "c", true},
		{"a/b/.wh.c", "a/b/c", true},
		{"/.wh..wh.x", "/.wh.x", true},
		{"/dir/file.txt", "", false},
		{"/dir/.wh.__dir_opaque", "", false},
		{".wh.", "", false},
	} {
		got, ok := WhiteoutTarget(tt.name)
		if got != tt.target || ok != tt.ok {
			t.Errorf("WhiteoutTarget(%q) = %q, %v; want %q, %v", tt.name, got, ok, tt.target, tt.ok)
		}
		if ok && WhiteoutName(got) != tt.name {
			t.Errorf("WhiteoutName(%q) = %q, want %q", got, WhiteoutName(got), tt.name)
		}
	}
	if !IsWhiteout("/x/.wh.y") || !IsWhiteout(OpaqueWhiteout) || IsWhiteout("/x/y.wh.") {
		t.Error("IsWhiteout")
	}
	if !IsOpaqueMarker("/x/"+OpaqueWhiteout) || IsOpaqueMarker("/x/.wh.y") {
		t.Error("IsOpaqueMarker")
	}
	if WhiteoutName("/") != "/.wh." {
		t.Errorf("WhiteoutName(/) = %q", WhiteoutName("/"))
	}
}

// --- whited-out names are gone from every read ---

func TestWhitedOutNamesAreInvisible(t *testing.T) {
	ufs, upper, lower := newTestUnion(t)
	lowerTree(t, lower)
	symlink(t, lower, "/top", "/link")

	if err := ufs.RemoveAll("/d"); err != nil {
		t.Fatal(err)
	}
	if err := ufs.Remove("/link"); err != nil {
		t.Fatal(err)
	}
	for _, p := range []string{"/d", "/d/a", "/d/sub", "/d/sub/c", "/link"} {
		wantNotExist(t, ufs, p)
	}
	if _, err := ufs.Readlink("/link"); !os.IsNotExist(err) {
		t.Fatalf("Readlink of whited-out link: %v", err)
	}
	if _, err := ufs.ReadDir("/d/sub"); !os.IsNotExist(err) {
		t.Fatalf("ReadDir under whited-out dir: %v", err)
	}
	if _, err := ufs.Open("/d/a"); !os.IsNotExist(err) {
		t.Fatalf("Open under whited-out dir: %v", err)
	}
	if _, err := ufs.ReadFile("/d/sub/c"); !os.IsNotExist(err) {
		t.Fatalf("ReadFile under whited-out dir: %v", err)
	}
	wantNames(t, list(t, ufs, "/"), "top")
	wantNames(t, upperMarkers(t, upper), "/.wh.d", "/.wh.link")
	if !exists(lower, "/d/sub/c") || !exists(lower, "/link") {
		t.Fatal("lower was modified")
	}
}

func TestMarkersAreNotNames(t *testing.T) {
	ufs, upper, lower := newTestUnion(t)
	put(t, lower, "/.wh.ghost", "") // a stray whiteout in a lower layer
	put(t, upper, "/"+OpaqueWhiteout+"x", "")
	for _, p := range []string{"/.wh.ghost", "/" + OpaqueWhiteout, "/.wh.ghost/x"} {
		wantNotExist(t, ufs, p)
	}
	for _, op := range []func() error{
		func() error { _, err := ufs.Create("/.wh.f"); return err },
		func() error { return ufs.Mkdir("/.wh.d", 0755) },
		func() error { return ufs.MkdirAll("/x/.wh.d", 0755) },
		func() error { return ufs.Symlink("/top", "/.wh.l") },
	} {
		wantErrIs(t, op(), syscall.EPERM)
	}
	put(t, ufs, "/f", "x")
	wantErrIs(t, ufs.Rename("/f", "/.wh.f"), syscall.EPERM)
	// MkdirAll made /x before it reached the refused name, as mkdir -p does.
	wantNames(t, list(t, ufs, "/"), "f", "x")
}

// --- Remove ---

func TestRemoveEdgeCases(t *testing.T) {
	ufs, upper, lower := newTestUnion(t)
	lowerTree(t, lower)

	wantErrIs(t, ufs.Remove("/"), syscall.EBUSY)
	if err := ufs.Remove("/missing"); !os.IsNotExist(err) {
		t.Fatalf("Remove missing: %v", err)
	}

	// A lower symlink is removed as a link, its target untouched.
	symlink(t, lower, "/d", "/dl")
	if err := ufs.Remove("/dl"); err != nil {
		t.Fatal(err)
	}
	if _, err := ufs.Stat("/d/a"); err != nil {
		t.Fatal(err)
	}

	// Inside an opaque directory nothing below shows, so nothing needs a
	// whiteout.
	if err := ufs.RemoveAll("/d"); err != nil {
		t.Fatal(err)
	}
	if err := ufs.Mkdir("/d", 0755); err != nil {
		t.Fatal(err)
	}
	put(t, ufs, "/d/a", "new")
	if err := ufs.Remove("/d/a"); err != nil {
		t.Fatal(err)
	}
	if exists(upper, "/d/.wh.a") {
		t.Fatal("whiteout written inside an opaque directory")
	}
	if err := ufs.Remove("/d"); err != nil {
		t.Fatal(err)
	}
	wantNames(t, upperMarkers(t, upper), "/.wh.d", "/.wh.dl")
	wantNotExist(t, ufs, "/d")
}

// --- RemoveAll ---

func TestRemoveAllMixedLayers(t *testing.T) {
	ufs, upper, lower := newTestUnion(t)
	lowerTree(t, lower)

	// Mix: modify one lower file, delete another, add upper-only entries.
	put(t, ufs, "/d/a", "ua")
	if err := ufs.Remove("/d/b"); err != nil {
		t.Fatal(err)
	}
	mustMkdirAll(t, ufs, "/d/sub/new/deep")
	put(t, ufs, "/d/sub/new/deep/f", "u")

	if err := ufs.RemoveAll("/d"); err != nil {
		t.Fatal(err)
	}
	wantNotExist(t, ufs, "/d")
	wantNotExist(t, ufs, "/d/sub/new/deep/f")
	wantNames(t, list(t, ufs, "/"), "top")
	if exists(upper, "/d") {
		t.Fatal("upper copy of the directory survived")
	}
	wantNames(t, upperMarkers(t, upper), "/.wh.d")
	if get(t, lower, "/d/a") != "la" || !exists(lower, "/d/b") {
		t.Fatal("lower was modified")
	}

	// Upper-only trees go without a trace.
	mustMkdirAll(t, ufs, "/u/v")
	put(t, ufs, "/u/v/w", "x")
	if err := ufs.RemoveAll("/u"); err != nil {
		t.Fatal(err)
	}
	wantNames(t, upperMarkers(t, upper), "/.wh.d")

	// A lower file under an upper-only... is just a file.
	if err := ufs.RemoveAll("/top"); err != nil {
		t.Fatal(err)
	}
	wantNames(t, upperMarkers(t, upper), "/.wh.d", "/.wh.top")
}

func TestRemoveAllRoot(t *testing.T) {
	ufs, upper, lower := newTestUnion(t)
	lowerTree(t, lower)
	put(t, ufs, "/mine", "m")
	if err := ufs.RemoveAll("/"); err != nil {
		t.Fatal(err)
	}
	wantNames(t, list(t, ufs, "/"))
	wantNames(t, upperMarkers(t, upper), "/.wh.d", "/.wh.top")
	if !exists(lower, "/top") {
		t.Fatal("lower was modified")
	}
}

// --- Mkdir, Create and Symlink over a whited-out name ---

func TestRecreateOverWhiteout(t *testing.T) {
	ufs, upper, lower := newTestUnion(t)
	lowerTree(t, lower)

	// A file: the whiteout goes, the new contents show.
	if err := ufs.Remove("/top"); err != nil {
		t.Fatal(err)
	}
	put(t, ufs, "/top", "new")
	if get(t, ufs, "/top") != "new" || exists(upper, "/.wh.top") {
		t.Fatal("Create over a whiteout")
	}

	// A directory: the whiteout goes and the new directory is opaque, so
	// none of the old lower entries reappear.
	if err := ufs.RemoveAll("/d"); err != nil {
		t.Fatal(err)
	}
	if err := ufs.Mkdir("/d", 0755); err != nil {
		t.Fatal(err)
	}
	wantNames(t, list(t, ufs, "/d"))
	wantNotExist(t, ufs, "/d/a")
	wantNotExist(t, ufs, "/d/sub/c")
	wantNames(t, upperMarkers(t, upper), "/d/"+OpaqueWhiteout)
	put(t, ufs, "/d/a", "fresh")
	wantNames(t, list(t, ufs, "/d"), "a")

	// A symlink.
	if err := ufs.Remove("/d/a"); err != nil {
		t.Fatal(err)
	}
	symlink(t, lower, "/top", "/ln")
	if err := ufs.Remove("/ln"); err != nil {
		t.Fatal(err)
	}
	if err := ufs.Symlink("/d", "/ln"); err != nil {
		t.Fatal(err)
	}
	if exists(upper, "/.wh.ln") {
		t.Fatal("Symlink left the whiteout")
	}
	if target, err := ufs.Readlink("/ln"); err != nil || target != "/d" {
		t.Fatalf("Readlink = %q, %v", target, err)
	}
	wantNames(t, upperMarkers(t, upper), "/d/"+OpaqueWhiteout)
}

func TestMkdirAllOverWhiteout(t *testing.T) {
	ufs, upper, lower := newTestUnion(t)
	lowerTree(t, lower)
	if err := ufs.RemoveAll("/d"); err != nil {
		t.Fatal(err)
	}
	if err := ufs.MkdirAll("/d/sub/x", 0755); err != nil {
		t.Fatal(err)
	}
	wantNames(t, list(t, ufs, "/d"), "sub")
	wantNames(t, list(t, ufs, "/d/sub"), "x")
	wantNotExist(t, ufs, "/d/sub/c")
	// Only /d needs the marker: below it nothing lower shows anyway.
	wantNames(t, upperMarkers(t, upper), "/d/"+OpaqueWhiteout)

	// MkdirAll over existing merged directories copies nothing up.
	if err := ufs.MkdirAll("/d/sub/x", 0755); err != nil {
		t.Fatal(err)
	}
	put(t, ufs, "/file", "f")
	if err := ufs.MkdirAll("/file/x", 0755); !errors.Is(err, syscall.ENOTDIR) {
		t.Fatalf("MkdirAll through a file: %v", err)
	}
}

func TestMkdirNewNameIsNotOpaque(t *testing.T) {
	ufs, upper, lower := newTestUnion(t)
	lowerTree(t, lower)
	if err := ufs.Mkdir("/d/new", 0755); err != nil {
		t.Fatal(err)
	}
	wantNames(t, upperMarkers(t, upper))
	if err := ufs.Mkdir("/d/new", 0755); !os.IsExist(err) {
		t.Fatalf("Mkdir existing: %v", err)
	}
	if err := ufs.Mkdir("/d", 0755); !os.IsExist(err) {
		t.Fatalf("Mkdir existing lower dir: %v", err)
	}
	if err := ufs.Mkdir("/nope/x", 0755); !os.IsNotExist(err) {
		t.Fatalf("Mkdir without parent: %v", err)
	}
	if err := ufs.Mkdir("/top/x", 0755); !errors.Is(err, syscall.ENOTDIR) {
		t.Fatalf("Mkdir under a file: %v", err)
	}
	// The directory a new entry goes into is copied up with its mode.
	// memfs's Chmod stores the mode verbatim, so keep the directory bit.
	if err := lower.Chmod("/d/sub", os.ModeDir|0750); err != nil {
		t.Fatal(err)
	}
	put(t, ufs, "/d/sub/n", "n")
	info, err := upper.Stat("/d/sub")
	if err != nil || !info.IsDir() || info.Mode().Perm() != 0750 {
		t.Fatalf("copied-up parent: %v, %v", info, err)
	}
}

func TestCreateNeedsParent(t *testing.T) {
	ufs, _, lower := newTestUnion(t)
	lowerTree(t, lower)
	if _, err := ufs.Create("/nope/f"); !os.IsNotExist(err) {
		t.Fatalf("Create without parent: %v", err)
	}
	if _, err := ufs.OpenFile("/missing", os.O_WRONLY, 0); !os.IsNotExist(err) {
		t.Fatalf("open missing for write without O_CREATE: %v", err)
	}
	if _, err := ufs.OpenFile("/top", os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0644); !os.IsExist(err) {
		t.Fatalf("O_EXCL on lower file: %v", err)
	}
	if _, err := ufs.OpenFile("/d", os.O_WRONLY, 0); !errors.Is(err, syscall.EISDIR) {
		t.Fatalf("open dir for write: %v", err)
	}
}

// --- opaque directories and multiple layers ---

func TestOpaqueAndWhiteoutsAcrossThreeLayers(t *testing.T) {
	top, _ := memfs.NewFS()
	mid, _ := memfs.NewFS()
	bot, _ := memfs.NewFS()
	mustMkdirAll(t, bot, "/o")
	put(t, bot, "/o/bottom", "b")
	put(t, bot, "/gone", "b")
	put(t, bot, "/kept", "b")
	mustMkdirAll(t, mid, "/o")
	put(t, mid, "/o/"+OpaqueWhiteout, "")
	put(t, mid, "/o/middle", "m")
	put(t, mid, "/.wh.gone", "")
	mustMkdirAll(t, top, "/o")
	put(t, top, "/o/top", "t")

	ufs := New(WithWritableLayer(top), WithReadOnlyLayer(mid), WithReadOnlyLayer(bot))
	wantNames(t, list(t, ufs, "/o"), "middle", "top")
	wantNotExist(t, ufs, "/o/bottom")
	wantNotExist(t, ufs, "/gone")
	wantNames(t, list(t, ufs, "/"), "kept", "o")

	// Deleting a name only the bottom layer has, under a mid-layer
	// whiteout, needs no whiteout of its own.
	if err := ufs.Remove("/kept"); err != nil {
		t.Fatal(err)
	}
	if !exists(top, "/.wh.kept") {
		t.Fatal("no whiteout for a bottom-layer name")
	}
	put(t, top, "/gone", "t")
	if err := ufs.Remove("/gone"); err != nil {
		t.Fatal(err)
	}
	if exists(top, "/.wh.gone") {
		t.Fatal("whiteout for a name the lower layers already hide")
	}
}

func TestOpaqueRootAndLowerNonDirectory(t *testing.T) {
	ufs, upper, lower := newTestUnion(t)
	lowerTree(t, lower)
	put(t, upper, "/"+OpaqueWhiteout, "")
	wantNames(t, list(t, ufs, "/"))
	wantNotExist(t, ufs, "/top")
	if err := upper.Remove("/" + OpaqueWhiteout); err != nil {
		t.Fatal(err)
	}

	// A lower file under an upper directory of the same name is hidden,
	// and so is anything further down at that path.
	mid, _ := memfs.NewFS()
	mustMkdirAll(t, mid, "/x")
	put(t, mid, "/x/m", "m")
	put(t, lower, "/x", "file")
	mustMkdirAll(t, upper, "/x")
	put(t, upper, "/x/u", "u")
	ufs = New(WithWritableLayer(upper), WithReadOnlyLayer(lower), WithReadOnlyLayer(mid))
	wantNames(t, list(t, ufs, "/x"), "u")
}

// --- Stat follows links through the union ---

func TestSymlinksResolveAcrossLayers(t *testing.T) {
	ufs, _, lower := newTestUnion(t)
	lowerTree(t, lower)

	// An upper link to a lower-only file.
	if err := ufs.Symlink("/d/a", "/la"); err != nil {
		t.Fatal(err)
	}
	info, err := ufs.Stat("/la")
	if err != nil || info.Name() != "la" || info.Size() != 2 {
		t.Fatalf("Stat through link: %v, %v", info, err)
	}
	if get(t, ufs, "/la") != "la" {
		t.Fatal("read through link")
	}
	// A relative link in a lower directory to an upper-only directory.
	mustMkdirAll(t, ufs, "/updir")
	put(t, ufs, "/updir/f", "uf")
	symlink(t, lower, "../updir", "/d/rel")
	if get(t, ufs, "/d/rel/f") != "uf" {
		t.Fatal("read through relative lower link")
	}
	wantNames(t, list(t, ufs, "/d/rel"), "f")
	if info, err := ufs.Lstat("/d/rel"); err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("Lstat of link: %v, %v", info, err)
	}
	if _, err := ufs.Readlink("/d/a"); !errors.Is(err, syscall.EINVAL) {
		t.Fatalf("Readlink of a file: %v", err)
	}

	// Writing through a link writes the target, copied up.
	put(t, ufs, "/la", "written")
	if get(t, ufs, "/d/a") != "written" || get(t, lower, "/d/a") != "la" {
		t.Fatal("write through link")
	}
	// Chmod follows; Lchown does not.
	if err := ufs.Chmod("/la", 0600); err != nil {
		t.Fatal(err)
	}
	if info, _ := ufs.Stat("/d/a"); info.Mode().Perm() != 0600 {
		t.Fatalf("Chmod through link: %v", info.Mode())
	}

	// Creating through a dangling link creates its target.
	if err := ufs.Symlink("/d/made", "/dangling"); err != nil {
		t.Fatal(err)
	}
	if _, err := ufs.OpenFile("/dangling", os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0644); !os.IsExist(err) {
		t.Fatalf("O_EXCL through dangling link: %v", err)
	}
	put(t, ufs, "/dangling", "made")
	if get(t, ufs, "/d/made") != "made" {
		t.Fatal("create through dangling link")
	}

	// Loops.
	if err := ufs.Symlink("/loop2", "/loop1"); err != nil {
		t.Fatal(err)
	}
	if err := ufs.Symlink("/loop1", "/loop2"); err != nil {
		t.Fatal(err)
	}
	if _, err := ufs.Stat("/loop1"); !errors.Is(err, syscall.ELOOP) {
		t.Fatalf("Stat of a loop: %v", err)
	}
	if _, err := ufs.Create("/loop1"); !errors.Is(err, syscall.ELOOP) {
		t.Fatalf("Create through a loop: %v", err)
	}
	if err := ufs.Symlink("/x", "/loop1"); !os.IsExist(err) {
		t.Fatalf("Symlink over existing: %v", err)
	}
}

func TestLowerSymlinkCopiedUpAsLink(t *testing.T) {
	ufs, upper, lower := newTestUnion(t)
	lowerTree(t, lower)
	symlink(t, lower, "d/a", "/ln")

	if err := ufs.Lchown("/ln", os.Getuid(), os.Getgid()); err != nil {
		t.Fatal(err)
	}
	info, err := upper.Lstat("/ln")
	if err != nil || info.Mode()&os.ModeSymlink == 0 {
		t.Fatalf("Lchown copy-up: %v, %v", info, err)
	}
	if target, _ := upper.Readlink("/ln"); target != "d/a" {
		t.Fatalf("copied link target = %q", target)
	}
	if exists(upper, "/d/a") {
		t.Fatal("Lchown followed the link")
	}

	// Renaming a lower link moves the link, not its target.
	symlink(t, lower, "/top", "/ln2")
	if err := ufs.Rename("/ln2", "/d/ln3"); err != nil {
		t.Fatal(err)
	}
	if target, err := ufs.Readlink("/d/ln3"); err != nil || target != "/top" {
		t.Fatalf("renamed link: %q, %v", target, err)
	}
	wantNotExist(t, ufs, "/ln2")
}

// --- copy-up on attribute changes and writes ---

func TestCopyUpOnChanges(t *testing.T) {
	when := time.Date(2020, 1, 2, 3, 4, 5, 0, time.UTC)
	for _, tc := range []struct {
		name   string
		change func(*UnionFS) error
		check  func(t *testing.T, ufs *UnionFS, upper absfs.FileSystem)
	}{
		{"Chmod", func(u *UnionFS) error { return u.Chmod("/d/a", 0600) },
			func(t *testing.T, u *UnionFS, up absfs.FileSystem) {
				if i, _ := u.Stat("/d/a"); i.Mode() != 0600 {
					t.Fatalf("mode %v", i.Mode())
				}
			}},
		{"ChmodDir", func(u *UnionFS) error { return u.Chmod("/d", 0700) },
			func(t *testing.T, u *UnionFS, up absfs.FileSystem) {
				i, _ := u.Stat("/d")
				if !i.IsDir() || i.Mode().Perm() != 0700 {
					t.Fatalf("mode %v", i.Mode())
				}
				wantNames(t, list(t, u, "/d"), "a", "b", "sub")
			}},
		{"Chtimes", func(u *UnionFS) error { return u.Chtimes("/d/a", when, when) },
			func(t *testing.T, u *UnionFS, up absfs.FileSystem) {
				if i, _ := u.Stat("/d/a"); !i.ModTime().Equal(when) {
					t.Fatalf("mtime %v", i.ModTime())
				}
			}},
		{"Chown", func(u *UnionFS) error { return u.Chown("/d/a", os.Getuid(), os.Getgid()) },
			func(t *testing.T, u *UnionFS, up absfs.FileSystem) {}},
		{"TruncateZero", func(u *UnionFS) error { return u.Truncate("/d/a", 0) },
			func(t *testing.T, u *UnionFS, up absfs.FileSystem) {
				if s := get(t, u, "/d/a"); s != "" {
					t.Fatalf("contents %q", s)
				}
			}},
		{"TruncateShorter", func(u *UnionFS) error { return u.Truncate("/d/a", 1) },
			func(t *testing.T, u *UnionFS, up absfs.FileSystem) {
				if s := get(t, u, "/d/a"); s != "l" {
					t.Fatalf("contents %q", s)
				}
			}},
		{"OpenWrite", func(u *UnionFS) error {
			f, err := u.OpenFile("/d/a", os.O_WRONLY, 0)
			if err != nil {
				return err
			}
			if _, err := f.Write([]byte("X")); err != nil {
				return err
			}
			return f.Close()
		}, func(t *testing.T, u *UnionFS, up absfs.FileSystem) {
			if s := get(t, u, "/d/a"); s != "Xa" {
				t.Fatalf("contents %q", s)
			}
		}},
		{"OpenAppend", func(u *UnionFS) error {
			f, err := u.OpenFile("/d/a", os.O_WRONLY|os.O_APPEND, 0)
			if err != nil {
				return err
			}
			if _, err := f.Write([]byte("+")); err != nil {
				return err
			}
			return f.Close()
		}, func(t *testing.T, u *UnionFS, up absfs.FileSystem) {
			if s := get(t, u, "/d/a"); s != "la+" {
				t.Fatalf("contents %q", s)
			}
		}},
		{"OpenTrunc", func(u *UnionFS) error {
			f, err := u.OpenFile("/d/a", os.O_WRONLY|os.O_TRUNC, 0)
			if err != nil {
				return err
			}
			return f.Close()
		}, func(t *testing.T, u *UnionFS, up absfs.FileSystem) {
			if s := get(t, u, "/d/a"); s != "" {
				t.Fatalf("contents %q", s)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ufs, upper, lower := newTestUnion(t)
			lowerTree(t, lower)
			if err := lower.Chmod("/d/a", 0640); err != nil {
				t.Fatal(err)
			}
			if err := tc.change(ufs); err != nil {
				t.Fatal(err)
			}
			if !exists(upper, "/d/a") && tc.name != "ChmodDir" {
				t.Fatal("not copied up")
			}
			tc.check(t, ufs, upper)
			if get(t, lower, "/d/a") != "la" {
				t.Fatal("lower was modified")
			}
			info, _ := lower.Stat("/d/a")
			if info.Mode().Perm() != 0640 {
				t.Fatal("lower mode was modified")
			}
			wantNames(t, upperMarkers(t, upper))
		})
	}
}

func TestCopyUpPreservesModeAndTime(t *testing.T) {
	ufs, upper, lower := newTestUnion(t)
	lowerTree(t, lower)
	when := time.Date(2021, 5, 6, 7, 8, 9, 0, time.UTC)
	if err := lower.Chmod("/d/b", 0751); err != nil {
		t.Fatal(err)
	}
	if err := lower.Chtimes("/d/b", when, when); err != nil {
		t.Fatal(err)
	}
	if err := ufs.Chown("/d/b", os.Getuid(), os.Getgid()); err != nil {
		t.Fatal(err)
	}
	info, err := upper.Stat("/d/b")
	if err != nil || info.Mode().Perm() != 0751 || !info.ModTime().Equal(when) {
		t.Fatalf("copied up: %v %v %v", info.Mode(), info.ModTime(), err)
	}
	if get(t, upper, "/d/b") != "lb" {
		t.Fatal("contents")
	}
	if err := ufs.Truncate("/d", 0); !errors.Is(err, syscall.EISDIR) {
		t.Fatalf("Truncate dir: %v", err)
	}
	if err := ufs.Truncate("/missing", 0); !os.IsNotExist(err) {
		t.Fatalf("Truncate missing: %v", err)
	}
	for _, f := range []func() error{
		func() error { return ufs.Chmod("/missing", 0) },
		func() error { return ufs.Chown("/missing", 0, 0) },
		func() error { return ufs.Chtimes("/missing", when, when) },
		func() error { return ufs.Lchown("/missing", 0, 0) },
	} {
		if err := f(); !os.IsNotExist(err) {
			t.Fatalf("change of missing file: %v", err)
		}
	}
}

// --- Rename ---

func TestRenameFiles(t *testing.T) {
	t.Run("UpperOnly", func(t *testing.T) {
		ufs, upper, _ := newTestUnion(t)
		put(t, ufs, "/a", "x")
		if err := ufs.Rename("/a", "/b"); err != nil {
			t.Fatal(err)
		}
		wantNotExist(t, ufs, "/a")
		if get(t, ufs, "/b") != "x" {
			t.Fatal("contents")
		}
		wantNames(t, upperMarkers(t, upper))
	})
	t.Run("LowerOnly", func(t *testing.T) {
		ufs, upper, lower := newTestUnion(t)
		lowerTree(t, lower)
		if err := lower.Chmod("/d/a", 0600); err != nil {
			t.Fatal(err)
		}
		if err := ufs.Rename("/d/a", "/moved"); err != nil {
			t.Fatal(err)
		}
		wantNotExist(t, ufs, "/d/a")
		if get(t, ufs, "/moved") != "la" {
			t.Fatal("contents")
		}
		if info, _ := ufs.Stat("/moved"); info.Mode().Perm() != 0600 {
			t.Fatalf("mode %v", info.Mode())
		}
		wantNames(t, upperMarkers(t, upper), "/d/.wh.a")
		if get(t, lower, "/d/a") != "la" {
			t.Fatal("lower modified")
		}
	})
	t.Run("OverLowerFile", func(t *testing.T) {
		ufs, upper, lower := newTestUnion(t)
		lowerTree(t, lower)
		if err := ufs.Rename("/d/a", "/d/b"); err != nil {
			t.Fatal(err)
		}
		if get(t, ufs, "/d/b") != "la" {
			t.Fatal("contents")
		}
		wantNames(t, list(t, ufs, "/d"), "b", "sub")
		wantNames(t, upperMarkers(t, upper), "/d/.wh.a")
	})
	t.Run("OverUpperFile", func(t *testing.T) {
		ufs, upper, _ := newTestUnion(t)
		put(t, ufs, "/a", "A")
		put(t, ufs, "/b", "B")
		if err := ufs.Rename("/a", "/b"); err != nil {
			t.Fatal(err)
		}
		if get(t, ufs, "/b") != "A" {
			t.Fatal("contents")
		}
		wantNames(t, list(t, ufs, "/"), "b")
		wantNames(t, upperMarkers(t, upper))
	})
	t.Run("OntoWhiteout", func(t *testing.T) {
		ufs, upper, lower := newTestUnion(t)
		lowerTree(t, lower)
		if err := ufs.Remove("/top"); err != nil {
			t.Fatal(err)
		}
		put(t, ufs, "/x", "X")
		if err := ufs.Rename("/x", "/top"); err != nil {
			t.Fatal(err)
		}
		if get(t, ufs, "/top") != "X" {
			t.Fatal("contents")
		}
		wantNames(t, upperMarkers(t, upper))
	})
	t.Run("SamePath", func(t *testing.T) {
		ufs, upper, lower := newTestUnion(t)
		lowerTree(t, lower)
		if err := ufs.Rename("/top", "/./top"); err != nil {
			t.Fatal(err)
		}
		if exists(upper, "/top") {
			t.Fatal("rename to itself copied up")
		}
	})
}

func TestRenameErrors(t *testing.T) {
	ufs, _, lower := newTestUnion(t)
	lowerTree(t, lower)
	mustMkdirAll(t, lower, "/empty")
	for _, tc := range []struct {
		old, new string
		want     error
	}{
		{"/missing", "/x", os.ErrNotExist},
		{"/top", "/missing/x", os.ErrNotExist},
		{"/top", "/top/x", syscall.ENOTDIR},
		{"/", "/x", syscall.EBUSY},
		{"/top", "/d", syscall.EISDIR},
		{"/d", "/top", syscall.ENOTDIR},
		{"/empty", "/d", syscall.ENOTEMPTY},
		{"/d", "/d/sub/x", syscall.EINVAL},
	} {
		err := ufs.Rename(tc.old, tc.new)
		var le *os.LinkError
		if !errors.As(err, &le) || !errors.Is(err, tc.want) {
			t.Errorf("Rename(%s, %s) = %v, want LinkError %v", tc.old, tc.new, err, tc.want)
		}
	}
}

func TestRenameDirectories(t *testing.T) {
	t.Run("UpperOnly", func(t *testing.T) {
		ufs, upper, _ := newTestUnion(t)
		mustMkdirAll(t, ufs, "/u/v")
		put(t, ufs, "/u/v/f", "f")
		if err := ufs.Rename("/u", "/w"); err != nil {
			t.Fatal(err)
		}
		wantNotExist(t, ufs, "/u")
		if get(t, ufs, "/w/v/f") != "f" {
			t.Fatal("contents")
		}
		wantNames(t, upperMarkers(t, upper))
	})
	t.Run("LowerOnly", func(t *testing.T) {
		ufs, upper, lower := newTestUnion(t)
		lowerTree(t, lower)
		if err := ufs.Rename("/d", "/e"); err != nil {
			t.Fatal(err)
		}
		wantNotExist(t, ufs, "/d")
		wantNotExist(t, ufs, "/d/sub/c")
		wantNames(t, list(t, ufs, "/e"), "a", "b", "sub")
		if get(t, ufs, "/e/sub/c") != "lc" {
			t.Fatal("contents")
		}
		wantNames(t, upperMarkers(t, upper), "/.wh.d")
		if get(t, lower, "/d/sub/c") != "lc" {
			t.Fatal("lower modified")
		}
	})
	t.Run("MixedWithWhiteoutsInside", func(t *testing.T) {
		ufs, upper, lower := newTestUnion(t)
		lowerTree(t, lower)
		put(t, ufs, "/d/a", "ua")
		if err := ufs.Remove("/d/b"); err != nil {
			t.Fatal(err)
		}
		if err := ufs.Remove("/d/sub/c"); err != nil {
			t.Fatal(err)
		}
		put(t, ufs, "/d/sub/n", "un")
		if err := ufs.Rename("/d", "/e"); err != nil {
			t.Fatal(err)
		}
		wantNames(t, list(t, ufs, "/e"), "a", "sub")
		wantNames(t, list(t, ufs, "/e/sub"), "n")
		if get(t, ufs, "/e/a") != "ua" {
			t.Fatal("contents")
		}
		wantNotExist(t, ufs, "/d")
		// The moved copy is self-contained: no stale markers inside it.
		wantNames(t, upperMarkers(t, upper), "/.wh.d")
	})
	t.Run("OntoWhitedOutLowerDir", func(t *testing.T) {
		ufs, upper, lower := newTestUnion(t)
		lowerTree(t, lower)
		mustMkdirAll(t, lower, "/old")
		put(t, lower, "/old/stale", "s")
		if err := ufs.RemoveAll("/old"); err != nil {
			t.Fatal(err)
		}
		mustMkdirAll(t, ufs, "/u")
		put(t, ufs, "/u/f", "f")
		if err := ufs.Rename("/u", "/old"); err != nil {
			t.Fatal(err)
		}
		wantNames(t, list(t, ufs, "/old"), "f")
		wantNames(t, upperMarkers(t, upper), "/old/"+OpaqueWhiteout)
	})
	t.Run("OntoEmptyMergedDir", func(t *testing.T) {
		ufs, upper, lower := newTestUnion(t)
		lowerTree(t, lower)
		mustMkdirAll(t, lower, "/tgt")
		put(t, lower, "/tgt/gone", "g")
		if err := ufs.Remove("/tgt/gone"); err != nil {
			t.Fatal(err)
		}
		mustMkdirAll(t, ufs, "/u")
		put(t, ufs, "/u/f", "f")
		if err := ufs.Rename("/u", "/tgt"); err != nil {
			t.Fatal(err)
		}
		wantNames(t, list(t, ufs, "/tgt"), "f")
		wantNames(t, upperMarkers(t, upper), "/tgt/"+OpaqueWhiteout)
	})
	t.Run("OpaqueDirToFreshName", func(t *testing.T) {
		ufs, upper, lower := newTestUnion(t)
		lowerTree(t, lower)
		if err := ufs.RemoveAll("/d"); err != nil {
			t.Fatal(err)
		}
		if err := ufs.Mkdir("/d", 0755); err != nil {
			t.Fatal(err)
		}
		put(t, ufs, "/d/n", "n")
		if err := ufs.Rename("/d", "/fresh"); err != nil {
			t.Fatal(err)
		}
		wantNames(t, list(t, ufs, "/fresh"), "n")
		// /d is whited out again; /fresh replaces nothing, so no marker.
		wantNames(t, upperMarkers(t, upper), "/.wh.d")
	})
	t.Run("Nested", func(t *testing.T) {
		ufs, upper, lower := newTestUnion(t)
		lowerTree(t, lower)
		if err := ufs.Rename("/d/sub", "/d/sub2"); err != nil {
			t.Fatal(err)
		}
		wantNames(t, list(t, ufs, "/d"), "a", "b", "sub2")
		if get(t, ufs, "/d/sub2/c") != "lc" {
			t.Fatal("contents")
		}
		wantNames(t, upperMarkers(t, upper), "/d/.wh.sub")
	})
}

// --- working directory, views, Sub ---

func TestWorkingDirectories(t *testing.T) {
	ufs, _, lower := newTestUnion(t)
	lowerTree(t, lower)

	if wd, _ := ufs.Getwd(); wd != "/" {
		t.Fatalf("Getwd = %q", wd)
	}
	if err := ufs.Chdir("/d"); err != nil {
		t.Fatal(err)
	}
	if wd, _ := ufs.Getwd(); wd != "/d" {
		t.Fatalf("Getwd = %q", wd)
	}
	if get(t, ufs, "sub/c") != "lc" {
		t.Fatal("relative read")
	}
	if err := ufs.Chdir("/top"); !errors.Is(err, syscall.ENOTDIR) {
		t.Fatalf("Chdir to a file: %v", err)
	}
	if err := ufs.Chdir("/missing"); !os.IsNotExist(err) {
		t.Fatalf("Chdir to missing: %v", err)
	}

	// Views keep their own working directory.
	v1, v2 := ufs.FileSystem(), ufs.SymlinkFileSystem()
	if err := v1.Chdir("/d/sub"); err != nil {
		t.Fatal(err)
	}
	if err := v1.Chdir("/top"); !errors.Is(err, syscall.ENOTDIR) {
		t.Fatalf("view Chdir to file: %v", err)
	}
	if err := v1.Chdir("/missing"); !os.IsNotExist(err) {
		t.Fatalf("view Chdir to missing: %v", err)
	}
	if wd, _ := v1.Getwd(); wd != "/d/sub" {
		t.Fatalf("view Getwd = %q", wd)
	}
	if wd, _ := v2.Getwd(); wd != "/" {
		t.Fatalf("other view Getwd = %q", wd)
	}
	put(t, v1, "rel", "r")
	if get(t, ufs, "/d/sub/rel") != "r" {
		t.Fatal("view relative write")
	}
	if err := v1.Truncate("rel", 0); err != nil {
		t.Fatal(err)
	}
	if err := v1.RemoveAll("rel"); err != nil {
		t.Fatal(err)
	}
	wantNotExist(t, ufs, "/d/sub/rel")
	if v1.TempDir() != "/tmp" {
		t.Fatal("TempDir")
	}

	// Sub sees the merged view.
	sub, err := ufs.Sub("/d")
	if err != nil {
		t.Fatal(err)
	}
	b, err := fsReadFile(sub, "sub/c")
	if err != nil || string(b) != "lc" {
		t.Fatalf("Sub read: %q, %v", b, err)
	}
	sub2, err := v1.Sub(".")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fsReadFile(sub2, "c"); err != nil {
		t.Fatal(err)
	}
}

// --- a union without a writable layer ---

func TestReadOnlyUnion(t *testing.T) {
	lower, _ := memfs.NewFS()
	lowerTree(t, lower)
	ufs := New(WithReadOnlyLayer(lower))
	when := time.Now()
	for name, err := range map[string]error{
		"Mkdir":     ufs.Mkdir("/x", 0755),
		"MkdirAll":  ufs.MkdirAll("/x/y", 0755),
		"Remove":    ufs.Remove("/top"),
		"RemoveAll": ufs.RemoveAll("/d"),
		"Rename":    ufs.Rename("/top", "/x"),
		"Chmod":     ufs.Chmod("/top", 0),
		"Chown":     ufs.Chown("/top", 0, 0),
		"Chtimes":   ufs.Chtimes("/top", when, when),
		"Lchown":    ufs.Lchown("/top", 0, 0),
		"Truncate":  ufs.Truncate("/top", 0),
		"Symlink":   ufs.Symlink("/top", "/l"),
	} {
		if !errors.Is(err, ErrNoWritableLayer) {
			t.Errorf("%s: %v, want ErrNoWritableLayer", name, err)
		}
	}
	if _, err := ufs.Create("/x"); !errors.Is(err, ErrNoWritableLayer) {
		t.Errorf("Create: %v", err)
	}
	if get(t, ufs, "/d/sub/c") != "lc" {
		t.Fatal("read")
	}
	wantNames(t, list(t, ufs, "/"), "d", "top")

	empty := New()
	if _, err := empty.Stat("/"); !os.IsNotExist(err) {
		t.Fatalf("Stat on a union with no layers: %v", err)
	}
}

// --- a changeset read back from the upper ---

// changeset reads an upper layer as a list of changes relative to the
// layers below: "D path" for a deletion (a whiteout), "R path" for a
// directory that replaces the lower one (an opaque marker), and "W path" for
// a file or link written, or a directory created or copied up.
func changeset(t *testing.T, upper absfs.FileSystem) []string {
	t.Helper()
	var out []string
	var walk func(dir string)
	walk = func(dir string) {
		ents, err := upper.ReadDir(dir)
		if err != nil {
			t.Fatal(err)
		}
		for _, e := range ents {
			p := strings.TrimSuffix(dir, "/") + "/" + e.Name()
			if IsOpaqueMarker(p) {
				out = append(out, "R "+dir)
				continue
			}
			if deleted, ok := WhiteoutTarget(p); ok {
				out = append(out, "D "+deleted)
				continue
			}
			out = append(out, "W "+p)
			if e.IsDir() {
				walk(p)
			}
		}
	}
	walk("/")
	sort.Strings(out)
	return out
}

func TestUpperReadsAsChangeset(t *testing.T) {
	ufs, upper, lower := newTestUnion(t)
	lowerTree(t, lower)

	put(t, ufs, "/d/a", "edited")           // modify
	must(t, ufs.Remove("/d/b"))             // delete
	must(t, ufs.RemoveAll("/d/sub"))        // delete a tree
	must(t, ufs.Mkdir("/d/sub", 0755))      // replace it
	put(t, ufs, "/d/sub/only", "o")         //
	put(t, ufs, "/added", "n")              // add
	put(t, ufs, "/tmpfile", "t")            // add then delete:
	must(t, ufs.Remove("/tmpfile"))         // no trace
	must(t, ufs.Rename("/top", "/renamed")) // move
	got := changeset(t, upper)
	want := []string{
		"D /d/b",
		"D /top",
		"R /d/sub",
		"W /added",
		"W /d",
		"W /d/a",
		"W /d/sub",
		"W /d/sub/only",
		"W /renamed",
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("changeset:\n got %q\nwant %q", got, want)
	}
}

var fsReadFile = iofs.ReadFile

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
