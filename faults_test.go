package unionfs

import (
	"errors"
	"io/fs"
	"os"
	"strings"
	"sync"
	"syscall"
	"testing"

	"github.com/absfs/absfs"
	"github.com/absfs/memfs"
)

var errInjected = errors.New("injected fault")

// faultFS wraps a layer and fails chosen operations on chosen paths.
type faultFS struct {
	absfs.SymlinkFileSystem
	mu    sync.Mutex
	fails map[string]error
}

func newFaultFS(t *testing.T) *faultFS {
	t.Helper()
	m, err := memfs.NewFS()
	if err != nil {
		t.Fatal(err)
	}
	return &faultFS{SymlinkFileSystem: m, fails: map[string]error{}}
}

// failOn makes op on p fail; p "*" matches every path.
func (f *faultFS) failOn(op, p string) {
	f.mu.Lock()
	f.fails[op+" "+p] = errInjected
	f.mu.Unlock()
}

// failWith makes op on p fail with err.
func (f *faultFS) failWith(op, p string, err error) {
	f.mu.Lock()
	f.fails[op+" "+p] = err
	f.mu.Unlock()
}

func (f *faultFS) clear() {
	f.mu.Lock()
	f.fails = map[string]error{}
	f.mu.Unlock()
}

func (f *faultFS) check(op, p string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if err := f.fails[op+" "+p]; err != nil {
		return &os.PathError{Op: op, Path: p, Err: err}
	}
	if err := f.fails[op+" *"]; err != nil {
		return &os.PathError{Op: op, Path: p, Err: err}
	}
	return nil
}

func (f *faultFS) Stat(p string) (os.FileInfo, error) {
	if err := f.check("stat", p); err != nil {
		return nil, err
	}
	return f.SymlinkFileSystem.Stat(p)
}

func (f *faultFS) Lstat(p string) (os.FileInfo, error) {
	if err := f.check("lstat", p); err != nil {
		return nil, err
	}
	return f.SymlinkFileSystem.Lstat(p)
}

func (f *faultFS) Open(p string) (absfs.File, error) {
	if err := f.check("open", p); err != nil {
		return nil, err
	}
	return f.SymlinkFileSystem.Open(p)
}

func (f *faultFS) OpenFile(p string, flag int, perm os.FileMode) (absfs.File, error) {
	if err := f.check("openfile", p); err != nil {
		return nil, err
	}
	return f.SymlinkFileSystem.OpenFile(p, flag, perm)
}

func (f *faultFS) Mkdir(p string, perm os.FileMode) error {
	if err := f.check("mkdir", p); err != nil {
		return err
	}
	return f.SymlinkFileSystem.Mkdir(p, perm)
}

func (f *faultFS) Remove(p string) error {
	if err := f.check("remove", p); err != nil {
		return err
	}
	return f.SymlinkFileSystem.Remove(p)
}

func (f *faultFS) RemoveAll(p string) error {
	if err := f.check("removeall", p); err != nil {
		return err
	}
	return f.SymlinkFileSystem.RemoveAll(p)
}

func (f *faultFS) Rename(o, n string) error {
	if err := f.check("rename", o); err != nil {
		return err
	}
	return f.SymlinkFileSystem.Rename(o, n)
}

func (f *faultFS) Chmod(p string, m os.FileMode) error {
	if err := f.check("chmod", p); err != nil {
		return err
	}
	return f.SymlinkFileSystem.Chmod(p, m)
}

func (f *faultFS) Chown(p string, uid, gid int) error {
	if err := f.check("chown", p); err != nil {
		return err
	}
	return f.SymlinkFileSystem.Chown(p, uid, gid)
}

func (f *faultFS) ReadDir(p string) ([]fs.DirEntry, error) {
	if err := f.check("readdir", p); err != nil {
		return nil, err
	}
	return f.SymlinkFileSystem.ReadDir(p)
}

func (f *faultFS) Readlink(p string) (string, error) {
	if err := f.check("readlink", p); err != nil {
		return "", err
	}
	return f.SymlinkFileSystem.Readlink(p)
}

func (f *faultFS) Symlink(o, n string) error {
	if err := f.check("symlink", n); err != nil {
		return err
	}
	return f.SymlinkFileSystem.Symlink(o, n)
}

// plainFS hides every optional capability of a layer: no Lstat, Readlink,
// Symlink or Lchown.
type plainFS struct{ absfs.FileSystem }

// faultUnion returns a union over a faulty upper and a faulty lower, with
// lowerTree in the lower.
func faultUnion(t *testing.T) (*UnionFS, *faultFS, *faultFS) {
	t.Helper()
	upper, lower := newFaultFS(t), newFaultFS(t)
	lowerTree(t, lower)
	return New(WithWritableLayer(upper), WithReadOnlyLayer(lower)), upper, lower
}

func wantInjected(t *testing.T, what string, err error) {
	t.Helper()
	if !errors.Is(err, errInjected) {
		t.Fatalf("%s: err = %v, want the injected fault", what, err)
	}
}

func TestLayerErrorsPropagate(t *testing.T) {
	ufs, upper, lower := faultUnion(t)

	lower.failOn("lstat", "/d/a")
	_, err := ufs.Stat("/d/a")
	wantInjected(t, "Stat", err)
	lower.clear()

	upper.failOn("stat", "/")
	_, err = ufs.Stat("/")
	wantInjected(t, "Stat with a broken root", err)
	upper.clear()

	upper.failOn("readdir", "/d")
	upper.failOn("readdir", "/")
	_, err = ufs.ReadDir("/")
	wantInjected(t, "ReadDir of the top layer", err)
	upper.clear()
	// A lower layer that fails to list fails the listing: skipping it
	// would show its files as deleted.
	must(t, ufs.Chtimes("/d", changedTime, changedTime))
	lower.failOn("readdir", "/d")
	_, err = ufs.ReadDir("/d")
	wantInjected(t, "ReadDir of a lower layer", err)
	f, err := ufs.Open("/d")
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.Readdirnames(-1)
	wantInjected(t, "Readdirnames", err)
	f.Close()
	lower.clear()

	// A broken lookup fails every operation that resolves the path.
	lower.failOn("lstat", "/d")
	_, err = ufs.Lstat("/d")
	wantInjected(t, "Lstat", err)
	_, err = ufs.Open("/d/a")
	wantInjected(t, "Open", err)
	_, err = ufs.ReadFile("/d/a")
	wantInjected(t, "ReadFile", err)
	_, err = ufs.ReadDir("/d")
	wantInjected(t, "ReadDir", err)
	_, err = ufs.Readlink("/d")
	wantInjected(t, "Readlink", err)
	_, err = ufs.Create("/d/x")
	wantInjected(t, "Create", err)
	_, err = ufs.OpenFile("/d", os.O_WRONLY, 0)
	wantInjected(t, "OpenFile", err)
	wantInjected(t, "Mkdir", ufs.Mkdir("/d/x", 0755))
	wantInjected(t, "MkdirAll", ufs.MkdirAll("/d/x", 0755))
	wantInjected(t, "Remove", ufs.Remove("/d"))
	wantInjected(t, "RemoveAll", ufs.RemoveAll("/d"))
	wantInjected(t, "Rename from", ufs.Rename("/d", "/e"))
	wantInjected(t, "Rename to", ufs.Rename("/top", "/d/x"))
	wantInjected(t, "Rename onto", ufs.Rename("/top", "/d"))
	wantInjected(t, "Chmod", ufs.Chmod("/d", 0700))
	wantInjected(t, "Truncate", ufs.Truncate("/d", 0))
	wantInjected(t, "Symlink", ufs.Symlink("/x", "/d"))
	wantInjected(t, "Chdir", ufs.Chdir("/d"))
	lower.clear()

	// A whiteout or opaque probe that fails is an error, not an absence.
	upper.failOn("lstat", "/d/.wh.a")
	_, err = ufs.Stat("/d/a")
	wantInjected(t, "whiteout probe", err)
	upper.clear()
	upper.failOn("lstat", "/d/"+OpaqueWhiteout)
	_, err = ufs.Stat("/d/a")
	wantInjected(t, "opaque probe", err)
	upper.clear()
	upper.failOn("lstat", "/"+OpaqueWhiteout)
	_, err = ufs.Stat("/d/a")
	wantInjected(t, "root opaque probe", err)
	upper.clear()
	upper.failOn("lstat", "/d/.wh.a")
	wantInjected(t, "lower probe on Remove", ufs.Remove("/d/a"))
	wantInjected(t, "lower probe on Rename", ufs.Rename("/d/a", "/x"))
	wantInjected(t, "lower probe on Rename", ufs.Rename("/top", "/d/a"))
	upper.clear()
	lower.failOn("lstat", "/d/new")
	wantInjected(t, "lower probe on Mkdir", ufs.Mkdir("/d/new", 0755))
	lower.clear()
}

func TestCopyUpFailuresLeaveNoPartialFile(t *testing.T) {
	ufs, upper, lower := faultUnion(t)

	lower.failOn("open", "/d/a")
	_, err := ufs.OpenFile("/d/a", os.O_WRONLY, 0)
	wantInjected(t, "copy-up read", err)
	if exists(upper, "/d/a") {
		t.Fatal("partial copy left in the upper")
	}
	lower.clear()

	upper.failOn("chmod", "/d/a")
	wantInjected(t, "copy-up chmod", ufs.Truncate("/d/a", 1))
	if exists(upper, "/d/a") {
		t.Fatal("partial copy left in the upper")
	}
	upper.clear()

	upper.failOn("openfile", "/d/b")
	wantInjected(t, "copy-up create", ufs.Truncate("/d/b", 1))
	upper.clear()

	ufs, upper, _ = faultUnion(t)
	upper.failOn("mkdir", "/d")
	wantInjected(t, "copy-up of the parent", ufs.Chtimes("/d/b", changedTime, changedTime))
	upper.clear()

	ufs, upper, _ = faultUnion(t)
	upper.failOn("chmod", "/d")
	wantInjected(t, "copy-up of the parent's mode", ufs.Chtimes("/d", changedTime, changedTime))
	upper.clear()

	ufs, upper, lower = faultUnion(t)
	// A lower link whose target cannot be read.
	symlink(t, lower, "/top", "/ln")
	lower.failOn("readlink", "/ln")
	wantInjected(t, "link copy-up", ufs.Lchown("/ln", 0, 0))
	_, err = ufs.Stat("/ln")
	wantInjected(t, "Stat through an unreadable link", err)
	_, err = ufs.Readlink("/ln")
	wantInjected(t, "Readlink", err)
	lower.clear()
	upper.failOn("symlink", "/ln")
	wantInjected(t, "link copy-up", ufs.Lchown("/ln", 0, 0))
	upper.clear()
}

func TestMarkerWriteFailures(t *testing.T) {
	ufs, upper, _ := faultUnion(t)

	upper.failOn("openfile", "/.wh.top")
	wantInjected(t, "whiteout write", ufs.Remove("/top"))
	upper.clear()

	must(t, ufs.Remove("/top"))
	upper.failOn("remove", "/.wh.top")
	_, err := ufs.Create("/top")
	wantInjected(t, "whiteout removal on create", err)
	wantInjected(t, "whiteout removal on mkdir", ufs.Mkdir("/top", 0755))
	wantInjected(t, "whiteout removal on symlink", ufs.Symlink("/x", "/top"))
	upper.clear()

	wantNotExist(t, ufs, "/top")
	wantNames(t, upperMarkers(t, upper), "/.wh.top")

	must(t, ufs.RemoveAll("/d"))
	upper.failOn("openfile", "/d/"+OpaqueWhiteout)
	wantInjected(t, "opaque marker write", ufs.Mkdir("/d", 0755))
	upper.clear()
	wantNotExist(t, ufs, "/d")
	wantNames(t, upperMarkers(t, upper), "/.wh.d", "/.wh.top")

	upper.failOn("mkdir", "/x")
	wantInjected(t, "mkdir", ufs.MkdirAll("/x/y", 0755))
	upper.clear()
}

func TestRemoveFailures(t *testing.T) {
	ufs, upper, _ := faultUnion(t)
	must(t, ufs.Remove("/d/a"))
	must(t, ufs.Remove("/d/b"))
	must(t, ufs.Remove("/d/sub/c"))

	upper.failOn("readdir", "/d/sub")
	wantInjected(t, "listing a directory to remove", ufs.Remove("/d/sub"))
	upper.clear()

	// The directory is moved aside before it is deleted; when that fails
	// the whiteout written for it is taken back.
	upper.failOn("rename", "/d/sub")
	wantInjected(t, "moving the directory aside", ufs.Remove("/d/sub"))
	upper.clear()
	wantNames(t, upperMarkers(t, upper), "/d/.wh.a", "/d/.wh.b", "/d/sub/.wh.c")
	wantNames(t, list(t, ufs, "/d/sub"))

	upper.failOn("lstat", "/d/"+internalPrefix+"2")
	wantInjected(t, "naming the aside copy", ufs.Remove("/d/sub"))
	upper.clear()

	upper.failOn("rename", "/d")
	wantInjected(t, "RemoveAll of the upper copy", ufs.RemoveAll("/d"))
	upper.clear()

	// Deleting the aside copy fails: the directory is gone from the merged
	// view, and the leftover hides nothing.
	upper.failOn("removeall", "*")
	wantInjected(t, "deleting the aside copy", ufs.Remove("/d/sub"))
	upper.clear()
	wantNotExist(t, ufs, "/d/sub")
	wantNames(t, list(t, ufs, "/d"))
	ents, err := upper.ReadDir("/d")
	must(t, err)
	var leftovers int
	for _, e := range ents {
		if strings.HasPrefix(e.Name(), internalPrefix) {
			leftovers++
			if _, ok := WhiteoutTarget(e.Name()); ok {
				t.Fatalf("leftover %s reads as a whiteout", e.Name())
			}
		}
	}
	if leftovers != 1 {
		t.Fatalf("leftovers = %d, want 1", leftovers)
	}
	// Removing the parent clears the leftover with the other markers.
	must(t, ufs.Remove("/d"))
	wantNames(t, upperMarkers(t, upper), "/.wh.d")

	// A directory only the writable layer has: its stray markers go first.
	ufs, upper, _ = faultUnion(t)
	must(t, ufs.Mkdir("/u", 0755))
	put(t, upper, "/u/.wh.stray", "")
	upper.failOn("removeall", "/u/.wh.stray")
	wantInjected(t, "clearing stray markers", ufs.Remove("/u"))
	upper.clear()
	upper.failOn("remove", "/u")
	wantInjected(t, "removing the upper directory", ufs.Remove("/u"))
	upper.clear()
	must(t, ufs.Remove("/u"))
	wantNames(t, upperMarkers(t, upper))

	// A modified lower file: the whiteout is taken back when the upper copy
	// cannot be deleted.
	put(t, ufs, "/top", "edited")
	upper.failOn("remove", "/top")
	wantInjected(t, "removing the upper copy", ufs.Remove("/top"))
	upper.clear()
	if get(t, ufs, "/top") != "edited" {
		t.Fatal("upper copy lost")
	}
	wantNames(t, upperMarkers(t, upper))

	upper.failOn("readdir", "/")
	wantInjected(t, "RemoveAll of the root", ufs.RemoveAll("/"))
	upper.clear()
	upper.failOn("openfile", "/.wh.top")
	wantInjected(t, "RemoveAll of the root", ufs.RemoveAll("/"))
	upper.clear()

	// The whiteout's directory cannot be copied up.
	ufs2, upper2, _ := faultUnion(t)
	upper2.failOn("mkdir", "/d")
	wantInjected(t, "whiteout parent", ufs2.Remove("/d/a"))
}

func TestRenameFailures(t *testing.T) {
	ufs, _, lower := faultUnion(t)

	// Copy-up of the source.
	lower.failOn("open", "/d/a")
	wantInjected(t, "file copy-up", ufs.Rename("/d/a", "/x"))
	lower.clear()
	lower.failOn("readdir", "/d/sub")
	wantInjected(t, "listing a lower directory to move", ufs.Rename("/d", "/e"))
	lower.clear()

	ufs, upper, _ := faultUnion(t)
	upper.failOn("mkdir", "/d/sub")
	wantInjected(t, "nested directory copy-up", ufs.Rename("/d", "/e"))
	upper.clear()

	ufs, upper, _ = faultUnion(t)
	upper.failOn("readdir", "/d")
	wantInjected(t, "listing the directory to move", ufs.Rename("/d", "/e"))
	upper.clear()

	ufs, upper, lower = faultUnion(t)
	lower.failOn("lstat", "/d/b")
	upper.failOn("readdir", "/nothing")
	wantInjected(t, "resolving an entry to move", ufs.Rename("/d", "/e"))
	lower.clear()

	ufs, upper, _ = faultUnion(t)
	must(t, ufs.Remove("/d/a"))
	upper.failOn("removeall", "/e/.wh.a")
	wantInjected(t, "stripping markers", ufs.Rename("/d", "/e"))
	upper.clear()
	ufs, upper, _ = faultUnion(t)
	must(t, ufs.Remove("/d/a"))
	upper.failOn("readdir", "/e/sub")
	wantInjected(t, "listing markers to strip", ufs.Rename("/d", "/e"))
	upper.clear()

	// The destination directory cannot be copied up.
	ufs, upper, _ = faultUnion(t)
	upper.failOn("mkdir", "/d")
	put(t, ufs, "/f", "f")
	wantInjected(t, "destination parent", ufs.Rename("/f", "/d/f"))
	upper.clear()

	// The move itself fails: with a target in the upper, the target is
	// moved aside, the move retried, and the target put back.
	upper.failOn("rename", "/f")
	wantInjected(t, "rename", ufs.Rename("/f", "/g"))
	put(t, ufs, "/g", "g")
	wantInjected(t, "rename over a target", ufs.Rename("/f", "/g"))
	if get(t, ufs, "/g") != "g" || get(t, ufs, "/f") != "f" {
		t.Fatal("a failed rename must leave source and target alone")
	}
	upper.failOn("rename", "/g")
	wantInjected(t, "moving the target aside", ufs.Rename("/f", "/g"))
	upper.clear()
	upper.failOn("rename", "/f")
	upper.failOn("lstat", "/"+internalPrefix+"3")
	wantInjected(t, "naming the target's aside copy", ufs.Rename("/f", "/g"))
	upper.clear()
	if get(t, ufs, "/g") != "g" {
		t.Fatal("target lost")
	}
	wantNames(t, list(t, ufs, "/"), "d", "f", "g", "top")

	// memfs refuses to replace, so this move always goes through the aside
	// copy; deleting it afterwards fails, and is reported, but the move
	// stands and the leftover is hidden.
	upper.failOn("removeall", "*")
	wantInjected(t, "deleting the replaced target", ufs.Rename("/f", "/g"))
	upper.clear()
	if get(t, ufs, "/g") != "f" {
		t.Fatal("the move must stand")
	}
	wantNames(t, list(t, ufs, "/"), "d", "g", "top")

	// A lower source: its whiteout is written before the move and taken
	// back when the move fails.
	ufs, upper, _ = faultUnion(t)
	upper.failOn("rename", "/top")
	wantInjected(t, "rename of a lower file", ufs.Rename("/top", "/top2"))
	upper.clear()
	wantNames(t, upperMarkers(t, upper))
	if get(t, ufs, "/top") != "lt" {
		t.Fatal("source lost")
	}
	wantNotExist(t, ufs, "/top2")

	// Markers after the move.
	ufs, upper, _ = faultUnion(t)
	upper.failOn("openfile", "/.wh.top")
	wantInjected(t, "whiteout of the old name", ufs.Rename("/top", "/top2"))
	upper.clear()

	ufs, upper, _ = faultUnion(t)
	must(t, ufs.Remove("/top"))
	put(t, ufs, "/n", "n")
	upper.failOn("remove", "/.wh.top")
	wantInjected(t, "whiteout of the new name", ufs.Rename("/n", "/top"))
	upper.clear()

	ufs, upper, _ = faultUnion(t)
	must(t, ufs.RemoveAll("/d"))
	must(t, ufs.Mkdir("/u", 0755))
	upper.failOn("openfile", "/d/"+OpaqueWhiteout)
	wantInjected(t, "opaque marker at the new name", ufs.Rename("/u", "/d"))
	upper.clear()

	ufs, upper, _ = faultUnion(t)
	must(t, ufs.RemoveAll("/d"))
	must(t, ufs.Mkdir("/d", 0755)) // opaque, replacing the lower /d
	upper.failOn("remove", "/fresh/"+OpaqueWhiteout)
	wantInjected(t, "dropping a stale opaque marker", ufs.Rename("/d", "/fresh"))
	upper.clear()
}

func TestRenameFailureKeepsTarget(t *testing.T) {
	ufs, upper, _ := faultUnion(t)
	put(t, ufs, "/a", "A")
	put(t, ufs, "/b", "B")
	upper.failOn("rename", "/a")
	wantInjected(t, "rename", ufs.Rename("/a", "/b"))
	upper.clear()
	if get(t, ufs, "/b") != "B" {
		t.Fatal("a failed rename deleted its target")
	}
	must(t, ufs.Rename("/a", "/b"))
	if get(t, ufs, "/b") != "A" {
		t.Fatal("contents")
	}
	wantNames(t, list(t, ufs, "/"), "b", "d", "top")
	wantNames(t, upperMarkers(t, upper))
}

func TestLayersWithoutOptionalMethods(t *testing.T) {
	upperMem, _ := memfs.NewFS()
	lowerMem, _ := memfs.NewFS()
	lowerTree(t, lowerMem)
	ufs := New(WithWritableLayer(plainFS{upperMem}), WithReadOnlyLayer(plainFS{lowerMem}))

	// Lookups fall back to Stat.
	if get(t, ufs, "/d/sub/c") != "lc" {
		t.Fatal("read through Stat-only layers")
	}
	must(t, ufs.Remove("/d/a"))
	wantNotExist(t, ufs, "/d/a")

	// No symlinks in the writable layer.
	if err := ufs.Symlink("/top", "/l"); !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("Symlink: %v", err)
	}
	// Lchown falls back to Chown.
	must(t, ufs.Lchown("/top", os.Getuid(), os.Getgid()))

	// A lower link the union cannot read, or copy up.
	symlink(t, lowerMem, "/top", "/ln")
	ufs = New(WithWritableLayer(plainFS{upperMem}), WithReadOnlyLayer(lowerMem))
	if err := ufs.Lchown("/ln", 0, 0); !errors.Is(err, errors.ErrUnsupported) {
		t.Fatalf("link copy-up to a layer without links: %v", err)
	}
	ufs = New(WithWritableLayer(upperMem), WithReadOnlyLayer(linkInfoFS{plainFS{lowerMem}}))
	if _, err := ufs.Stat("/ln"); !errors.Is(err, syscall.EINVAL) {
		t.Fatalf("Stat of a link in a layer without Readlink: %v", err)
	}
}

// linkInfoFS has Lstat but no Readlink.
type linkInfoFS struct{ plainFS }

func (l linkInfoFS) Lstat(p string) (os.FileInfo, error) {
	return l.FileSystem.(absfs.SymlinkFileSystem).Lstat(p)
}

// pipeFS reports /pipe as a named pipe.
type pipeFS struct{ absfs.SymlinkFileSystem }

type pipeInfo struct{ os.FileInfo }

func (pipeInfo) Mode() os.FileMode { return os.ModeNamedPipe | 0644 }

func (p pipeFS) Lstat(name string) (os.FileInfo, error) {
	info, err := p.SymlinkFileSystem.Lstat(name)
	if err == nil && name == "/pipe" {
		info = pipeInfo{info}
	}
	return info, err
}

func TestCopyUpOfSpecialFileIsRefused(t *testing.T) {
	upper, _ := memfs.NewFS()
	lower, _ := memfs.NewFS()
	put(t, lower, "/pipe", "")
	ufs := New(WithWritableLayer(upper), WithReadOnlyLayer(pipeFS{lower}))
	if err := ufs.Chmod("/pipe", 0600); !errors.Is(err, syscall.ENOTSUP) {
		t.Fatalf("copy-up of a named pipe: %v", err)
	}
}

// TestTruncatingCopyUpReadsNothing checks that copying a file up to
// truncate it to zero never reads the lower copy, while any other copy-up
// does.
func TestTruncatingCopyUpReadsNothing(t *testing.T) {
	ufs, upper, lower := faultUnion(t)
	lower.failOn("open", "*")
	lower.failOn("openfile", "*")

	f, err := ufs.OpenFile("/d/a", os.O_WRONLY|os.O_TRUNC, 0)
	must(t, err)
	must(t, f.Close())
	must(t, ufs.Truncate("/d/b", 0))
	wantInjected(t, "Truncate to a non-zero size", ufs.Truncate("/top", 1))
	_, err = ufs.OpenFile("/d/sub/c", os.O_WRONLY, 0)
	wantInjected(t, "OpenFile without O_TRUNC", err)
	lower.clear()

	for _, p := range []string{"/d/a", "/d/b"} {
		if get(t, ufs, p) != "" {
			t.Fatalf("%s not empty", p)
		}
		if !exists(upper, p) {
			t.Fatalf("%s not copied up", p)
		}
	}
}

// TestCreateFlagAloneCopiesNothing opens an existing lower file with
// O_RDONLY|O_CREATE: that is a read, and must not copy the file up.
func TestCreateFlagAloneCopiesNothing(t *testing.T) {
	ufs, upper, _ := faultUnion(t)
	f, err := ufs.OpenFile("/d/a", os.O_RDONLY|os.O_CREATE, 0644)
	must(t, err)
	buf := make([]byte, 8)
	n, _ := f.Read(buf)
	must(t, f.Close())
	if string(buf[:n]) != "la" {
		t.Fatalf("read %q", buf[:n])
	}
	if exists(upper, "/d/a") || exists(upper, "/d") {
		t.Fatal("O_RDONLY|O_CREATE copied the file up")
	}
	_, err = ufs.OpenFile("/d", os.O_RDONLY|os.O_CREATE, 0755)
	wantErrIs(t, err, syscall.EISDIR)
	// A missing file is still created.
	f, err = ufs.OpenFile("/d/new", os.O_RDONLY|os.O_CREATE, 0644)
	must(t, err)
	must(t, f.Close())
	if !exists(upper, "/d/new") {
		t.Fatal("O_RDONLY|O_CREATE did not create a missing file")
	}
}
