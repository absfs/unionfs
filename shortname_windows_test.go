//go:build windows

package unionfs

import (
	"errors"
	"os"
	"path/filepath"
	"syscall"
	"testing"

	"github.com/absfs/basefs"
	"github.com/absfs/osfs"
)

// shortName returns the 8.3 short name the host gave the file at p, or ""
// when it gave none (8.3 names disabled on the volume).
func shortName(t *testing.T, p string) string {
	t.Helper()
	long, err := syscall.UTF16PtrFromString(p)
	must(t, err)
	buf := make([]uint16, syscall.MAX_PATH)
	n, err := syscall.GetShortPathName(long, &buf[0], uint32(len(buf)))
	must(t, err)
	s := filepath.Base(syscall.UTF16ToString(buf[:n]))
	if s == filepath.Base(p) {
		return ""
	}
	return s
}

// TestShortNameAliasOfMarker checks that a marker in a host-directory
// writable layer cannot be reached through the 8.3 short name NTFS gives
// it (".wh.x" is also "WH~1.X" where 8.3 names are on).
func TestShortNameAliasOfMarker(t *testing.T) {
	host, err := osfs.NewFS()
	must(t, err)
	root := t.TempDir()
	var dirs [2]string
	var layers [2]*basefs.SymlinkFileSystem
	for i, d := range []string{"upper", "lower"} {
		dirs[i] = filepath.Join(root, d)
		must(t, os.Mkdir(dirs[i], 0755))
		l, err := basefs.NewFS(host, dirs[i])
		must(t, err)
		layers[i] = l
	}
	must(t, os.WriteFile(filepath.Join(dirs[1], "x"), []byte("lower x"), 0644))
	must(t, os.WriteFile(filepath.Join(dirs[1], "y"), []byte("lower y"), 0644))
	ufs := New(WithWritableLayer(layers[0]), WithReadOnlyLayer(layers[1]))
	must(t, ufs.Remove("/x"))
	must(t, ufs.Remove("/y"))
	wh := filepath.Join(dirs[0], ".wh.x")
	if _, err := os.Lstat(wh); err != nil {
		t.Fatalf("whiteout: %v", err)
	}
	short := shortName(t, wh)
	if short == "" {
		t.Skip("8.3 short names are off on this volume")
	}
	shortY := shortName(t, filepath.Join(dirs[0], ".wh.y"))
	t.Logf("short names: %s, %s", short, shortY)

	if _, err := ufs.Lstat("/" + short); err == nil {
		t.Errorf("Lstat(%q) resolves the whiteout", short)
	}
	if err := ufs.Remove("/" + short); err == nil {
		t.Errorf("Remove(%q) succeeded", short)
	}
	if err := ufs.Rename("/"+shortY, "/moved"); err == nil {
		t.Errorf("Rename(%q) succeeded", shortY)
	}
	put(t, ufs, "/z", "z")
	if err := ufs.Rename("/z", "/"+short); err == nil {
		t.Errorf("Rename onto %q succeeded", short)
	}
	if f, err := ufs.OpenFile("/"+short, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0644); err == nil {
		f.Write([]byte("data"))
		f.Close()
		t.Errorf("OpenFile(%q, O_CREATE) succeeded", short)
	}
	for _, n := range []string{"/x", "/y"} {
		if _, err := ufs.Lstat(n); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s came back: %v", n, err)
		}
	}
	for _, n := range []string{".wh.x", ".wh.y"} {
		info, err := os.Lstat(filepath.Join(dirs[0], n))
		if err != nil || info.Size() != 0 {
			t.Errorf("%s: %v (size %v)", n, err, info)
		}
	}
}
