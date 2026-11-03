package unionfs

import (
	"errors"
	"fmt"
	"io"
	"os"
	"sync"
	"syscall"
	"testing"
	"time"
)

func TestConcurrentChanges(t *testing.T) {
	ufs, _, lower := newTestUnion(t)
	lowerTree(t, lower)
	for i := 0; i < 20; i++ {
		put(t, lower, fmt.Sprintf("/d/f%d", i), "x")
	}

	var wg sync.WaitGroup
	for w := 0; w < 8; w++ {
		w := w
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := w; i < 20; i += 8 {
				name := fmt.Sprintf("/d/f%d", i)
				_ = ufs.Chmod(name, 0600)
				if f, err := ufs.OpenFile(name, os.O_WRONLY|os.O_APPEND, 0); err == nil {
					_, _ = f.Write([]byte("y"))
					_ = f.Close()
				}
				_, _ = ufs.Stat(name)
				_, _ = ufs.ReadDir("/d")
				_ = ufs.Rename(name, name+".moved")
				_ = ufs.Remove(name + ".moved")
				_ = ufs.MkdirAll(fmt.Sprintf("/w%d/x", w), 0755)
				_ = ufs.RemoveAll(fmt.Sprintf("/w%d", w))
			}
		}()
	}
	wg.Wait()
	wantNames(t, list(t, ufs, "/d"), "a", "b", "sub")
	wantNames(t, list(t, ufs, "/"), "d", "top")
}

func TestStatCacheFollowsChanges(t *testing.T) {
	upper, lower := newFaultFS(t), newFaultFS(t)
	lowerTree(t, lower)
	ufs := New(WithWritableLayer(upper), WithReadOnlyLayer(lower), WithStatCache(true, time.Hour))

	if _, err := ufs.Stat("/d/a"); err != nil {
		t.Fatal(err)
	}
	if _, err := ufs.Stat("/missing"); !os.IsNotExist(err) {
		t.Fatal(err)
	}
	if s := ufs.CacheStats(); s.StatCacheSize != 1 || s.NegativeCacheSize != 1 {
		t.Fatalf("cache stats %+v", s)
	}

	// Served from the cache: a failing layer is not consulted.
	lower.failOn("lstat", "/d/a")
	if _, err := ufs.Stat("/d/a"); err != nil {
		t.Fatalf("cached Stat: %v", err)
	}
	lower.clear()

	// Changes through the union drop what they touch.
	must(t, ufs.RemoveAll("/d"))
	wantNotExist(t, ufs, "/d/a")
	put(t, ufs, "/missing", "now")
	if _, err := ufs.Stat("/missing"); err != nil {
		t.Fatalf("Stat after create: %v", err)
	}

	// Direct layer changes need an explicit invalidation.
	put(t, upper, "/direct", "d")
	if _, err := ufs.Stat("/direct"); err != nil {
		t.Fatal(err)
	}
	must(t, upper.Remove("/direct"))
	if _, err := ufs.Stat("/direct"); err != nil {
		t.Fatal("expected the stale cached entry")
	}
	ufs.InvalidateCache("/direct")
	wantNotExist(t, ufs, "/direct")
	mustMkdirAll(t, upper, "/tree")
	put(t, upper, "/tree/x", "x")
	ufs.InvalidateCacheTree("/tree")
	if _, err := ufs.Stat("/tree/x"); err != nil {
		t.Fatal(err)
	}
	ufs.ClearCache()
	if s := ufs.CacheStats(); s.StatCacheSize != 0 {
		t.Fatalf("cache not cleared: %+v", s)
	}
}

func TestOpenDirectoryHandle(t *testing.T) {
	ufs, _, lower := newTestUnion(t)
	lowerTree(t, lower)
	put(t, ufs, "/d/z", "z")
	must(t, ufs.Remove("/d/b"))

	f, err := ufs.Open("/d")
	if err != nil {
		t.Fatal(err)
	}
	ents, err := f.ReadDir(2)
	if err != nil || len(ents) != 2 || ents[0].Name() != "a" || ents[1].Name() != "sub" {
		t.Fatalf("ReadDir(2) = %v, %v", ents, err)
	}
	ents, err = f.ReadDir(2)
	if err != nil || len(ents) != 1 || ents[0].Name() != "z" {
		t.Fatalf("ReadDir(2) again = %v, %v", ents, err)
	}
	if _, err := f.ReadDir(1); err != io.EOF {
		t.Fatalf("ReadDir at end: %v", err)
	}
	if ents, err := f.ReadDir(-1); err != nil || len(ents) != 0 {
		t.Fatalf("ReadDir(-1) at end: %v, %v", ents, err)
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	if ents, err := f.ReadDir(-1); err != nil || len(ents) != 3 {
		t.Fatalf("ReadDir(-1) after Seek: %v, %v", ents, err)
	}
	info, err := f.Stat()
	if err != nil || !info.IsDir() || info.Name() != "d" {
		t.Fatalf("Stat = %v, %v", info, err)
	}
	must(t, f.Close())
	if _, err := f.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("Stat after Close: %v", err)
	}
	if _, err := f.ReadDir(-1); !errors.Is(err, os.ErrClosed) {
		t.Fatalf("ReadDir after Close: %v", err)
	}

	// A directory removed after it was opened lists as an error.
	g, err := ufs.Open("/d/sub")
	if err != nil {
		t.Fatal(err)
	}
	must(t, ufs.RemoveAll("/d/sub"))
	if _, err := g.ReadDir(-1); !os.IsNotExist(err) {
		t.Fatalf("ReadDir of a removed directory: %v", err)
	}
}

func TestMiscErrors(t *testing.T) {
	ufs, _, lower := newTestUnion(t)
	lowerTree(t, lower)
	if _, err := ufs.ReadDir("/top"); !errors.Is(err, syscall.ENOTDIR) {
		t.Fatalf("ReadDir of a file: %v", err)
	}
	if _, err := ufs.ReadFile("/d"); !errors.Is(err, syscall.EISDIR) {
		t.Fatalf("ReadFile of a dir: %v", err)
	}
	if _, err := ufs.Stat("/top/x"); !errors.Is(err, syscall.ENOTDIR) {
		t.Fatalf("Stat through a file: %v", err)
	}
	if _, err := ufs.Create("/"); !errors.Is(err, syscall.EISDIR) {
		t.Fatalf("Create /: %v", err)
	}
	if err := ufs.Mkdir("/", 0755); !os.IsExist(err) {
		t.Fatalf("Mkdir /: %v", err)
	}
	if err := ufs.Rename("/top", "/"); err == nil {
		t.Fatal("Rename onto /")
	}
	// A chain of links longer than the limit.
	must(t, ufs.Symlink("/top", "/l0"))
	for i := 1; i <= maxSymlinks; i++ {
		must(t, ufs.Symlink(fmt.Sprintf("/l%d", i-1), fmt.Sprintf("/l%d", i)))
	}
	if _, err := ufs.Stat(fmt.Sprintf("/l%d", maxSymlinks-1)); err != nil {
		t.Fatalf("Stat at the limit: %v", err)
	}
	if _, err := ufs.Stat(fmt.Sprintf("/l%d", maxSymlinks)); !errors.Is(err, syscall.ELOOP) {
		t.Fatalf("Stat past the limit: %v", err)
	}
	// Dangling chains for create.
	must(t, ufs.Symlink("/m1", "/m0"))
	must(t, ufs.Symlink("/m0", "/m1"))
	if _, err := ufs.Create("/m0"); !errors.Is(err, syscall.ELOOP) {
		t.Fatalf("Create through a dangling loop: %v", err)
	}
	must(t, ufs.Symlink("rel-target", "/d/rel"))
	put(t, ufs, "/d/rel", "r")
	if get(t, ufs, "/d/rel-target") != "r" {
		t.Fatal("create through a relative dangling link")
	}

	// The view's Lchown.
	v := ufs.SymlinkFileSystem()
	must(t, v.Lchown("/d/rel", os.Getuid(), os.Getgid()))
	if ufs.Name() != "UnionFS" {
		t.Fatal("Name")
	}

	// MkdirAll follows links to directories, and stops at links to files.
	symlink(t, lower, "/d", "/dl")
	must(t, ufs.MkdirAll("/dl/made/deep", 0755))
	if info, err := ufs.Stat("/d/made/deep"); err != nil || !info.IsDir() {
		t.Fatalf("MkdirAll through a link: %v, %v", info, err)
	}
	symlink(t, lower, "/top", "/tl")
	if err := ufs.MkdirAll("/tl/x", 0755); !errors.Is(err, syscall.ENOTDIR) {
		t.Fatalf("MkdirAll through a link to a file: %v", err)
	}
	if err := ufs.MkdirAll("/m0/x", 0755); !errors.Is(err, syscall.ELOOP) {
		t.Fatalf("MkdirAll through a loop: %v", err)
	}
}
