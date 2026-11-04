package unionfs

import (
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"syscall"
	"testing"
	"time"

	"github.com/absfs/basefs"
	"github.com/absfs/memfs"
	"github.com/absfs/osfs"
)

// outOfBandWrites are the changes made through a cached directory that has
// since become a link around the union. Each must leave what the link
// points at alone.
var outOfBandWrites = []struct {
	name string
	run  func(ufs *UnionFS, dir string) error
}{
	{"open-trunc", func(ufs *UnionFS, dir string) error {
		f, err := ufs.OpenFile(dir+"/canary", os.O_WRONLY|os.O_TRUNC, 0)
		if err == nil {
			f.Write([]byte("changed"))
			f.Close()
		}
		return err
	}},
	{"open-append", func(ufs *UnionFS, dir string) error {
		f, err := ufs.OpenFile(dir+"/canary", os.O_WRONLY|os.O_APPEND, 0)
		if err == nil {
			f.Write([]byte("changed"))
			f.Close()
		}
		return err
	}},
	{"create", func(ufs *UnionFS, dir string) error {
		f, err := ufs.Create(dir + "/planted")
		if err == nil {
			f.Close()
		}
		return err
	}},
	{"chmod", func(ufs *UnionFS, dir string) error { return ufs.Chmod(dir+"/canary", 0777) }},
	{"chtimes", func(ufs *UnionFS, dir string) error {
		return ufs.Chtimes(dir+"/canary", time.Now(), time.Now())
	}},
	{"chown", func(ufs *UnionFS, dir string) error {
		return ufs.Chown(dir+"/canary", os.Getuid(), os.Getgid())
	}},
	{"lchown", func(ufs *UnionFS, dir string) error {
		return ufs.Lchown(dir+"/canary", os.Getuid(), os.Getgid())
	}},
	{"truncate", func(ufs *UnionFS, dir string) error { return ufs.Truncate(dir+"/canary", 0) }},
	{"remove", func(ufs *UnionFS, dir string) error { return ufs.Remove(dir + "/canary") }},
	{"removeall", func(ufs *UnionFS, dir string) error { return ufs.RemoveAll(dir + "/canary") }},
	{"rename-from", func(ufs *UnionFS, dir string) error { return ufs.Rename(dir+"/canary", "/moved") }},
	{"rename-to", func(ufs *UnionFS, dir string) error {
		f, err := ufs.Create("/src")
		if err != nil {
			return err
		}
		f.Close()
		return ufs.Rename("/src", dir+"/canary")
	}},
	{"mkdir", func(ufs *UnionFS, dir string) error { return ufs.Mkdir(dir+"/planted", 0755) }},
	{"symlink", func(ufs *UnionFS, dir string) error { return ufs.Symlink("x", dir+"/planted") }},
}

// A directory the lookup cache holds that is replaced, around the union,
// by a link climbing out of the writable layer, at the spelling the union
// cached or another the host takes for it (issue #12). The layer is basefs
// over the host. From NewFSUnconfined, and from NewFS before Go 1.25, it
// is not confined, and the host follows the link wherever the union hands
// it a path through it; from NewFS on Go 1.25 or later the host itself
// refuses. A change through the cached directory must not
// reach what the link points at: it fails with ESTALE, and a retry
// resolves the link through the union, where it leads nowhere.
func TestLookupCacheOutOfBandLink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("host-directory layers are exercised on Unix")
	}
	for _, confine := range []bool{false, true} {
		for _, sp := range []struct{ name, cached, alias string }{
			{"same", "/d", "/d"},
			{"case", "/d", "/D"},
			{"nfd", "/é", "/é"},
		} {
			for _, w := range outOfBandWrites {
				name := "NewFSUnconfined/" + sp.name + "/" + w.name
				if confine {
					name = "NewFS/" + sp.name + "/" + w.name
				}
				t.Run(name, func(t *testing.T) {
					testOutOfBandLink(t, confine, sp.cached, sp.alias, w.run)
				})
			}
		}
	}
}

func testOutOfBandLink(t *testing.T, confine bool, cached, alias string, run func(*UnionFS, string) error) {
	root, err := filepath.EvalSymlinks(t.TempDir())
	must(t, err)
	dirs := map[string]string{}
	for _, d := range []string{"upper", "lower", "outside"} {
		dirs[d] = filepath.Join(root, d)
		must(t, os.Mkdir(dirs[d], 0755))
	}
	canary := filepath.Join(dirs["outside"], "canary")
	must(t, os.WriteFile(canary, []byte(canaryData), 0600))
	must(t, os.Chtimes(canary, canaryTime, canaryTime))
	before, err := os.Lstat(canary)
	must(t, err)
	host, err := osfs.NewFS()
	must(t, err)
	newFS := basefs.NewFSUnconfined
	if confine {
		newFS = basefs.NewFS
	}
	upper, err := newFS(host, dirs["upper"])
	must(t, err)
	lower, err := newFS(host, dirs["lower"])
	must(t, err)
	if !confine && basefs.Confined(upper) {
		t.Fatal("NewFSUnconfined is confined")
	}
	ufs := New(WithWritableLayer(upper), WithReadOnlyLayer(lower), WithLookupCache(time.Minute, 0))

	must(t, ufs.Mkdir(cached, 0755))
	if _, err := ufs.Stat(cached + "/nothing"); !os.IsNotExist(err) {
		t.Fatalf("Stat %s/nothing: %v", cached, err)
	}
	// Around the union: the directory goes, and a link climbing out of the
	// layer takes its place.
	if err := os.Remove(filepath.Join(dirs["upper"], alias[1:])); err != nil {
		t.Skipf("the host does not take %q for %q: %v", alias, cached, err)
	}
	must(t, os.Symlink("../outside", filepath.Join(dirs["upper"], alias[1:])))
	if !confine {
		// The layer itself follows the link out.
		if _, err := upper.Lstat(cached + "/canary"); err != nil {
			t.Fatalf("the unconfined layer does not reach the canary: %v", err)
		}
	}

	err = run(ufs, cached)
	t.Logf("first: %v", err)
	if !basefs.Confined(upper) && !errors.Is(err, syscall.ESTALE) {
		t.Errorf("the change through the stale directory: %v, want ESTALE", err)
	}
	// The cache was emptied: a retry sees the link, resolved against the
	// union, where it leads nowhere.
	err = run(ufs, cached)
	t.Logf("retry: %v", err)

	after, err := os.Lstat(canary)
	must(t, err)
	data, err := os.ReadFile(canary)
	must(t, err)
	if string(data) != canaryData || after.Mode() != before.Mode() || !after.ModTime().Equal(before.ModTime()) {
		t.Fatalf("canary outside the layer changed: %q %v %v", data, after.Mode(), after.ModTime())
	}
	ents, err := os.ReadDir(dirs["outside"])
	must(t, err)
	if len(ents) != 1 {
		t.Fatalf("outside the layer: %d entries, want only the canary", len(ents))
	}
	if _, err := os.Lstat(filepath.Join(root, "moved")); !os.IsNotExist(err) {
		t.Fatalf("moved canary next to the layer: %v", err)
	}
}

// confirmDirs fails a change when a directory above the name cannot be
// checked, and changes nothing.
func TestConfirmDirsLstatError(t *testing.T) {
	upper := newFaultFS(t)
	ufs := New(WithWritableLayer(upper), WithLookupCache(time.Minute, 0))
	must(t, ufs.MkdirAll("/a/b", 0755))
	if _, err := ufs.Stat("/a/b/nothing"); !os.IsNotExist(err) {
		t.Fatalf("Stat /a/b/nothing: %v", err)
	}
	// The walk to /a/b takes /a from the cache; only the check reads it.
	upper.failOn("lstat", "/a")
	f, err := ufs.Create("/a/b/f")
	if err == nil {
		f.Close()
	}
	if !errors.Is(err, errInjected) {
		t.Fatalf("Create /a/b/f: %v, want the injected fault", err)
	}
	upper.clear()
	if _, err := upper.Lstat("/a/b/f"); !os.IsNotExist(err) {
		t.Fatalf("/a/b/f was created: %v", err)
	}
}

// A change under directories that are all still there goes ahead, and one
// under a directory removed around the union fails with ENOENT.
func TestConfirmDirs(t *testing.T) {
	upper, err := memfs.NewFS()
	must(t, err)
	ufs := New(WithWritableLayer(upper), WithLookupCache(time.Minute, 0))
	must(t, ufs.MkdirAll("/a/b/c", 0755))
	put(t, ufs, "/a/b/c/f", "x")
	must(t, ufs.Chmod("/a/b/c/f", 0600))
	if info, err := upper.Lstat("/a/b/c/f"); err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("/a/b/c/f: %v, %v", info, err)
	}

	if _, err := ufs.Stat("/a/b/c/nothing"); !os.IsNotExist(err) {
		t.Fatalf("Stat /a/b/c/nothing: %v", err)
	}
	must(t, upper.RemoveAll("/a/b"))
	f, err := ufs.Create("/a/b/c/g")
	if err == nil {
		f.Close()
	}
	if !os.IsNotExist(err) {
		t.Fatalf("Create /a/b/c/g: %v, want ENOENT", err)
	}
	if _, err := upper.Lstat("/a/b"); !os.IsNotExist(err) {
		t.Fatalf("/a/b came back: %v", err)
	}
}
