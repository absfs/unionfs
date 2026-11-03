package unionfs

import (
	"errors"
	"os"
	"path"
	"syscall"
)

// Readlink returns the target of the symbolic link name, as stored. It
// fails with EINVAL when name is not a symbolic link.
func (ufs *UnionFS) Readlink(name string) (string, error) {
	p := ufs.abs(name)
	ufs.mu.RLock()
	defer ufs.mu.RUnlock()
	n, err := ufs.resolve(p, false)
	if err != nil {
		return "", pathErr("readlink", name, err)
	}
	if !isSymlink(n.info) {
		return "", &os.PathError{Op: "readlink", Path: name, Err: syscall.EINVAL}
	}
	target, err := ufs.layerReadlink(n.top(), n.path)
	if err != nil {
		return "", pathErr("readlink", name, err)
	}
	return target, nil
}

// Symlink creates newname in the writable layer as a symbolic link to
// oldname, which is stored as given and need not exist. A whiteout for
// newname is removed. The link is resolved against the merged view, so it
// may point at files that only a lower layer has. It fails when newname
// exists, and with errors.ErrUnsupported when the writable layer has no
// symbolic links.
func (ufs *UnionFS) Symlink(oldname, newname string) error {
	if ufs.writableLayer == nil {
		return ErrNoWritableLayer
	}
	p := ufs.abs(newname)
	fail := func(err error) error { return linkErr("symlink", oldname, newname, err) }
	ufs.mu.Lock()
	defer ufs.mu.Unlock()
	defer ufs.invalidate(p)

	s, ok := ufs.upper().(symlinker)
	if !ok {
		return fail(errors.ErrUnsupported)
	}
	dir, base, err := ufs.createTarget("symlink", p, false)
	if err != nil {
		return fail(err)
	}
	if err := ufs.ensureUpperDir(dir); err != nil {
		return fail(err)
	}
	full := path.Join(dir.path, base)
	if err := s.Symlink(oldname, full); err != nil {
		return fail(err)
	}
	if err := ufs.dropWhiteout(full); err != nil {
		return fail(err)
	}
	return nil
}

// LstatIfPossible is Lstat, reporting that Lstat is supported.
func (ufs *UnionFS) LstatIfPossible(name string) (os.FileInfo, bool, error) {
	info, err := ufs.Lstat(name)
	return info, true, err
}

// ReadlinkIfPossible is Readlink.
func (ufs *UnionFS) ReadlinkIfPossible(name string) (string, error) {
	return ufs.Readlink(name)
}

// SymlinkIfPossible is Symlink.
func (ufs *UnionFS) SymlinkIfPossible(oldname, newname string) error {
	return ufs.Symlink(oldname, newname)
}
