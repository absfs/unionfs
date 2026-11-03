package unionfs

import (
	"errors"
	"fmt"
	"io/fs"
	"math/rand"
	"os"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"

	"github.com/absfs/absfs"
)

// countFS wraps a layer and counts the calls the union makes to it.
type countFS struct {
	absfs.SymlinkFileSystem
	n atomic.Int64
}

func (c *countFS) count() int64 { return c.n.Swap(0) }

func (c *countFS) Stat(p string) (os.FileInfo, error) {
	c.n.Add(1)
	return c.SymlinkFileSystem.Stat(p)
}

func (c *countFS) Lstat(p string) (os.FileInfo, error) {
	c.n.Add(1)
	return c.SymlinkFileSystem.Lstat(p)
}

func (c *countFS) Readlink(p string) (string, error) {
	c.n.Add(1)
	return c.SymlinkFileSystem.Readlink(p)
}

func (c *countFS) ReadDir(p string) ([]fs.DirEntry, error) {
	c.n.Add(1)
	return c.SymlinkFileSystem.ReadDir(p)
}

func (c *countFS) Open(p string) (absfs.File, error) {
	c.n.Add(1)
	return c.SymlinkFileSystem.Open(p)
}

func (c *countFS) OpenFile(p string, flag int, perm os.FileMode) (absfs.File, error) {
	c.n.Add(1)
	return c.SymlinkFileSystem.OpenFile(p, flag, perm)
}

// countedUnion returns a union over counting layers: an empty memfs upper
// and a memfs lower holding the benchmark tree, with opts.
func countedUnion(t *testing.T, opts ...Option) (*UnionFS, *countFS, *countFS) {
	t.Helper()
	upper := &countFS{SymlinkFileSystem: mustNewMemFS().(absfs.SymlinkFileSystem)}
	lower := &countFS{SymlinkFileSystem: mustNewMemFS().(absfs.SymlinkFileSystem)}
	fillLstatTree(t, lower)
	ufs := New(append([]Option{WithWritableLayer(upper), WithReadOnlyLayer(lower)}, opts...)...)
	upper.count()
	lower.count()
	return ufs, upper, lower
}

// Lstat through the union costs one layer call per element and more
// without a cache; the lookup cache brings a repeated one down to a single
// call, and the Stat cache to none.
func TestLstatLayerCalls(t *testing.T) {
	for _, tc := range []struct {
		name string
		opts []Option
		want map[string]int64 // layer calls for a repeated Lstat
	}{
		{"NoCache", nil, map[string]int64{"/d001/f001.txt": 5, "/.git/objects/pack/p.idx": 7}},
		{"LookupCache", []Option{WithLookupCache(time.Hour, 0)}, map[string]int64{"/d001/f001.txt": 1, "/.git/objects/pack/p.idx": 1}},
		{"StatCache", []Option{WithStatCache(true, time.Hour)}, map[string]int64{"/d001/f001.txt": 0, "/.git/objects/pack/p.idx": 0}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ufs, upper, lower := countedUnion(t, tc.opts...)
			for p, want := range tc.want {
				for i := 0; i < 3; i++ {
					info, err := ufs.Lstat(p)
					must(t, err)
					if info.Size() == 0 {
						t.Fatalf("%s: size 0", p)
					}
					got := upper.count() + lower.count()
					if i > 0 && got != want {
						t.Errorf("Lstat %s, call %d: %d layer calls, want %d", p, i+1, got, want)
					}
				}
			}
		})
	}
	// Missing names are cached too.
	ufs, upper, lower := countedUnion(t, WithLookupCache(time.Hour, 0))
	for i := 0; i < 2; i++ {
		if _, err := ufs.Lstat("/d001/nope"); !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
	if got := upper.count() + lower.count(); got != 5 {
		t.Fatalf("two missing lookups cost %d layer calls, want 5 for the first, 0 for the second", got)
	}
}

// The lookup cache never serves attributes: a write through an open handle
// shows in the next Lstat.
func TestLookupCacheAttributesStayFresh(t *testing.T) {
	ufs, _, _ := countedUnion(t, WithLookupCache(time.Hour, 0))
	f, err := ufs.OpenFile("/d001/f001.txt", os.O_WRONLY|os.O_APPEND, 0)
	must(t, err)
	defer f.Close()
	for i := 1; i <= 3; i++ {
		_, err := f.Write([]byte("more"))
		must(t, err)
		if info := lstat(t, ufs, "/d001/f001.txt"); info.Size() != int64(1+4*i) {
			t.Fatalf("size %d after %d writes", info.Size(), i)
		}
	}
	must(t, ufs.Chmod("/d002/f002.txt", 0700))
	wantMode(t, "marked", lstat(t, ufs, "/d002/f002.txt"), 0700)
	must(t, ufs.Chmod("/d002/f002.txt", 0750))
	wantMode(t, "marked again", lstat(t, ufs, "/d002/f002.txt"), 0750)
}

// diffNames are the names TestLookupCacheDifferential changes and checks.
var diffNames = []string{"/a", "/b", "/d", "/d/a", "/d/b", "/d/e", "/d/e/a", "/e", "/e/a", "/l", "/d/l"}

// diffState records what a union shows for every name in diffNames.
func diffState(ufs *UnionFS) string {
	s := ""
	for _, p := range diffNames {
		for _, follow := range []bool{false, true} {
			var info os.FileInfo
			var err error
			if follow {
				info, err = ufs.Stat(p)
			} else {
				info, err = ufs.Lstat(p)
			}
			switch {
			case err != nil:
				s += fmt.Sprintf("%s %v: %v\n", p, follow, errName(err))
			default:
				s += fmt.Sprintf("%s %v: %v %d\n", p, follow, info.Mode(), info.Size())
			}
		}
		if ents, err := ufs.ReadDir(p); err == nil {
			for _, e := range ents {
				if info, err := e.Info(); err != nil {
					s += fmt.Sprintf("  %s: %v\n", e.Name(), errName(err))
				} else {
					s += fmt.Sprintf("  %s %v\n", e.Name(), info.Mode())
				}
			}
		}
	}
	return s
}

func errName(err error) string {
	for _, e := range []error{os.ErrNotExist, os.ErrExist, syscall.ENOTDIR, syscall.EISDIR, syscall.ENOTEMPTY, syscall.EINVAL, syscall.ELOOP, syscall.EPERM, syscall.EBUSY} {
		if os.IsNotExist(err) && e == os.ErrNotExist || os.IsExist(err) && e == os.ErrExist || errors.Is(err, e) {
			return e.Error()
		}
	}
	return "other: " + err.Error()
}

// diffOp applies change number i, drawn from r, to ufs.
func diffOp(ufs *UnionFS, r *rand.Rand) string {
	pick := func() string { return diffNames[r.Intn(len(diffNames))] }
	a, b := pick(), pick()
	when := time.Date(2020, 1, 1, 0, 0, r.Intn(1000), 0, time.UTC)
	var err error
	op := r.Intn(13)
	switch op {
	case 0:
		var f absfs.File
		if f, err = ufs.Create(a); err == nil {
			_, err = f.Write([]byte("created"))
			f.Close()
		}
	case 1:
		err = ufs.Mkdir(a, 0755)
	case 2:
		err = ufs.MkdirAll(a, 0755)
	case 3:
		err = ufs.Remove(a)
	case 4:
		err = ufs.RemoveAll(a)
	case 5:
		err = ufs.Rename(a, b)
	case 6:
		// The owner keeps full access, so a host directory stays
		// searchable and the test's cleanup can remove it.
		err = ufs.Chmod(a, os.FileMode(0700|r.Intn(2)*0055))
	case 7:
		err = ufs.Chtimes(a, when, when)
	case 8:
		err = ufs.Truncate(a, int64(r.Intn(3)))
	case 9:
		err = ufs.Symlink(b, a)
	case 10:
		var f absfs.File
		if f, err = ufs.OpenFile(a, os.O_WRONLY|os.O_APPEND, 0); err == nil {
			_, err = f.Write([]byte("+"))
			f.Close()
		}
	case 11:
		err = ufs.Lchown(a, os.Getuid(), os.Getgid())
	case 12:
		_, err = ufs.ReadFile(a)
	}
	res := "ok"
	if err != nil {
		res = errName(err)
	}
	return fmt.Sprintf("op %d %s %s: %s", op, a, b, res)
}

// diffLower fills a lower layer for the differential test.
func diffLower(t *testing.T, lower absfs.SymlinkFileSystem) {
	t.Helper()
	mustMkdirAll(t, lower, "/d/e")
	mustMkdirAll(t, lower, "/e")
	put(t, lower, "/a", "la")
	put(t, lower, "/d/a", "lda")
	put(t, lower, "/d/e/a", "ldea")
	put(t, lower, "/e/a", "lea")
	symlink(t, lower, "d", "/l")
}

// Two unions over identical layers, one with the lookup cache and one
// without, go through the same random changes and must show the same view
// after each: every change through the union drops what it affects.
func TestLookupCacheDifferential(t *testing.T) {
	for _, kind := range layerKinds {
		t.Run(kind, func(t *testing.T) {
			for seed := int64(1); seed <= 20; seed++ {
				_, upA, lowA := metaUnion(t, kind)
				_, upB, lowB := metaUnion(t, kind)
				for _, l := range []absfs.SymlinkFileSystem{lowA, lowB} {
					must(t, l.RemoveAll("/d"))
					diffLower(t, l)
				}
				plain := New(WithWritableLayer(upA), WithReadOnlyLayer(lowA))
				cached := New(WithWritableLayer(upB), WithReadOnlyLayer(lowB), WithLookupCache(time.Hour, 0))
				ra, rb := rand.New(rand.NewSource(seed)), rand.New(rand.NewSource(seed))
				for i := 0; i < 60; i++ {
					oa, ob := diffOp(plain, ra), diffOp(cached, rb)
					if oa != ob {
						t.Fatalf("seed %d step %d: %s without the cache, %s with it", seed, i, oa, ob)
					}
					sa, sb := diffState(plain), diffState(cached)
					if sa != sb {
						t.Fatalf("seed %d step %d after %s:\nwithout the cache:\n%s\nwith it:\n%s", seed, i, oa, sa, sb)
					}
				}
				if cached.CacheStats().LookupEntries == 0 {
					t.Fatal("the lookup cache held nothing")
				}
			}
		})
	}
}

// Changes made to a layer directly show once the entries expire, or after
// InvalidateCache, InvalidateCacheTree or ClearCache; a name removed from
// its layer is found missing at once.
func TestLookupCacheStaleness(t *testing.T) {
	upper, lower := mustNewMemFS().(absfs.SymlinkFileSystem), mustNewMemFS().(absfs.SymlinkFileSystem)
	lowerTree(t, lower)
	mustMkdirAll(t, upper, "/d")
	ufs := New(WithWritableLayer(upper), WithReadOnlyLayer(lower), WithLookupCache(time.Minute, 0))
	if _, err := ufs.Lstat("/d/new"); !os.IsNotExist(err) {
		t.Fatal(err)
	}
	_ = lstat(t, ufs, "/d/a")
	put(t, upper, "/d/new", "n")
	put(t, upper, "/d/a", "upper a")

	// Stale: the miss is cached, and so is the layer that held /d/a.
	if _, err := ufs.Lstat("/d/new"); !os.IsNotExist(err) {
		t.Fatalf("cached miss not served: %v", err)
	}
	if get(t, ufs, "/d/a") != "la" {
		t.Fatal("cached layer not served")
	}
	ufs.InvalidateCache("/d/new")
	_ = lstat(t, ufs, "/d/new")
	ufs.InvalidateCacheTree("/d")
	if get(t, ufs, "/d/a") != "upper a" {
		t.Fatal("not re-resolved after InvalidateCacheTree")
	}

	// A name gone from its layer is resolved again at once.
	must(t, upper.Remove("/d/a"))
	if get(t, ufs, "/d/a") != "la" {
		t.Fatal("lower not found again")
	}
	must(t, lower.Remove("/d/b"))
	if _, err := ufs.Lstat("/d/b"); !os.IsNotExist(err) {
		t.Fatalf("removed name: %v", err)
	}
	// So is one whose type changed.
	_ = lstat(t, ufs, "/top")
	must(t, lower.Remove("/top"))
	mustMkdirAll(t, lower, "/top")
	if !lstat(t, ufs, "/top").IsDir() {
		t.Fatal("type change not seen")
	}
	// And a marked file whose marker went.
	must(t, ufs.Chmod("/d/sub/c", 0700))
	_ = lstat(t, ufs, "/d/sub/c")
	must(t, upper.Remove("/d/sub/.wh..wh.meta.c"))
	wantMode(t, "unmarked", lstat(t, ufs, "/d/sub/c"), 0644)

	// Expiry.
	for _, l := range []absfs.FileSystem{nil, lower, upper} {
		if l != nil {
			put(t, l, "/late", "l")
		}
		if _, err := ufs.Lstat("/late"); !os.IsNotExist(err) {
			t.Fatalf("/late: %v", err)
		}
	}
	later := time.Now().Add(time.Hour)
	ufs.lc.now = func() time.Time { return later }
	_ = lstat(t, ufs, "/late")
	for i := 0; i < 2; i++ {
		if _, err := ufs.Lstat("/later"); err == nil {
			t.Fatal("not cached")
		}
		put(t, upper, "/later", "l")
	}
	ufs.ClearCache()
	_ = lstat(t, ufs, "/later")
	if st := ufs.CacheStats(); !st.LookupEnabled || st.LookupTTL != time.Minute || st.LookupEntries == 0 {
		t.Fatalf("stats %+v", st)
	}
}

// A layer error while re-reading a cached name is reported.
func TestLookupCacheLayerErrors(t *testing.T) {
	upper, lower := newFaultFS(t), newFaultFS(t)
	lowerTree(t, lower)
	ufs := New(WithWritableLayer(upper), WithReadOnlyLayer(lower), WithLookupCache(time.Hour, 0))
	must(t, ufs.Chmod("/d/a", 0700))
	_ = lstat(t, ufs, "/d/a")
	_ = lstat(t, ufs, "/d/b")
	lower.failOn("lstat", "/d/b")
	_, err := ufs.Lstat("/d/b")
	wantInjected(t, "cached file", err)
	upper.failOn("lstat", "/d/.wh..wh.meta.a")
	_, err = ufs.Lstat("/d/a")
	wantInjected(t, "cached marker", err)
	upper.clear()
	upper.failOn("stat", "/")
	_, err = ufs.Stat("/")
	wantInjected(t, "root", err)
	upper.clear()
	lower.clear()
	upper.failOn("lstat", "/.wh.__dir_opaque")
	ufs.ClearCache()
	_, err = ufs.Stat("/")
	wantInjected(t, "root opaque probe", err)
}

// The cache is bounded: when it fills it starts over.
func TestLookupCacheBound(t *testing.T) {
	ufs, _, _ := countedUnion(t, WithLookupCache(time.Hour, 50))
	for d := 1; d <= 3; d++ {
		for f := 1; f <= 100; f++ {
			_ = lstat(t, ufs, fmt.Sprintf("/d%03d/f%03d.txt", d, f))
			if n := ufs.CacheStats().LookupEntries; n > 50 {
				t.Fatalf("%d entries", n)
			}
		}
	}
	if New(WithLookupCache(0, 10)).lc != nil || New(WithLookupCache(time.Hour, 5), WithLookupCache(-1, 5)).lc != nil {
		t.Fatal("a zero or negative ttl left the cache on")
	}
	if c := newLookupCache(time.Hour, 0); c.max != defaultLookupEntries {
		t.Fatalf("default bound %d", c.max)
	}
}

// The tree's bookkeeping: puts under a node the cache no longer holds are
// dropped, and dropping a path that is not cached does nothing.
func TestLookupCacheTree(t *testing.T) {
	c := newLookupCache(time.Hour, 0)
	root := &node{path: "/"}
	d := &node{path: "/d", parent: root}
	c.put(root, "d", d) // no root yet
	if c.len() != 0 {
		t.Fatal("put without a root")
	}
	c.invalidate("/d") // nothing cached
	c.putRoot(root)
	c.put(root, "d", d)
	c.put(d, "a", nil)
	c.put(d, "b", nil)
	c.put(&node{path: "/d"}, "c", nil) // another node for /d
	if c.len() != 3 {
		t.Fatalf("len %d, want 3", c.len())
	}
	c.put(root, "d", d) // replaces /d and everything under it
	if c.len() != 1 {
		t.Fatalf("len %d after replacing /d", c.len())
	}
	c.put(d, "a", nil)
	c.invalidate("/x/y")
	c.invalidate("/d/a/b")
	c.invalidate("/d")
	if c.len() != 0 {
		t.Fatalf("len %d after dropping /d", c.len())
	}
	if _, ok := c.get(d, "a"); ok {
		t.Fatal("dropped entry served")
	}
	c.put(root, "d", d)
	c.put(d, "a", nil)
	later := time.Now().Add(2 * time.Hour)
	c.now = func() time.Time { return later }
	if _, ok := c.get(d, "a"); ok || c.getRoot() != nil {
		t.Fatal("expired entry served")
	}
	c.invalidate("/")
	if c.len() != 0 {
		t.Fatal("not emptied")
	}
	// A full cache starts over; a put under a node it then drops is
	// discarded.
	c = newLookupCache(time.Hour, 2)
	c.putRoot(root)
	c.put(root, "d", d)
	c.put(d, "a", nil)
	c.put(d, "b", nil)
	if c.len() != 0 {
		t.Fatalf("len %d after a put into a full cache", c.len())
	}
	c.put(root, "e", nil)
	if c.len() != 1 {
		t.Fatalf("len %d", c.len())
	}
}

// The Stat cache holds Lstat results and misses, and drops them with the
// Stat entries.
func TestStatCacheLstat(t *testing.T) {
	ufs, upper, lower := countedUnion(t, WithStatCache(true, time.Hour))
	symlink(t, lower, "d001", "/link")
	for i := 0; i < 2; i++ {
		if !isSymlink(lstat(t, ufs, "/link")) || !stat(t, ufs, "/link").IsDir() {
			t.Fatal("Lstat and Stat of a link share an entry")
		}
		if _, err := ufs.Lstat("/missing"); !os.IsNotExist(err) {
			t.Fatal(err)
		}
	}
	upper.count()
	lower.count()
	_ = lstat(t, ufs, "/link")
	_, _ = ufs.Lstat("/missing")
	if n := upper.count() + lower.count(); n != 0 {
		t.Fatalf("%d layer calls for cached Lstats", n)
	}
	must(t, ufs.Chmod("/d001/f001.txt", 0700))
	_ = lstat(t, ufs, "/d001/f001.txt")
	must(t, ufs.Chmod("/d001/f001.txt", 0750))
	wantMode(t, "after Chmod", lstat(t, ufs, "/d001/f001.txt"), 0750)
	put(t, upper, "/missing", "now")
	ufs.InvalidateCache("/missing")
	_ = lstat(t, ufs, "/missing")
}

// Concurrent lookups and changes with the lookup cache on.
func TestLookupCacheConcurrent(t *testing.T) {
	for _, kind := range layerKinds {
		t.Run(kind, func(t *testing.T) {
			_, upper, lower := metaUnion(t, kind)
			ufs := New(WithWritableLayer(upper), WithReadOnlyLayer(lower), WithLookupCache(time.Hour, 20))
			var wg sync.WaitGroup
			for g := 0; g < 8; g++ {
				wg.Add(1)
				go func(g int) {
					defer wg.Done()
					for i := 0; i < 40; i++ {
						name := fmt.Sprintf("/d/g%d-%d", g, i%4)
						switch (g + i) % 6 {
						case 0:
							put(t, ufs, name, "x")
						case 1:
							_ = ufs.Rename(name, name+"r")
						case 2:
							_ = ufs.Remove(name + "r")
						case 3:
							_ = ufs.Chmod("/d/a", os.FileMode(0600|i%2*0100))
						case 4:
							if _, err := ufs.Lstat("/d/sub/c"); err != nil {
								t.Errorf("Lstat: %v", err)
							}
						case 5:
							_, _ = ufs.ReadDir("/d")
						}
					}
				}(g)
			}
			wg.Wait()
			plain := New(WithWritableLayer(upper), WithReadOnlyLayer(lower))
			if a, b := list(t, plain, "/d"), list(t, ufs, "/d"); fmt.Sprint(a) != fmt.Sprint(b) {
				t.Fatalf("cached view %q, uncached %q", b, a)
			}
		})
	}
}
