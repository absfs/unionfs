package unionfs

import (
	"io/fs"
	"os"
	"path/filepath"
	"runtime"
	"testing"

	"github.com/absfs/absfs"
	"github.com/absfs/basefs"
	"github.com/absfs/memfs"
	"github.com/absfs/osfs"
)

// opaqueFS is a foldFS whose Stat and Lstat wrap their infos and report no
// identity in them, as basefs does on Windows, where a host record holds no
// file ID, and as c4fs does. Its listings are memfs's own, so an info from
// Lstat and one from ReadDir differ in type, as with basefs.
type opaqueFS struct{ *foldFS }

type opaqueInfo struct{ fs.FileInfo }

func (opaqueInfo) Sys() any { return nil }

func (f *opaqueFS) Stat(p string) (os.FileInfo, error) {
	info, err := f.foldFS.Stat(p)
	if err != nil {
		return nil, err
	}
	return opaqueInfo{info}, nil
}

func (f *opaqueFS) Lstat(p string) (os.FileInfo, error) {
	info, err := f.foldFS.Lstat(p)
	if err != nil {
		return nil, err
	}
	return opaqueInfo{info}, nil
}

// renamedInfo wraps an info under another name and does not hand it back,
// as a third-party layer might.
type renamedInfo struct {
	fs.FileInfo
	name string
}

func (i renamedInfo) Name() string { return i.name }

// unwrappingInfo wraps an info and hands it back.
type unwrappingInfo struct{ fs.FileInfo }

func (i unwrappingInfo) Sys() any            { return nil }
func (i unwrappingInfo) Unwrap() fs.FileInfo { return i.FileInfo }

// hostCaseLayers returns basefs layers over osfs on two new host
// directories, and the union of them, skipping the test where the host
// does not fold case.
func hostCaseLayers(t *testing.T, opts ...Option) (ufs *UnionFS, upper, lower absfs.SymlinkFileSystem, root string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("host-directory layers are exercised on Unix")
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	must(t, err)
	for _, d := range []string{"upper", "lower"} {
		must(t, os.Mkdir(filepath.Join(root, d), 0755))
	}
	host, err := osfs.NewFS()
	must(t, err)
	upper, err = basefs.NewFS(host, filepath.Join(root, "upper"))
	must(t, err)
	lower, err = basefs.NewFS(host, filepath.Join(root, "lower"))
	must(t, err)
	ufs = New(append([]Option{WithWritableLayer(upper), WithReadOnlyLayer(lower)}, opts...)...)
	return ufs, upper, lower, root
}

// Issue #10: over basefs layers on a host that folds case, a Rename that
// changes only the case of a name changes its spelling, whether the name
// is a lower layer's only, a copy the writable layer made of one, or the
// writable layer's own. basefs wraps the infos Lstat returns, which
// os.SameFile cannot compare, and the union took the new spelling for
// another entry stored under the old one: Rename returned nil and did
// nothing.
func TestCaseOnlyRenameOverBasefs(t *testing.T) {
	for _, tc := range []struct {
		name  string
		setup func(t *testing.T, ufs *UnionFS, lower absfs.FileSystem)
		want  []string
	}{
		{"lower", func(t *testing.T, ufs *UnionFS, lower absfs.FileSystem) {
			put(t, lower, "/README.md", "r")
		}, []string{"D /README.md", "W /Readme.md"}},
		{"copied-up", func(t *testing.T, ufs *UnionFS, lower absfs.FileSystem) {
			put(t, lower, "/README.md", "old")
			put(t, ufs, "/README.md", "r")
		}, []string{"D /README.md", "W /Readme.md"}},
		{"upper", func(t *testing.T, ufs *UnionFS, lower absfs.FileSystem) {
			put(t, ufs, "/README.md", "r")
		}, []string{"W /Readme.md"}},
	} {
		for _, cache := range [][]Option{nil, {WithLookupCache(0, 0)}} {
			name := tc.name
			if cache != nil {
				name += "/LookupCache"
			}
			t.Run(name, func(t *testing.T) {
				ufs, upper, lower, root := hostCaseLayers(t, cache...)
				if _, err := os.Lstat(filepath.Join(root, "UPPER")); err != nil {
					t.Skip("the host does not fold case here")
				}
				tc.setup(t, ufs, lower)
				lstat(t, ufs, "/Readme.md")
				must(t, ufs.Rename("/README.md", "/Readme.md"))
				wantNames(t, changeset(t, upper), tc.want...)
				wantNames(t, list(t, ufs, "/"), "Readme.md")
				if got := get(t, ufs, "/Readme.md"); got != "r" {
					t.Fatalf("contents = %q", got)
				}
			})
		}
	}
}

// The same for a rename between the composed and decomposed forms of a
// name, which macOS takes for one another even on a case-sensitive volume.
func TestNormalisationOnlyRenameOverBasefs(t *testing.T) {
	const nfc, nfd = "é", "é"
	ufs, upper, lower, root := hostCaseLayers(t)
	put(t, lower, "/f"+nfd, "f")
	if _, err := os.Lstat(filepath.Join(root, "lower", "f"+nfc)); err != nil {
		t.Skip("the host does not normalise names here")
	}
	must(t, ufs.Rename("/f"+nfd, "/f"+nfc))
	wantNames(t, changeset(t, upper), "D /f"+nfd, "W /f"+nfc)
	wantNames(t, list(t, ufs, "/"), "f"+nfc)
}

// storedName finds a lower layer's decomposed spelling of a name the caller
// spells composed by the file's identity: basefs's Lstat infos are its
// wrappers, its ReadDir infos the host's own.
func TestStoredNameNormalisationOverBasefs(t *testing.T) {
	const nfc, nfd = "é", "é"
	ufs, _, lower, root := hostCaseLayers(t)
	put(t, lower, "/f"+nfd, "f")
	if _, err := os.Lstat(filepath.Join(root, "lower", "f"+nfc)); err != nil {
		t.Skip("the host does not normalise names here")
	}
	ufs.mu.Lock()
	got := ufs.storedName(1, "/", "f"+nfc)
	ufs.mu.Unlock()
	if got != "f"+nfd {
		t.Fatalf("storedName = %+q, want %+q", got, "f"+nfd)
	}
}

// SameFile tells host files apart and together through any wrapper on
// Unix, where a host record holds the device and inode numbers, and
// through wrappers that hand back what they wrap everywhere.
func TestSameFileWrapped(t *testing.T) {
	dir := t.TempDir()
	a, b := filepath.Join(dir, "a"), filepath.Join(dir, "b")
	must(t, os.WriteFile(a, nil, 0644))
	must(t, os.WriteFile(b, nil, 0644))
	ai, err := os.Lstat(a)
	must(t, err)
	ai2, err := os.Lstat(a)
	must(t, err)
	bi, err := os.Lstat(b)
	must(t, err)

	check := func(what string, x, y fs.FileInfo, want bool) {
		t.Helper()
		if got := SameFile(x, y); got != want {
			t.Errorf("%s: SameFile = %v, want %v", what, got, want)
		}
	}
	check("raw", ai, ai2, true)
	check("raw, different", ai, bi, false)
	check("unwrapping", unwrappingInfo{ai}, unwrappingInfo{unwrappingInfo{ai2}}, true)
	check("unwrapping, different", unwrappingInfo{ai}, bi, false)
	check("nil", nil, ai, false)
	if runtime.GOOS != "windows" {
		check("renamed", renamedInfo{ai, "A"}, ai2, true)
		check("renamed, different", renamedInfo{ai, "A"}, renamedInfo{bi, "B"}, false)
	}

	host, err := osfs.NewFS()
	must(t, err)
	bfs, err := basefs.NewFS(host, dir)
	must(t, err)
	x, err := bfs.Lstat("/a")
	must(t, err)
	if runtime.GOOS != "windows" {
		check("basefs", x, ai, true)
		check("basefs, different", x, bi, false)
	}

	m, err := memfs.NewFS()
	must(t, err)
	put(t, m, "/a", "")
	put(t, m, "/b", "")
	ma, err := m.Lstat("/a")
	must(t, err)
	ma2, err := m.Lstat("/a")
	must(t, err)
	mb, err := m.Lstat("/b")
	must(t, err)
	check("memfs", ma, ma2, true)
	check("memfs, different", ma, mb, false)
	check("memfs, wrapped", unwrappingInfo{ma}, ma2, true)
	check("memfs and host", ma, ai, false)
}

// A file a metadata marker changes reports a merged Sys value, a fresh
// *inode.Inode on memfs per Lstat, and the union's own wrapper, which
// os.SameFile cannot compare. SameFile looks through it.
func TestSameFileMetaMarked(t *testing.T) {
	t.Run("memfs", func(t *testing.T) {
		ufs, _, lower := newTestUnion(t)
		put(t, lower, "/f", "f")
		put(t, lower, "/g", "g")
		must(t, ufs.Chmod("/f", 0600))
		a, b, g := lstat(t, ufs, "/f"), lstat(t, ufs, "/f"), lstat(t, ufs, "/g")
		if a.Sys() == b.Sys() {
			t.Fatal("expected a fresh merged record per Lstat")
		}
		if !SameFile(a, b) || SameFile(a, g) {
			t.Fatalf("SameFile: %v %v", SameFile(a, b), SameFile(a, g))
		}
	})
	t.Run("osfs", func(t *testing.T) {
		ufs, _, lower, _ := hostCaseLayers(t)
		put(t, lower, "/f", "f")
		put(t, lower, "/g", "g")
		must(t, ufs.Chmod("/f", 0600))
		a, b, g := lstat(t, ufs, "/f"), lstat(t, ufs, "/f"), lstat(t, ufs, "/g")
		if os.SameFile(a, b) {
			t.Log("os.SameFile sees through the union's wrapper")
		}
		if !SameFile(a, b) || SameFile(a, g) {
			t.Fatalf("SameFile: %v %v", SameFile(a, b), SameFile(a, g))
		}
	})
}

// isOSInfo knows the os package's own infos, by their type's name.
func TestIsOSInfo(t *testing.T) {
	info, err := os.Lstat(t.TempDir())
	must(t, err)
	if !isOSInfo(info) {
		t.Fatalf("isOSInfo(%T) = false", info)
	}
	if isOSInfo(renamedInfo{info, "x"}) {
		t.Fatal("isOSInfo of a wrapper")
	}
}

// Stat of a link wraps its target's info under the link's name; SameFile
// looks through that too.
func TestSameFileThroughLinkName(t *testing.T) {
	ufs, _, lower := newTestUnion(t)
	put(t, lower, "/f", "f")
	must(t, ufs.Symlink("f", "/l"))
	viaLink, err := ufs.Stat("/l")
	must(t, err)
	if viaLink.Name() != "l" {
		t.Fatalf("Stat name = %q", viaLink.Name())
	}
	if !SameFile(viaLink, lstat(t, ufs, "/f")) {
		t.Fatal("Stat through a link is not the target")
	}
}

// Where a layer's infos carry no identity, Rename takes the target for the
// source only when both are stored under one spelling in one directory:
// renaming onto another spelling of a name in another directory replaces
// that entry, keeping its spelling, whether or not the directories have
// the same top layer.
func TestOpaqueRenameAcrossDirectories(t *testing.T) {
	for _, upperB := range []bool{false, true} {
		name := "same-top"
		if upperB {
			name = "other-top"
		}
		t.Run(name, func(t *testing.T) {
			upper, lower := foldedLayers(t, "opaque")
			mustMkdirAll(t, lower, "/a")
			mustMkdirAll(t, lower, "/b")
			put(t, lower, "/a/f", "a")
			put(t, lower, "/b/f", "b")
			ufs := New(WithWritableLayer(upper), WithReadOnlyLayer(lower))
			want := []string{"D /a/f", "W /a", "W /b", "W /b/f"}
			if upperB {
				// The writable layer has /b, the lower /b/f: the two
				// directories' top layers differ.
				put(t, ufs, "/b/g", "g")
				want = append(want, "W /b/g")
			}
			must(t, ufs.Rename("/a/f", "/b/F"))
			wantNames(t, changeset(t, upper), want...)
			wantNotExist(t, ufs, "/a/f")
			if got := get(t, ufs, "/b/f"); got != "a" {
				t.Fatalf("/b/f = %q", got)
			}
		})
	}
}
