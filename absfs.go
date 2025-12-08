package unionfs

import (
	"os"
	"time"

	"github.com/absfs/absfs"
)

// absFSAdapter wraps UnionFS to implement absfs.Filer with correct types
type absFSAdapter struct {
	ufs *UnionFS
}

// Ensure absFSAdapter implements absfs.Filer interface at compile time
var _ absfs.Filer = (*absFSAdapter)(nil)

// FileSystem returns an absfs.FileSystem view of this UnionFS.
// The returned FileSystem maintains its own working directory state
// and provides the full absfs.FileSystem interface including convenience
// methods like Open, Create, MkdirAll, RemoveAll, and Truncate.
//
// This enables seamless integration with the absfs ecosystem.
//
// Example:
//
//	ufs := unionfs.New(
//	    unionfs.WithWritableLayer(overlay),
//	    unionfs.WithReadOnlyLayer(base),
//	)
//
//	// Use as absfs.FileSystem
//	fs := ufs.FileSystem()
//	fs.Chdir("/app")
//	file, err := fs.Open("config.yml") // Uses current working directory
func (ufs *UnionFS) FileSystem() absfs.FileSystem {
	adapter := &absFSAdapter{ufs: ufs}
	return absfs.ExtendFiler(adapter)
}

// OpenFile implements absfs.Filer
func (a *absFSAdapter) OpenFile(name string, flag int, perm os.FileMode) (absfs.File, error) {
	name = cleanPath(name)
	return a.ufs.OpenFile(name, flag, perm)
}

// Mkdir implements absfs.Filer
func (a *absFSAdapter) Mkdir(name string, perm os.FileMode) error {
	return a.ufs.Mkdir(cleanPath(name), perm)
}

// Remove implements absfs.Filer
func (a *absFSAdapter) Remove(name string) error {
	return a.ufs.Remove(cleanPath(name))
}

// Rename implements absfs.Filer
func (a *absFSAdapter) Rename(oldpath, newpath string) error {
	return a.ufs.Rename(cleanPath(oldpath), cleanPath(newpath))
}

// Stat implements absfs.Filer
func (a *absFSAdapter) Stat(name string) (os.FileInfo, error) {
	return a.ufs.Stat(cleanPath(name))
}

// Chmod implements absfs.Filer
func (a *absFSAdapter) Chmod(name string, mode os.FileMode) error {
	return a.ufs.Chmod(cleanPath(name), mode)
}

// Chtimes implements absfs.Filer
func (a *absFSAdapter) Chtimes(name string, atime time.Time, mtime time.Time) error {
	return a.ufs.Chtimes(cleanPath(name), atime, mtime)
}

// Chown implements absfs.Filer
func (a *absFSAdapter) Chown(name string, uid, gid int) error {
	return a.ufs.Chown(cleanPath(name), uid, gid)
}

// Separator returns the path separator (always forward slash for virtual paths)
func (a *absFSAdapter) Separator() uint8 {
	return '/'
}

// ListSeparator returns the path list separator (always colon for virtual paths)
func (a *absFSAdapter) ListSeparator() uint8 {
	return ':'
}

// Truncate changes the size of the named file
func (a *absFSAdapter) Truncate(name string, size int64) error {
	ufs := a.ufs
	name = cleanPath(name)

	// Get writable layer
	layer, err := ufs.getWritableLayer()
	if err != nil {
		return err
	}

	// Check if file exists and copy up if needed
	info, layerIdx, err := ufs.findFile(name)
	if err != nil {
		return err
	}

	// Don't truncate directories
	if info.IsDir() {
		return &os.PathError{Op: "truncate", Path: name, Err: os.ErrInvalid}
	}

	// Copy up if file is in a lower layer
	if layerIdx > 0 {
		if err := ufs.copyUp(name, info); err != nil {
			return err
		}
	}

	// Use the filesystem's Truncate if available, otherwise open and truncate file
	if truncater, ok := layer.fs.(interface{ Truncate(string, int64) error }); ok {
		err = truncater.Truncate(name, size)
	} else {
		// Fallback: open file and truncate
		file, err := layer.fs.OpenFile(name, os.O_WRONLY, 0)
		if err != nil {
			return err
		}
		defer file.Close()

		if tf, ok := file.(interface{ Truncate(int64) error }); ok {
			err = tf.Truncate(size)
		} else {
			err = &os.PathError{Op: "truncate", Path: name, Err: os.ErrInvalid}
		}
	}

	if err == nil {
		ufs.InvalidateCache(name)
	}

	return err
}

// AsAbsFS returns an absfs.FileSystem adapter for this UnionFS.
// This is an alias for FileSystem() provided for clarity.
//
// Deprecated: Use FileSystem() instead.
func (ufs *UnionFS) AsAbsFS() absfs.FileSystem {
	return ufs.FileSystem()
}
