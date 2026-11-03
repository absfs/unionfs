package unionfs

import (
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/absfs/absfs"
	"github.com/absfs/basefs"
	"github.com/absfs/memfs"
	"github.com/absfs/osfs"
)

// lstatPaths are the paths the Lstat benchmarks look up: a file two levels
// down, and a git pack index four levels down, in a lower the upper has not
// touched.
var lstatPaths = []string{"/d001/f001.txt", "/.git/objects/pack/p.idx"}

// fillLstatTree fills fs with /d001../d010, each holding f001.txt..f100.txt,
// and /.git/objects/pack/p.idx.
func fillLstatTree(tb testing.TB, fs absfs.FileSystem) {
	tb.Helper()
	for d := 1; d <= 10; d++ {
		dir := fmt.Sprintf("/d%03d", d)
		if err := fs.MkdirAll(dir, 0755); err != nil {
			tb.Fatal(err)
		}
		for f := 1; f <= 100; f++ {
			if err := writeFile(fs, fmt.Sprintf("%s/f%03d.txt", dir, f), []byte("x"), 0644); err != nil {
				tb.Fatal(err)
			}
		}
	}
	if err := writeFile(fs, "/.git/objects/pack/p.idx", []byte("idx"), 0644); err != nil {
		tb.Fatal(err)
	}
}

// lstatLayers returns an empty upper and a filled lower of the given kind,
// "memfs" or "osfs" (basefs over host directories), and, for osfs, the
// lower's host directory.
func lstatLayers(tb testing.TB, kind string) (absfs.SymlinkFileSystem, absfs.SymlinkFileSystem, string) {
	tb.Helper()
	if kind == "memfs" {
		u, err := memfs.NewFS()
		if err != nil {
			tb.Fatal(err)
		}
		l, err := memfs.NewFS()
		if err != nil {
			tb.Fatal(err)
		}
		fillLstatTree(tb, l)
		return u, l, ""
	}
	if runtime.GOOS == "windows" {
		tb.Skip("host-directory layers are exercised on Unix")
	}
	host, err := osfs.NewFS()
	if err != nil {
		tb.Fatal(err)
	}
	root, err := filepath.EvalSymlinks(tb.TempDir())
	if err != nil {
		tb.Fatal(err)
	}
	var layers [2]absfs.SymlinkFileSystem
	for i, d := range []string{"upper", "lower"} {
		if err := os.Mkdir(filepath.Join(root, d), 0755); err != nil {
			tb.Fatal(err)
		}
		fs, err := basefs.NewFS(host, filepath.Join(root, d))
		if err != nil {
			tb.Fatal(err)
		}
		layers[i] = fs
	}
	fillLstatTree(tb, layers[1])
	return layers[0], layers[1], filepath.Join(root, "lower")
}

// benchLstat runs Lstat of each lstatPaths entry through fs.
func benchLstat(b *testing.B, fs absfs.SymlinkFileSystem) {
	for _, p := range lstatPaths {
		b.Run(p, func(b *testing.B) {
			if _, err := fs.Lstat(p); err != nil {
				b.Fatal(err)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				if _, err := fs.Lstat(p); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkLstat measures Lstat through a two-layer union of memfs layers
// and of host directories, with no cache, with the Stat cache and with the
// lookup cache, and through the lower layer alone for comparison.
func BenchmarkLstat(b *testing.B) {
	for _, kind := range []string{"memfs", "osfs"} {
		b.Run(kind, func(b *testing.B) {
			upper, lower, _ := lstatLayers(b, kind)
			b.Run("LowerAlone", func(b *testing.B) { benchLstat(b, lower) })
			b.Run("NoCache", func(b *testing.B) {
				benchLstat(b, New(WithWritableLayer(upper), WithReadOnlyLayer(lower)))
			})
			b.Run("StatCache", func(b *testing.B) {
				benchLstat(b, New(WithWritableLayer(upper), WithReadOnlyLayer(lower),
					WithStatCache(true, time.Hour)))
			})
			b.Run("LookupCache", func(b *testing.B) {
				benchLstat(b, New(WithWritableLayer(upper), WithReadOnlyLayer(lower),
					WithLookupCache(time.Hour, 0)))
			})
		})
	}
}
