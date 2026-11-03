package unionfs

import (
	"os"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"

	"github.com/absfs/absfs"
	"github.com/absfs/basefs"
	"github.com/absfs/fstesting"
	"github.com/absfs/osfs"
)

// osLayers returns a union of two host directories, as C4's NFS workspace
// prototype composes them: basefs over osfs for a writable upper and a
// read-only lower. It returns the union and both host directories.
func osLayers(t *testing.T) (*UnionFS, string, string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		t.Skip("host-directory layers are exercised on Unix")
	}
	host, err := osfs.NewFS()
	if err != nil {
		t.Fatal(err)
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	upperDir, lowerDir := filepath.Join(root, "upper"), filepath.Join(root, "lower")
	for _, d := range []string{upperDir, lowerDir} {
		if err := os.Mkdir(d, 0755); err != nil {
			t.Fatal(err)
		}
	}
	upper, err := basefs.NewFS(host, upperDir)
	if err != nil {
		t.Fatal(err)
	}
	lower, err := basefs.NewFS(host, lowerDir)
	if err != nil {
		t.Fatal(err)
	}
	return New(WithWritableLayer(upper), WithReadOnlyLayer(lower)), upperDir, lowerDir
}

// hostTree lists every path under dir on the host, with "/" after
// directories and "@" after symlinks, sorted.
func hostTree(t *testing.T, dir string) []string {
	t.Helper()
	var out []string
	err := filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if p == dir {
			return nil
		}
		rel := "/" + filepath.ToSlash(strings.TrimPrefix(p, dir+string(filepath.Separator)))
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			rel += "@"
		case info.IsDir():
			rel += "/"
		}
		out = append(out, rel)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(out)
	if out == nil {
		out = []string{}
	}
	return out
}

func hostWrite(t *testing.T, name, data string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(name), 0755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(name, []byte(data), 0644); err != nil {
		t.Fatal(err)
	}
}

func TestOSLayersSuite(t *testing.T) {
	ufs, _, _ := osLayers(t)
	suite := &fstesting.Suite{
		FS: ufs,
		Features: fstesting.Features{
			Symlinks:      true,
			Permissions:   true,
			Timestamps:    true,
			CaseSensitive: runtime.GOOS != "darwin",
			AtomicRename:  true,
			LargeFiles:    true,
		},
	}
	suite.Run(t)
}

// TestOSLayersWorkspace replays what a developer does in a C4 workspace,
// over host-directory layers, and checks the upper holds exactly the
// changes and the lower is untouched.
func TestOSLayersWorkspace(t *testing.T) {
	ufs, upperDir, lowerDir := osLayers(t)
	hostWrite(t, filepath.Join(lowerDir, "go.mod"), "module x\n")
	hostWrite(t, filepath.Join(lowerDir, "pkg", "a.go"), "package pkg\n")
	hostWrite(t, filepath.Join(lowerDir, "pkg", "b.go"), "package pkg\n")
	hostWrite(t, filepath.Join(lowerDir, "pkg", "internal", "c.go"), "package internal\n")
	hostWrite(t, filepath.Join(lowerDir, "docs", "x.md"), "x\n")
	hostWrite(t, filepath.Join(lowerDir, "old", "y.md"), "y\n")
	if err := os.Symlink("pkg/a.go", filepath.Join(lowerDir, "alink")); err != nil {
		t.Fatal(err)
	}
	before := hostTree(t, lowerDir)

	// rm of each file in a lower directory, then rmdir of it.
	must(t, ufs.Remove("/docs/x.md"))
	must(t, ufs.Remove("/docs"))
	// rm -r of a lower tree.
	must(t, ufs.RemoveAll("/old"))
	// An editor's save: write a temp file, rename it over the original.
	put(t, ufs, "/pkg/.a.go.swp", "package pkg // edited\n")
	must(t, ufs.Rename("/pkg/.a.go.swp", "/pkg/a.go"))
	// A new file, and one created and deleted again.
	put(t, ufs, "/pkg/new.go", "package pkg\n")
	put(t, ufs, "/scratch", "s")
	must(t, ufs.Remove("/scratch"))
	// mv of a directory with a deletion inside.
	must(t, ufs.Remove("/pkg/b.go"))
	must(t, ufs.Rename("/pkg/internal", "/pkg/inner"))
	// The link still resolves, now to the edited file.
	if get(t, ufs, "/alink") != "package pkg // edited\n" {
		t.Fatal("link through the union")
	}

	wantNames(t, list(t, ufs, "/"), "alink", "go.mod", "pkg")
	wantNames(t, list(t, ufs, "/pkg"), "a.go", "inner", "new.go")
	wantNames(t, list(t, ufs, "/pkg/inner"), "c.go")
	wantNames(t, hostTree(t, upperDir),
		"/.wh.docs", "/.wh.old",
		"/pkg/", "/pkg/.wh.b.go", "/pkg/.wh.internal", "/pkg/a.go",
		"/pkg/inner/", "/pkg/inner/c.go", "/pkg/new.go")
	wantNames(t, hostTree(t, lowerDir), before...)

	// Re-creating a deleted directory makes it opaque on disk.
	must(t, ufs.Mkdir("/old", 0755))
	wantNames(t, list(t, ufs, "/old"))
	if _, err := os.Stat(filepath.Join(upperDir, "old", OpaqueWhiteout)); err != nil {
		t.Fatalf("opaque marker on disk: %v", err)
	}
	if _, err := os.Stat(filepath.Join(upperDir, ".wh.old")); !os.IsNotExist(err) {
		t.Fatalf("whiteout left beside the new directory: %v", err)
	}
}

// TestOSLayersRenameOverDirWithWhiteouts moves a directory onto one whose
// merged view is empty but whose upper copy holds whiteouts: the host
// refuses to rename onto a non-empty directory, so the union clears it.
func TestOSLayersRenameOverDirWithWhiteouts(t *testing.T) {
	ufs, upperDir, lowerDir := osLayers(t)
	hostWrite(t, filepath.Join(lowerDir, "tgt", "gone"), "g")
	must(t, ufs.Remove("/tgt/gone"))
	must(t, ufs.Mkdir("/src", 0755))
	put(t, ufs, "/src/f", "f")
	must(t, ufs.Rename("/src", "/tgt"))
	wantNames(t, list(t, ufs, "/tgt"), "f")
	wantNames(t, hostTree(t, upperDir), "/tgt/", "/tgt/"+OpaqueWhiteout, "/tgt/f")
}

var _ absfs.SymlinkFileSystem = (*basefs.SymlinkFileSystem)(nil)
