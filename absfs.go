package unionfs

import (
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/absfs/absfs"
	"github.com/spf13/afero"
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
// This enables seamless integration with the absfs ecosystem while
// preserving afero.Fs compatibility.
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

// toVirtualPath converts an OS path to a virtual path (forward slashes)
func toVirtualPath(p string) string {
	return filepath.ToSlash(p)
}

// OpenFile implements absfs.Filer
func (a *absFSAdapter) OpenFile(name string, flag int, perm os.FileMode) (absfs.File, error) {
	file, err := a.ufs.OpenFile(toVirtualPath(name), flag, perm)
	if err != nil {
		return nil, err
	}
	return absfs.ExtendSeekable(&unionFile{File: file}), nil
}

// Mkdir implements absfs.Filer
func (a *absFSAdapter) Mkdir(name string, perm os.FileMode) error {
	return a.ufs.Mkdir(toVirtualPath(name), perm)
}

// Remove implements absfs.Filer
func (a *absFSAdapter) Remove(name string) error {
	return a.ufs.Remove(toVirtualPath(name))
}

// Rename implements absfs.Filer
func (a *absFSAdapter) Rename(oldpath, newpath string) error {
	return a.ufs.Rename(toVirtualPath(oldpath), toVirtualPath(newpath))
}

// Stat implements absfs.Filer
func (a *absFSAdapter) Stat(name string) (os.FileInfo, error) {
	return a.ufs.Stat(toVirtualPath(name))
}

// Chmod implements absfs.Filer
func (a *absFSAdapter) Chmod(name string, mode os.FileMode) error {
	return a.ufs.Chmod(toVirtualPath(name), mode)
}

// Chtimes implements absfs.Filer
func (a *absFSAdapter) Chtimes(name string, atime time.Time, mtime time.Time) error {
	return a.ufs.Chtimes(toVirtualPath(name), atime, mtime)
}

// Chown implements absfs.Filer
func (a *absFSAdapter) Chown(name string, uid, gid int) error {
	return a.ufs.Chown(toVirtualPath(name), uid, gid)
}

// Separator returns the OS-specific path separator for absfs compatibility
// Note: internally unionfs uses forward slashes, but we report OS separator
// so absfs.ExtendFiler does the right path normalization
func (a *absFSAdapter) Separator() uint8 {
	return filepath.Separator
}

// ListSeparator returns the OS-specific path list separator
func (a *absFSAdapter) ListSeparator() uint8 {
	return filepath.ListSeparator
}

// Truncate changes the size of the named file
func (a *absFSAdapter) Truncate(name string, size int64) error {
	ufs := a.ufs
	name = cleanPath(toVirtualPath(name))

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

	// Open file and truncate
	file, err := layer.fs.OpenFile(toAferoPath(name), os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer file.Close()

	// Truncate using the file handle if it supports it
	if tf, ok := file.(interface{ Truncate(int64) error }); ok {
		err = tf.Truncate(size)
	} else {
		// Fallback: not all afero.File implementations support Truncate
		err = &os.PathError{Op: "truncate", Path: name, Err: os.ErrInvalid}
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

// unionFile wraps an afero.File to provide absfs.Seekable interface
type unionFile struct {
	afero.File
}

// Ensure unionFile implements the necessary interfaces for absfs.Seekable
var _ io.Reader = (*unionFile)(nil)
var _ io.Writer = (*unionFile)(nil)
var _ io.Seeker = (*unionFile)(nil)
var _ io.Closer = (*unionFile)(nil)

// Name returns the file name in OS format (as returned by afero)
func (f *unionFile) Name() string {
	return f.File.Name()
}

// Stat returns file info
func (f *unionFile) Stat() (os.FileInfo, error) {
	return f.File.Stat()
}

// Sync commits the file
func (f *unionFile) Sync() error {
	if syncer, ok := f.File.(interface{ Sync() error }); ok {
		return syncer.Sync()
	}
	return nil
}

// ReadAt implements io.ReaderAt
func (f *unionFile) ReadAt(p []byte, off int64) (n int, err error) {
	if ra, ok := f.File.(io.ReaderAt); ok {
		return ra.ReadAt(p, off)
	}
	// Fallback: seek and read
	if seeker, ok := f.File.(io.Seeker); ok {
		if _, err := seeker.Seek(off, io.SeekStart); err != nil {
			return 0, err
		}
		return f.File.Read(p)
	}
	return 0, &os.PathError{Op: "readat", Path: f.File.Name(), Err: os.ErrInvalid}
}

// WriteAt implements io.WriterAt
func (f *unionFile) WriteAt(p []byte, off int64) (n int, err error) {
	if wa, ok := f.File.(io.WriterAt); ok {
		return wa.WriteAt(p, off)
	}
	// Fallback: seek and write
	if seeker, ok := f.File.(io.Seeker); ok {
		if _, err := seeker.Seek(off, io.SeekStart); err != nil {
			return 0, err
		}
		return f.File.Write(p)
	}
	return 0, &os.PathError{Op: "writeat", Path: f.File.Name(), Err: os.ErrInvalid}
}

// WriteString implements io.StringWriter
func (f *unionFile) WriteString(s string) (n int, err error) {
	return f.File.Write([]byte(s))
}

// Truncate truncates the file
func (f *unionFile) Truncate(size int64) error {
	if tf, ok := f.File.(interface{ Truncate(int64) error }); ok {
		return tf.Truncate(size)
	}
	return &os.PathError{Op: "truncate", Path: f.File.Name(), Err: os.ErrInvalid}
}
