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

// outOfBandWrites are the changes made through a name the lookup cache
// holds that has since become, around the union, a link climbing out of
// the writable layer. canary is a name whose host path reaches the canary
// outside the layer, planted a name the union holds as missing whose host
// path reaches outside it. Each change must leave the outside alone. One
// marked follows is one the layer would make through a link at the name
// itself, not to the link, from the union's cached view of the name, and
// fails with ESTALE when only the name itself was replaced. Create is not:
// the union looks a name it creates up again, and resolves a link there
// itself.
var outOfBandWrites = []struct {
	name    string
	follows bool
	run     func(ufs *UnionFS, canary, planted string) error
}{
	{"open-trunc", true, func(ufs *UnionFS, canary, _ string) error {
		f, err := ufs.OpenFile(canary, os.O_WRONLY|os.O_TRUNC, 0)
		if err == nil {
			f.Write([]byte("changed"))
			f.Close()
		}
		return err
	}},
	{"open-append", true, func(ufs *UnionFS, canary, _ string) error {
		f, err := ufs.OpenFile(canary, os.O_WRONLY|os.O_APPEND, 0)
		if err == nil {
			f.Write([]byte("changed"))
			f.Close()
		}
		return err
	}},
	{"create-over", true, func(ufs *UnionFS, canary, _ string) error {
		f, err := ufs.Create(canary)
		if err == nil {
			f.Close()
		}
		return err
	}},
	{"create", false, func(ufs *UnionFS, _, planted string) error {
		f, err := ufs.Create(planted)
		if err == nil {
			f.Close()
		}
		return err
	}},
	{"chmod", true, func(ufs *UnionFS, canary, _ string) error { return ufs.Chmod(canary, 0777) }},
	{"chtimes", true, func(ufs *UnionFS, canary, _ string) error {
		return ufs.Chtimes(canary, time.Now(), time.Now())
	}},
	{"chown", true, func(ufs *UnionFS, canary, _ string) error {
		return ufs.Chown(canary, os.Getuid(), os.Getgid())
	}},
	{"lchown", false, func(ufs *UnionFS, canary, _ string) error {
		return ufs.Lchown(canary, os.Getuid(), os.Getgid())
	}},
	{"truncate", true, func(ufs *UnionFS, canary, _ string) error { return ufs.Truncate(canary, 0) }},
	{"remove", false, func(ufs *UnionFS, canary, _ string) error { return ufs.Remove(canary) }},
	{"removeall", false, func(ufs *UnionFS, canary, _ string) error { return ufs.RemoveAll(canary) }},
	{"rename-from", false, func(ufs *UnionFS, canary, _ string) error { return ufs.Rename(canary, "/moved") }},
	{"rename-to", false, func(ufs *UnionFS, canary, _ string) error {
		f, err := ufs.Create("/src")
		if err != nil {
			return err
		}
		f.Close()
		return ufs.Rename("/src", canary)
	}},
	{"mkdir", false, func(ufs *UnionFS, _, planted string) error { return ufs.Mkdir(planted, 0755) }},
	{"symlink", false, func(ufs *UnionFS, _, planted string) error { return ufs.Symlink("x", planted) }},
}

// A name the lookup cache holds that is replaced, around the union, by a
// link climbing out of the writable layer, at the spelling the union
// cached or another the host takes for it (issue #12): a directory, or a
// file and a name cached as missing. The layer is basefs over the host.
// From NewFSUnconfined, and from NewFS before Go 1.25, it is not confined,
// and the host follows the link wherever the union hands it a path through
// it; from NewFS on Go 1.25 or later the host itself refuses. A change
// through the cached name must not reach what the link points at: one the
// layer would make through the link fails with ESTALE, and a retry
// resolves the link through the union, where it leads nowhere.
func TestLookupCacheOutOfBandLink(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("host-directory layers are exercised on Unix")
	}
	for _, confine := range []bool{false, true} {
		for _, kind := range []string{"dir", "file"} {
			for _, sp := range []struct{ name, cached, alias string }{
				{"same", "d", "d"},
				{"case", "d", "D"},
				{"nfd", "\u00e9", "e\u0301"},
			} {
				for _, w := range outOfBandWrites {
					name := "NewFSUnconfined/"
					if confine {
						name = "NewFS/"
					}
					name += kind + "/" + sp.name + "/" + w.name
					t.Run(name, func(t *testing.T) {
						testOutOfBandLink(t, confine, kind == "file", sp.cached, sp.alias, w.follows, w.run)
					})
				}
			}
		}
	}
}

// testOutOfBandLink runs a change of outOfBandWrites through a cached
// directory /cached, or with file, through a cached file /d/cached and a
// name /d/cached-new cached as missing, which a host program replaces with
// a link out of the layer at the spelling alias.
func testOutOfBandLink(t *testing.T, confine, file bool, cached, alias string, follows bool, run func(*UnionFS, string, string) error) {
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

	// Through the union: the names are cached, and around it, each goes
	// and a link climbing out of the layer takes its place.
	var target, planted string
	var links [][2]string
	if file {
		must(t, ufs.Mkdir("/d", 0755))
		target, planted = "/d/"+cached, "/d/"+cached+"-new"
		put(t, ufs, target, "inside")
		if _, err := ufs.Stat(target); err != nil {
			t.Fatalf("Stat %s: %v", target, err)
		}
		links = [][2]string{
			{"../../outside/canary", filepath.Join(dirs["upper"], "d", alias)},
			{"../../outside/planted", filepath.Join(dirs["upper"], "d", alias+"-new")},
		}
	} else {
		must(t, ufs.Mkdir("/"+cached, 0755))
		target, planted = "/"+cached+"/canary", "/"+cached+"/planted"
		links = [][2]string{{"../outside", filepath.Join(dirs["upper"], alias)}}
	}
	if _, err := ufs.Stat(planted); !os.IsNotExist(err) {
		t.Fatalf("Stat %s: %v", planted, err)
	}
	if err := os.Remove(links[0][1]); err != nil {
		t.Skipf("the host does not take %q for %q: %v", alias, cached, err)
	}
	for _, l := range links {
		must(t, os.Symlink(l[0], l[1]))
	}
	if !confine {
		// The layer itself follows the link out.
		if _, err := upper.Stat(target); err != nil {
			t.Fatalf("the unconfined layer does not reach the canary: %v", err)
		}
	}

	err = run(ufs, target, planted)
	t.Logf("first: %v", err)
	if follows || !file {
		if !basefs.Confined(upper) && !errors.Is(err, syscall.ESTALE) {
			t.Errorf("the change through the stale name: %v, want ESTALE", err)
		}
	}
	// The cache was emptied: a retry sees the link, resolved against the
	// union, where it leads nowhere.
	err = run(ufs, target, planted)
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

// confirmPath fails a change that would follow a link at the name when the
// name itself cannot be checked, and changes nothing; a name that is
// missing is left to the change.
func TestConfirmPathLstatError(t *testing.T) {
	upper := newFaultFS(t)
	ufs := New(WithWritableLayer(upper), WithLookupCache(time.Minute, 0))
	must(t, ufs.MkdirAll("/a/b", 0755))
	put(t, ufs, "/a/b/f", "x")
	must(t, ufs.Chmod("/a/b/f", 0600))
	upper.failOn("lstat", "/a/b/f")
	if err := ufs.Chmod("/a/b/f", 0644); !errors.Is(err, errInjected) {
		t.Fatalf("Chmod /a/b/f: %v, want the injected fault", err)
	}
	upper.clear()
	if info, err := upper.Lstat("/a/b/f"); err != nil || info.Mode().Perm() != 0600 {
		t.Fatalf("/a/b/f: %v, %v", info, err)
	}
	if err := upper.Remove("/a/b/f"); err != nil {
		t.Fatal(err)
	}
	if err := ufs.Chmod("/a/b/f", 0644); !os.IsNotExist(err) {
		t.Fatalf("Chmod of /a/b/f removed around the union: %v, want ENOENT", err)
	}
}
