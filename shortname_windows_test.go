//go:build windows

package unionfs

import (
	"errors"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"syscall"
	"testing"
	"time"

	"github.com/absfs/absfs"
	"github.com/absfs/osfs"
)

// hostDir is a layer rooted at a host directory, for Windows, where
// basefs v1.0.0 cannot root osfs at a directory (it wants a native path,
// osfs an absfs one).
type hostDir struct {
	absfs.SymlinkFileSystem
	root string // the directory, as an absfs path ("/c/...")
}

func newHostDir(t *testing.T, dir string) *hostDir {
	t.Helper()
	host, err := osfs.NewFS()
	must(t, err)
	return &hostDir{SymlinkFileSystem: host, root: osfs.FromNative(dir)}
}

func (h *hostDir) p(name string) string { return path.Join(h.root, name) }

func (h *hostDir) Stat(p string) (os.FileInfo, error)  { return h.SymlinkFileSystem.Stat(h.p(p)) }
func (h *hostDir) Lstat(p string) (os.FileInfo, error) { return h.SymlinkFileSystem.Lstat(h.p(p)) }
func (h *hostDir) Open(p string) (absfs.File, error)   { return h.SymlinkFileSystem.Open(h.p(p)) }
func (h *hostDir) Create(p string) (absfs.File, error) { return h.SymlinkFileSystem.Create(h.p(p)) }
func (h *hostDir) OpenFile(p string, flag int, perm os.FileMode) (absfs.File, error) {
	return h.SymlinkFileSystem.OpenFile(h.p(p), flag, perm)
}
func (h *hostDir) Mkdir(p string, perm os.FileMode) error {
	return h.SymlinkFileSystem.Mkdir(h.p(p), perm)
}
func (h *hostDir) MkdirAll(p string, perm os.FileMode) error {
	return h.SymlinkFileSystem.MkdirAll(h.p(p), perm)
}
func (h *hostDir) Remove(p string) error    { return h.SymlinkFileSystem.Remove(h.p(p)) }
func (h *hostDir) RemoveAll(p string) error { return h.SymlinkFileSystem.RemoveAll(h.p(p)) }
func (h *hostDir) Rename(o, n string) error { return h.SymlinkFileSystem.Rename(h.p(o), h.p(n)) }
func (h *hostDir) Chmod(p string, m os.FileMode) error {
	return h.SymlinkFileSystem.Chmod(h.p(p), m)
}
func (h *hostDir) Chown(p string, uid, gid int) error {
	return h.SymlinkFileSystem.Chown(h.p(p), uid, gid)
}
func (h *hostDir) Lchown(p string, uid, gid int) error {
	return h.SymlinkFileSystem.Lchown(h.p(p), uid, gid)
}
func (h *hostDir) Chtimes(p string, a, m time.Time) error {
	return h.SymlinkFileSystem.Chtimes(h.p(p), a, m)
}
func (h *hostDir) Truncate(p string, size int64) error {
	return h.SymlinkFileSystem.Truncate(h.p(p), size)
}
func (h *hostDir) ReadDir(p string) ([]fs.DirEntry, error) {
	return h.SymlinkFileSystem.ReadDir(h.p(p))
}
func (h *hostDir) ReadFile(p string) ([]byte, error) { return h.SymlinkFileSystem.ReadFile(h.p(p)) }
func (h *hostDir) Readlink(p string) (string, error) { return h.SymlinkFileSystem.Readlink(h.p(p)) }
func (h *hostDir) Symlink(o, n string) error         { return h.SymlinkFileSystem.Symlink(o, h.p(n)) }

// shortName returns the 8.3 short name the host gave the entry at p, or ""
// when it gave none (8.3 names off on the volume).
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

// TestShortNameAliasOfMarker checks that the union's markers in a
// host-directory writable layer cannot be reached through the 8.3 short
// names NTFS gives them where 8.3 names are on (".wh.x" is also "WH~1.X").
func TestShortNameAliasOfMarker(t *testing.T) {
	root := t.TempDir()
	var dirs [2]string
	for i, d := range []string{"upper", "lower"} {
		dirs[i] = filepath.Join(root, d)
		must(t, os.Mkdir(dirs[i], 0755))
	}
	for _, n := range []string{"x", "y", "z"} {
		must(t, os.WriteFile(filepath.Join(dirs[1], n), []byte("lower "+n), 0644))
	}
	must(t, os.MkdirAll(filepath.Join(dirs[1], "dir", "inner"), 0755))
	upper := newHostDir(t, dirs[0])
	ufs := New(WithWritableLayer(upper), WithReadOnlyLayer(newHostDir(t, dirs[1])))
	for _, n := range []string{"/x", "/y", "/z", "/dir"} {
		must(t, ufs.RemoveAll(n))
	}
	must(t, ufs.Mkdir("/dir", 0755)) // opaque
	short := map[string]string{}
	for _, n := range []string{".wh.x", ".wh.y", ".wh.z", `dir\.wh.__dir_opaque`} {
		s := shortName(t, filepath.Join(dirs[0], n))
		if s == "" {
			t.Skip("8.3 short names are off on this volume")
		}
		short[filepath.Base(n)] = s
	}
	t.Logf("short names: %v", short)

	gone := func() {
		t.Helper()
		for _, n := range []string{"/x", "/y", "/z", "/dir/inner"} {
			if _, err := ufs.Lstat(n); !errors.Is(err, os.ErrNotExist) {
				t.Errorf("%s came back: %v", n, err)
			}
		}
		for _, n := range []string{".wh.x", ".wh.y", ".wh.z", `dir\.wh.__dir_opaque`} {
			info, err := os.Lstat(filepath.Join(dirs[0], n))
			if err != nil || info.Size() != 0 {
				t.Errorf("%s: %v (%v)", n, err, info)
			}
		}
	}
	refused := func(what string, err error) {
		t.Helper()
		if !errors.Is(err, syscall.EPERM) {
			t.Errorf("%s: err = %v, want EPERM", what, err)
		}
		gone()
	}

	_, err := ufs.Lstat("/" + short[".wh.x"])
	refused("Lstat", err)
	refused("Remove", ufs.Remove("/"+short[".wh.x"]))
	refused("Remove opaque", ufs.Remove("/dir/"+short[".wh.__dir_opaque"]))
	refused("Rename", ufs.Rename("/"+short[".wh.y"], "/moved"))
	put(t, ufs, "/new", "new")
	refused("Rename onto", ufs.Rename("/new", "/"+short[".wh.z"]))
	f, err := ufs.OpenFile("/"+short[".wh.z"], os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0644)
	if err == nil {
		f.Write([]byte("data"))
		f.Close()
	}
	refused("OpenFile O_CREATE", err)
	refused("Mkdir", ufs.Mkdir("/"+short[".wh.z"], 0755))
	refused("Chmod", ufs.Chmod("/"+short[".wh.x"], 0600))

	// A name of that shape that the layer holds as its own name is an
	// ordinary entry.
	put(t, ufs, "/ab~1.txt", "literal")
	if got := get(t, ufs, "/AB~1.TXT"); got != "literal" {
		t.Errorf("/AB~1.TXT = %q", got)
	}
	must(t, ufs.Remove("/ab~1.txt"))
	gone()
}
