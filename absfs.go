package unionfs

import (
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/absfs/absfs"
)

// absFSAdapter is a view of a UnionFS with its own working directory. It
// resolves relative names against that directory and passes absolute names
// to the union.
type absFSAdapter struct {
	ufs *UnionFS
	mu  sync.RWMutex
	cwd string
}

var _ absfs.SymlinkFileSystem = (*absFSAdapter)(nil)

// FileSystem returns an absfs.FileSystem view of the union with its own
// working directory, starting at "/". Views share the union's layers,
// lock and cache; only the working directory is per view.
//
//	ufs := unionfs.New(
//	    unionfs.WithWritableLayer(overlay),
//	    unionfs.WithReadOnlyLayer(base),
//	)
//	fs := ufs.FileSystem()
//	fs.Chdir("/app")
//	f, err := fs.Open("config.yml") // opens /app/config.yml
func (ufs *UnionFS) FileSystem() absfs.FileSystem {
	return &absFSAdapter{ufs: ufs, cwd: "/"}
}

// SymlinkFileSystem returns an absfs.SymlinkFileSystem view of the union
// with its own working directory, as FileSystem does. *UnionFS is itself an
// absfs.SymlinkFileSystem; use a view when several users of one union need
// separate working directories.
func (ufs *UnionFS) SymlinkFileSystem() absfs.SymlinkFileSystem {
	return &absFSAdapter{ufs: ufs, cwd: "/"}
}

// AsAbsFS returns FileSystem().
//
// Deprecated: Use FileSystem instead.
func (ufs *UnionFS) AsAbsFS() absfs.FileSystem {
	return ufs.FileSystem()
}

// abs resolves name against the view's working directory.
func (a *absFSAdapter) abs(name string) string {
	name = filepath.ToSlash(name)
	if path.IsAbs(name) {
		return cleanPath(name)
	}
	a.mu.RLock()
	cwd := a.cwd
	a.mu.RUnlock()
	if cwd == "" {
		cwd = "/"
	}
	return cleanPath(path.Join(cwd, name))
}

// OpenFile implements absfs.Filer.
func (a *absFSAdapter) OpenFile(name string, flag int, perm os.FileMode) (absfs.File, error) {
	return a.ufs.OpenFile(a.abs(name), flag, perm)
}

// Mkdir implements absfs.Filer.
func (a *absFSAdapter) Mkdir(name string, perm os.FileMode) error {
	return a.ufs.Mkdir(a.abs(name), perm)
}

// Remove implements absfs.Filer.
func (a *absFSAdapter) Remove(name string) error {
	return a.ufs.Remove(a.abs(name))
}

// Rename implements absfs.Filer.
func (a *absFSAdapter) Rename(oldpath, newpath string) error {
	return a.ufs.Rename(a.abs(oldpath), a.abs(newpath))
}

// Stat implements absfs.Filer.
func (a *absFSAdapter) Stat(name string) (os.FileInfo, error) {
	return a.ufs.Stat(a.abs(name))
}

// Chmod implements absfs.Filer.
func (a *absFSAdapter) Chmod(name string, mode os.FileMode) error {
	return a.ufs.Chmod(a.abs(name), mode)
}

// Chtimes implements absfs.Filer.
func (a *absFSAdapter) Chtimes(name string, atime time.Time, mtime time.Time) error {
	return a.ufs.Chtimes(a.abs(name), atime, mtime)
}

// Chown implements absfs.Filer.
func (a *absFSAdapter) Chown(name string, uid, gid int) error {
	return a.ufs.Chown(a.abs(name), uid, gid)
}

// ReadDir implements absfs.Filer.
func (a *absFSAdapter) ReadDir(name string) ([]fs.DirEntry, error) {
	return a.ufs.ReadDir(a.abs(name))
}

// ReadFile implements absfs.Filer.
func (a *absFSAdapter) ReadFile(name string) ([]byte, error) {
	return a.ufs.ReadFile(a.abs(name))
}

// Sub implements absfs.Filer.
func (a *absFSAdapter) Sub(dir string) (fs.FS, error) {
	return absfs.FilerToFS(a.ufs, a.abs(dir))
}

// Chdir implements absfs.FileSystem.
func (a *absFSAdapter) Chdir(dir string) error {
	p := a.abs(dir)
	info, err := a.ufs.Stat(p)
	if err != nil {
		return pathErr("chdir", dir, err)
	}
	if !info.IsDir() {
		return &os.PathError{Op: "chdir", Path: dir, Err: syscall.ENOTDIR}
	}
	a.mu.Lock()
	a.cwd = p
	a.mu.Unlock()
	return nil
}

// Getwd implements absfs.FileSystem.
func (a *absFSAdapter) Getwd() (string, error) {
	return a.abs("."), nil
}

// TempDir implements absfs.FileSystem.
func (a *absFSAdapter) TempDir() string {
	return a.ufs.TempDir()
}

// Open implements absfs.FileSystem.
func (a *absFSAdapter) Open(name string) (absfs.File, error) {
	return a.ufs.Open(a.abs(name))
}

// Create implements absfs.FileSystem.
func (a *absFSAdapter) Create(name string) (absfs.File, error) {
	return a.ufs.Create(a.abs(name))
}

// MkdirAll implements absfs.FileSystem.
func (a *absFSAdapter) MkdirAll(name string, perm os.FileMode) error {
	return a.ufs.MkdirAll(a.abs(name), perm)
}

// RemoveAll implements absfs.FileSystem.
func (a *absFSAdapter) RemoveAll(name string) error {
	return a.ufs.RemoveAll(a.abs(name))
}

// Truncate implements absfs.FileSystem.
func (a *absFSAdapter) Truncate(name string, size int64) error {
	return a.ufs.Truncate(a.abs(name), size)
}

// Lstat implements absfs.SymLinker.
func (a *absFSAdapter) Lstat(name string) (os.FileInfo, error) {
	return a.ufs.Lstat(a.abs(name))
}

// Lchown implements absfs.SymLinker.
func (a *absFSAdapter) Lchown(name string, uid, gid int) error {
	return a.ufs.Lchown(a.abs(name), uid, gid)
}

// Readlink implements absfs.SymLinker.
func (a *absFSAdapter) Readlink(name string) (string, error) {
	return a.ufs.Readlink(a.abs(name))
}

// Symlink implements absfs.SymLinker. The target, oldname, is stored as
// given.
func (a *absFSAdapter) Symlink(oldname, newname string) error {
	return a.ufs.Symlink(oldname, a.abs(newname))
}
