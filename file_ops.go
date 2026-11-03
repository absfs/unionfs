package unionfs

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/absfs/absfs"
)

// writeFlags are the OpenFile flags that make an open go to the writable
// layer. modifyFlags are those that change an existing file: O_CREATE alone
// does not, so it copies nothing up.
const (
	modifyFlags = os.O_WRONLY | os.O_RDWR | os.O_APPEND | os.O_TRUNC
	writeFlags  = modifyFlags | os.O_CREATE
)

// abs makes name an absolute, clean virtual path, relative names being taken
// from the union's working directory.
func (ufs *UnionFS) abs(name string) string {
	name = filepath.ToSlash(name)
	if !path.IsAbs(name) {
		ufs.cwdMu.RLock()
		cwd := ufs.cwd
		ufs.cwdMu.RUnlock()
		if cwd == "" {
			cwd = "/"
		}
		name = path.Join(cwd, name)
	}
	return cleanPath(name)
}

// checkRemoveName refuses names that Remove, RemoveAll and Rename must not
// act on, as the host calls do: an empty name (ENOENT), and a name whose
// last element is "." or ".." (EINVAL). Made absolute, these would name the
// working directory or its parent.
func checkRemoveName(name string) error {
	if name == "" {
		return syscall.ENOENT
	}
	trimmed := strings.TrimRight(filepath.ToSlash(name), "/")
	if last := trimmed[strings.LastIndex(trimmed, "/")+1:]; last == "." || last == ".." {
		return syscall.EINVAL
	}
	return nil
}

// pathErr reports err as a *os.PathError for op on name, the path the
// caller gave.
func pathErr(op, name string, err error) error {
	var pe *os.PathError
	if errors.As(err, &pe) {
		err = pe.Err
	}
	return &os.PathError{Op: op, Path: name, Err: err}
}

// linkErr reports err as a *os.LinkError.
func linkErr(op, oldname, newname string, err error) error {
	var pe *os.PathError
	if errors.As(err, &pe) {
		err = pe.Err
	}
	var le *os.LinkError
	if errors.As(err, &le) {
		err = le.Err
	}
	return &os.LinkError{Op: op, Old: oldname, New: newname, Err: err}
}

// invalidate drops every cached entry at or under the given paths.
func (ufs *UnionFS) invalidate(paths ...string) {
	for _, p := range paths {
		ufs.cache.invalidateTree(p)
	}
}

// Stat returns the FileInfo of name in the merged view, following symbolic
// links. Links are resolved against the union, so a link in the writable
// layer may point at a file that only a lower layer has.
func (ufs *UnionFS) Stat(name string) (os.FileInfo, error) {
	p := ufs.abs(name)
	if info, _, ok := ufs.cache.getStat(p); ok {
		return info, nil
	}
	if ufs.cache.isNegative(p) {
		return nil, &os.PathError{Op: "stat", Path: name, Err: os.ErrNotExist}
	}

	ufs.mu.RLock()
	defer ufs.mu.RUnlock()
	n, err := ufs.resolve(p, true)
	if err != nil {
		if os.IsNotExist(err) {
			ufs.cache.putNegative(p)
		}
		return nil, pathErr("stat", name, err)
	}
	info := withName(n.info, path.Base(p))
	ufs.cache.putStat(p, info, n.top())
	return info, nil
}

// Lstat returns the FileInfo of name in the merged view without following a
// symbolic link at name itself.
func (ufs *UnionFS) Lstat(name string) (os.FileInfo, error) {
	p := ufs.abs(name)
	ufs.mu.RLock()
	defer ufs.mu.RUnlock()
	n, err := ufs.resolve(p, false)
	if err != nil {
		return nil, pathErr("lstat", name, err)
	}
	return n.info, nil
}

// Open opens name for reading.
func (ufs *UnionFS) Open(name string) (absfs.File, error) {
	return ufs.OpenFile(name, os.O_RDONLY, 0)
}

// Create creates or truncates name in the writable layer and opens it for
// reading and writing.
func (ufs *UnionFS) Create(name string) (absfs.File, error) {
	return ufs.OpenFile(name, os.O_RDWR|os.O_CREATE|os.O_TRUNC, 0666)
}

// OpenFile opens name with the given flags.
//
// A read-only open is served by the layer the file comes from; a directory
// opens as a merged listing. An open that can change the file (O_WRONLY,
// O_RDWR, O_APPEND or O_TRUNC) is served by the writable layer, a file only a
// lower layer has being copied up first (just its mode and times when O_TRUNC
// is set). With O_CREATE a missing file is created in the writable layer,
// its directory copied up as needed and any whiteout for its name removed;
// O_CREATE alone on an existing file opens it for reading and copies nothing
// up. Opening a directory with O_CREATE or for writing fails with EISDIR.
func (ufs *UnionFS) OpenFile(name string, flag int, perm os.FileMode) (absfs.File, error) {
	p := ufs.abs(name)
	if flag&writeFlags == 0 {
		return ufs.openRead(name, p, flag, perm)
	}
	if ufs.writableLayer == nil {
		return nil, ErrNoWritableLayer
	}

	ufs.mu.Lock()
	defer ufs.mu.Unlock()
	defer ufs.invalidate(p)

	n, err := ufs.resolve(p, true)
	switch {
	case err == nil:
		if flag&os.O_CREATE != 0 && flag&os.O_EXCL != 0 {
			return nil, &os.PathError{Op: "open", Path: name, Err: os.ErrExist}
		}
		if n.info.IsDir() {
			return nil, &os.PathError{Op: "open", Path: name, Err: syscall.EISDIR}
		}
		if flag&modifyFlags == 0 {
			// O_RDONLY|O_CREATE of an existing file is a read.
			return ufs.layers[n.top()].fs.OpenFile(n.path, flag&^os.O_CREATE, perm)
		}
		if !ufs.inUpper(n) {
			if err := ufs.copyUpFile(n, flag&os.O_TRUNC == 0); err != nil {
				return nil, pathErr("open", name, err)
			}
		}
		ufs.invalidate(n.path)
		return ufs.upper().OpenFile(n.path, flag&^(os.O_CREATE|os.O_EXCL), perm)
	case !os.IsNotExist(err) || flag&os.O_CREATE == 0:
		return nil, pathErr("open", name, err)
	}

	dir, base, err := ufs.createTarget("open", p, flag&os.O_EXCL == 0)
	if err != nil {
		return nil, pathErr("open", name, err)
	}
	if err := ufs.ensureUpperDir(dir); err != nil {
		return nil, pathErr("open", name, err)
	}
	full := path.Join(dir.path, base)
	f, err := ufs.upper().OpenFile(full, flag, perm)
	if err != nil {
		return nil, err
	}
	ufs.invalidate(full)
	if err := ufs.dropWhiteout(full); err != nil {
		f.Close()
		_ = ufs.upper().Remove(full)
		return nil, pathErr("open", name, err)
	}
	return f, nil
}

func (ufs *UnionFS) openRead(name, p string, flag int, perm os.FileMode) (absfs.File, error) {
	ufs.mu.RLock()
	defer ufs.mu.RUnlock()
	n, err := ufs.resolve(p, true)
	if err != nil {
		return nil, pathErr("open", name, err)
	}
	if n.info.IsDir() {
		return newUnionDir(ufs, n.path), nil
	}
	return ufs.layers[n.top()].fs.OpenFile(n.path, flag, perm)
}

// createTarget finds where a new entry for absolute path p goes: the
// directory that holds it and its name there. When follow is set, a dangling
// symbolic link at p is followed to the path it names, as creating a file
// through such a link does; otherwise an existing entry, link or not, is
// EEXIST. A name with the whiteout prefix is refused with EPERM.
func (ufs *UnionFS) createTarget(op, p string, follow bool) (*node, string, error) {
	if p == "/" {
		return nil, "", &os.PathError{Op: op, Path: p, Err: os.ErrExist}
	}
	links := 0
	var start *node // where a relative link target is walked from
	for {
		dir, base, err := ufs.parentFrom(op, start, p, &links)
		if err != nil {
			return nil, "", err
		}
		full := path.Join(dir.path, base)
		if IsWhiteout(base) {
			return nil, "", &os.PathError{Op: op, Path: full, Err: syscall.EPERM}
		}
		c, err := ufs.child(dir, base)
		if err != nil {
			if os.IsNotExist(err) {
				return dir, base, nil
			}
			return nil, "", err
		}
		if !follow || !isSymlink(c.info) {
			return nil, "", &os.PathError{Op: op, Path: full, Err: os.ErrExist}
		}
		// resolve has already refused a chain this long for OpenFile, the
		// only caller that follows; this bound guards against layers
		// changed underneath the union.
		links++
		if links > maxSymlinks {
			return nil, "", &os.PathError{Op: op, Path: full, Err: syscall.ELOOP}
		}
		target, err := ufs.layerReadlink(c.top(), c.path)
		if err != nil {
			return nil, "", err
		}
		// The target is walked as stored, from the link's directory, so
		// that a ".." after a linked directory in it steps out of the
		// directory that link resolves to.
		p, start = target, dir
	}
}

// Mkdir creates directory name in the writable layer. When a lower layer has
// the name, hidden by a whiteout, the new directory is made opaque so that
// none of the old lower directory's entries show through it.
func (ufs *UnionFS) Mkdir(name string, perm os.FileMode) error {
	if ufs.writableLayer == nil {
		return ErrNoWritableLayer
	}
	p := ufs.abs(name)
	ufs.mu.Lock()
	defer ufs.mu.Unlock()
	// mkdirAt drops the resolved path; p may reach it through a link.
	defer ufs.invalidate(p)
	dir, base, err := ufs.createTarget("mkdir", p, false)
	if err != nil {
		return pathErr("mkdir", name, err)
	}
	if err := ufs.mkdirAt(dir, base, perm); err != nil {
		return pathErr("mkdir", name, err)
	}
	return nil
}

// mkdirAt creates directory base inside dir.
func (ufs *UnionFS) mkdirAt(dir *node, base string, perm os.FileMode) error {
	p := path.Join(dir.path, base)
	defer ufs.invalidate(p)
	if err := ufs.ensureUpperDir(dir); err != nil {
		return err
	}
	opaque, err := ufs.lowerHas(dir, base)
	if err != nil {
		return err
	}
	if err := ufs.upper().Mkdir(p, perm); err != nil {
		return err
	}
	if opaque {
		// perm may lack owner write permission (0555): the marker is
		// bookkeeping, written under keepDir's grant as the others are.
		err = ufs.keepDir(p, func() error { return ufs.writeMarker(opaquePath(p)) })
	}
	if err == nil {
		err = ufs.dropWhiteout(p)
	}
	if err != nil {
		// Leave the name as it was rather than half made.
		_ = ufs.upper().RemoveAll(p)
		return err
	}
	return nil
}

// MkdirAll creates directory name and any missing parents in the writable
// layer, as Mkdir does for each. It does nothing for a directory that
// already exists in the merged view, and follows symbolic links to
// directories on the way.
func (ufs *UnionFS) MkdirAll(name string, perm os.FileMode) error {
	if ufs.writableLayer == nil {
		return ErrNoWritableLayer
	}
	p := ufs.abs(name)
	ufs.mu.Lock()
	defer ufs.mu.Unlock()

	cur, err := ufs.root()
	if err != nil {
		return pathErr("mkdir", name, err)
	}
	given := "/" // the prefix of p walked so far, as the caller named it
	for _, elem := range splitPath(p) {
		given = path.Join(given, elem)
		next, err := ufs.child(cur, elem)
		switch {
		case os.IsNotExist(err):
			if IsWhiteout(elem) {
				return &os.PathError{Op: "mkdir", Path: name, Err: syscall.EPERM}
			}
			// mkdirAt drops the resolved path; given may reach it through
			// a link.
			ufs.invalidate(given)
			if err := ufs.mkdirAt(cur, elem, perm); err != nil {
				return pathErr("mkdir", name, err)
			}
			info, err := ufs.layerLstat(0, path.Join(cur.path, elem))
			if err != nil {
				return pathErr("mkdir", name, err)
			}
			next = &node{path: path.Join(cur.path, elem), info: info, layers: []int{0}, parent: cur}
		case err != nil:
			return pathErr("mkdir", name, err)
		case isSymlink(next.info):
			if next, err = ufs.resolve(next.path, true); err != nil {
				return pathErr("mkdir", name, err)
			}
		}
		if !next.info.IsDir() {
			return &os.PathError{Op: "mkdir", Path: name, Err: syscall.ENOTDIR}
		}
		cur = next
	}
	return nil
}

// Remove removes the file, symbolic link or empty directory name from the
// merged view. A directory is empty when its merged listing is: its upper
// copy may still hold whiteouts, and they are removed with it. Whatever the
// writable layer has is deleted; a whiteout is written only when a lower
// layer still has the name, so deleting a file that exists only in the
// writable layer leaves no trace. As with os.Remove, an empty name fails with
// ENOENT, and a name ending in "." or ".." with EINVAL.
func (ufs *UnionFS) Remove(name string) error {
	if ufs.writableLayer == nil {
		return ErrNoWritableLayer
	}
	if err := checkRemoveName(name); err != nil {
		return &os.PathError{Op: "remove", Path: name, Err: err}
	}
	p := ufs.abs(name)
	ufs.mu.Lock()
	defer ufs.mu.Unlock()

	n, err := ufs.resolve(p, false)
	if err != nil {
		return pathErr("remove", name, err)
	}
	defer ufs.invalidate(p, n.path)
	if n.parent == nil {
		return &os.PathError{Op: "remove", Path: name, Err: syscall.EBUSY}
	}
	if n.info.IsDir() {
		ents, err := ufs.listDir(n)
		if err != nil {
			return pathErr("remove", name, err)
		}
		if len(ents) > 0 {
			return &os.PathError{Op: "remove", Path: name, Err: syscall.ENOTEMPTY}
		}
	}
	if err := ufs.removeNode(n, false); err != nil {
		return pathErr("remove", name, err)
	}
	return nil
}

// removeNode deletes n from the writable layer, recursively when all is
// set, and writes a whiteout when a lower layer still has its name.
//
// The whiteout is written first: it has no effect while the writable layer
// still has the entry, and once that is gone it hides the lower one at
// once, so no failure part way exposes lower entries that n, or markers
// inside n, were hiding.
func (ufs *UnionFS) removeNode(n *node, all bool) error {
	needWhiteout, err := ufs.lowerHas(n.parent, path.Base(n.path))
	if err != nil {
		return err
	}
	if !needWhiteout {
		return ufs.removeUpper(n, all)
	}
	if err := ufs.ensureUpperDir(n.parent); err != nil {
		return err
	}
	wh := WhiteoutName(n.path)
	if err := ufs.writeMarker(wh); err != nil {
		return err
	}
	if !ufs.inUpper(n) {
		return nil
	}
	if n.info.IsDir() {
		err = ufs.discard(n.path, all)
	} else {
		err = ufs.upper().Remove(n.path)
	}
	if err != nil && ufs.layerHas(0, n.path) {
		_ = ufs.upper().Remove(wh)
	}
	return err
}

// removeUpper deletes n, which no lower layer has, from the writable layer.
// A directory's own markers hide nothing then, so they go first.
func (ufs *UnionFS) removeUpper(n *node, all bool) error {
	if !ufs.inUpper(n) {
		return nil
	}
	up := ufs.upper()
	switch {
	case all:
		return up.RemoveAll(n.path)
	case n.info.IsDir():
		if err := ufs.clearMarkers(n.path); err != nil {
			return err
		}
	}
	return up.Remove(n.path)
}

// RemoveAll removes name and everything under it from the merged view. The
// writable layer's copy is deleted, and a single whiteout for name hides
// whatever lower layers have. It returns nil when name does not exist, and,
// as os.RemoveAll does, for an empty name. A name ending in "." or ".."
// fails with EINVAL. RemoveAll("/") removes every entry of the root.
func (ufs *UnionFS) RemoveAll(name string) error {
	if ufs.writableLayer == nil {
		return ErrNoWritableLayer
	}
	if name == "" {
		return nil
	}
	if err := checkRemoveName(name); err != nil {
		return &os.PathError{Op: "removeall", Path: name, Err: err}
	}
	p := ufs.abs(name)
	ufs.mu.Lock()
	defer ufs.mu.Unlock()

	n, err := ufs.resolve(p, false)
	if err != nil {
		if os.IsNotExist(err) {
			return nil
		}
		return pathErr("removeall", name, err)
	}
	defer ufs.invalidate(p, n.path)
	if n.parent != nil {
		if err := ufs.removeNode(n, true); err != nil {
			return pathErr("removeall", name, err)
		}
		return nil
	}
	ents, err := ufs.listDir(n)
	if err != nil {
		return pathErr("removeall", name, err)
	}
	for _, e := range ents {
		c, err := ufs.child(n, e.Name())
		if err == nil {
			err = ufs.removeNode(c, true)
		}
		if err != nil {
			return pathErr("removeall", name, err)
		}
	}
	return nil
}

// Rename moves oldname to newname within the merged view, replacing a
// newname that is a non-directory, or an empty directory when oldname is a
// directory, as rename(2) does.
//
// The moved entry ends up in the writable layer. A file or link only a lower
// layer has is copied up first. A directory that lower layers contribute to
// is copied up whole (its merged contents, recursively) before the move, so
// the cost is proportional to its size; a directory only the writable layer
// has is moved directly. A moved directory is made opaque when a lower layer
// has newname. A whiteout is left at oldname when a lower layer still has it.
// An empty name fails with ENOENT, and a name ending in "." or ".." with
// EINVAL.
func (ufs *UnionFS) Rename(oldname, newname string) error {
	if ufs.writableLayer == nil {
		return ErrNoWritableLayer
	}
	fail := func(err error) error { return linkErr("rename", oldname, newname, err) }
	for _, name := range []string{oldname, newname} {
		if err := checkRemoveName(name); err != nil {
			return fail(err)
		}
	}
	op, np := ufs.abs(oldname), ufs.abs(newname)
	ufs.mu.Lock()
	defer ufs.mu.Unlock()

	src, err := ufs.resolve(op, false)
	if err != nil {
		return fail(err)
	}
	if src.parent == nil {
		return fail(syscall.EBUSY)
	}
	dstDir, base, err := ufs.resolveParent("rename", np)
	if err != nil {
		return fail(err)
	}
	if IsWhiteout(base) {
		return fail(syscall.EPERM)
	}
	dp := path.Join(dstDir.path, base)
	defer ufs.invalidate(op, np, src.path, dp)
	if dp == src.path {
		return nil
	}
	isDir := src.info.IsDir()
	if isDir && strings.HasPrefix(dp, src.path+"/") {
		return fail(syscall.EINVAL)
	}
	dst, err := ufs.child(dstDir, base)
	switch {
	case os.IsNotExist(err):
		dst = nil
	case err != nil:
		return fail(err)
	}
	if dst != nil {
		switch {
		case isDir && !dst.info.IsDir():
			return fail(syscall.ENOTDIR)
		case !isDir && dst.info.IsDir():
			return fail(syscall.EISDIR)
		case isDir:
			ents, err := ufs.listDir(dst)
			if err != nil {
				return fail(err)
			}
			if len(ents) > 0 {
				return fail(syscall.ENOTEMPTY)
			}
		}
	}

	srcLower, err := ufs.lowerHas(src.parent, path.Base(src.path))
	if err != nil {
		return fail(err)
	}
	dstLower, err := ufs.lowerHas(dstDir, base)
	if err != nil {
		return fail(err)
	}

	// Bring the source wholly into the writable layer; the merged view
	// does not change.
	materialized := isDir && !(ufs.inUpper(src) && len(src.layers) == 1)
	switch {
	case materialized:
		err = ufs.materialize(src)
	case !isDir:
		err = ufs.copyUp(src)
	}
	if err != nil {
		return fail(err)
	}
	if err := ufs.ensureUpperDir(dstDir); err != nil {
		return fail(err)
	}

	// Whiteout the old name before the move, as removeNode does, so that a
	// failure leaves the merged view as it was.
	up := ufs.upper()
	srcWh := WhiteoutName(src.path)
	if srcLower {
		if err := ufs.writeMarker(srcWh); err != nil {
			return fail(err)
		}
	}
	aside, err := ufs.moveUpper(src.path, dp)
	if err != nil {
		if srcLower {
			_ = up.Remove(srcWh)
		}
		return fail(err)
	}

	err = ufs.dropWhiteout(dp)
	if err == nil && materialized {
		err = ufs.stripMarkers(dp)
	}
	if err == nil && isDir {
		err = ufs.keepDir(dp, func() error {
			if dstLower {
				return ufs.writeMarker(opaquePath(dp))
			}
			if ufs.layerHas(0, opaquePath(dp)) {
				return up.Remove(opaquePath(dp))
			}
			return nil
		})
	}
	if aside != "" {
		// The replaced target was an empty directory in the merged view,
		// so its upper copy holds at most the union's markers.
		if rerr := ufs.deleteAside(aside, isDir); err == nil {
			err = rerr
		}
	}
	if err != nil {
		return fail(err)
	}
	return nil
}

// resolveForChange resolves name for an attribute change and copies it up.
func (ufs *UnionFS) resolveForChange(op, name string, follow bool) (*node, error) {
	return ufs.resolveForChangeUnless(op, name, follow, nil)
}

// resolveForChangeUnless is resolveForChange, except that it returns a nil
// node, copying nothing up, when name resolves to a node only lower layers
// have and unchanged reports that the change leaves that node as it is.
func (ufs *UnionFS) resolveForChangeUnless(op, name string, follow bool, unchanged func(os.FileInfo) bool) (*node, error) {
	if ufs.writableLayer == nil {
		return nil, ErrNoWritableLayer
	}
	p := ufs.abs(name)
	n, err := ufs.resolve(p, follow)
	if err != nil {
		return nil, pathErr(op, name, err)
	}
	if unchanged != nil && !ufs.inUpper(n) && unchanged(n.info) {
		return nil, nil
	}
	ufs.invalidate(p, n.path)
	if err := ufs.copyUp(n); err != nil {
		return nil, pathErr(op, name, err)
	}
	return n, nil
}

// Chmod changes the mode of name, following symbolic links, after copying
// it up to the writable layer.
func (ufs *UnionFS) Chmod(name string, mode os.FileMode) error {
	ufs.mu.Lock()
	defer ufs.mu.Unlock()
	n, err := ufs.resolveForChange("chmod", name, true)
	if err != nil {
		return err
	}
	return ufs.chmodUpper(n.path, n.info.Mode(), mode)
}

// Chown changes the owner of name, following symbolic links, after copying
// it up to the writable layer.
func (ufs *UnionFS) Chown(name string, uid, gid int) error {
	ufs.mu.Lock()
	defer ufs.mu.Unlock()
	n, err := ufs.resolveForChange("chown", name, true)
	if err != nil {
		return err
	}
	return ufs.upper().Chown(n.path, uid, gid)
}

// Chtimes changes the access and modification times of name, following
// symbolic links, after copying it up to the writable layer.
//
// A zero mtime leaves the modification time as it is, as os.Chtimes does,
// whatever the writable layer would make of a zero Time.
//
// A change to the access time alone does not copy up. When name resolves to
// a file, link target or directory that only lower layers have, and mtime
// is zero or equals the lower entry's current modification time, Chtimes
// copies nothing up, changes nothing and returns nil: os.FileInfo has no
// access time, so the change could not be seen through the union, and an
// NFS client that records reads as access-time SETATTRs would otherwise
// copy up every file it reads. A FileInfo's Sys value from a host layer
// keeps reporting the lower file's own access time. Any other mtime copies
// up as before, and an entry the writable layer has gets both times. A
// zero atime is passed to the writable layer as given: a host directory
// leaves the access time alone, memfs stores the zero Time.
func (ufs *UnionFS) Chtimes(name string, atime, mtime time.Time) error {
	ufs.mu.Lock()
	defer ufs.mu.Unlock()
	n, err := ufs.resolveForChangeUnless("chtimes", name, true, func(info os.FileInfo) bool {
		return mtime.IsZero() || mtime.Equal(info.ModTime())
	})
	if err != nil || n == nil {
		return err
	}
	if mtime.IsZero() {
		// Only an entry the writable layer already has gets here with a
		// zero mtime, and n.info is that entry as it is now.
		mtime = n.info.ModTime()
	}
	return ufs.upper().Chtimes(n.path, atime, mtime)
}

// Lchown changes the owner of name without following a symbolic link at
// name, after copying it up to the writable layer. A writable layer without
// Lchown gets Chown.
func (ufs *UnionFS) Lchown(name string, uid, gid int) error {
	ufs.mu.Lock()
	defer ufs.mu.Unlock()
	n, err := ufs.resolveForChange("lchown", name, false)
	if err != nil {
		return err
	}
	if l, ok := ufs.upper().(lchowner); ok {
		return l.Lchown(n.path, uid, gid)
	}
	return ufs.upper().Chown(n.path, uid, gid)
}

// Truncate changes the size of file name, following symbolic links. A file
// only a lower layer has is copied up first; truncating it to zero copies
// none of its contents.
func (ufs *UnionFS) Truncate(name string, size int64) error {
	if ufs.writableLayer == nil {
		return ErrNoWritableLayer
	}
	p := ufs.abs(name)
	ufs.mu.Lock()
	defer ufs.mu.Unlock()
	n, err := ufs.resolve(p, true)
	if err != nil {
		return pathErr("truncate", name, err)
	}
	defer ufs.invalidate(p, n.path)
	if n.info.IsDir() {
		return &os.PathError{Op: "truncate", Path: name, Err: syscall.EISDIR}
	}
	if !ufs.inUpper(n) {
		if err := ufs.copyUpFile(n, size > 0); err != nil {
			return pathErr("truncate", name, err)
		}
	}
	return ufs.upper().Truncate(n.path, size)
}

// splitPath splits a path into its elements.
func splitPath(p string) []string {
	p = strings.Trim(cleanPath(p), "/")
	if p == "" {
		return []string{}
	}
	return strings.Split(p, "/")
}

// ReadDir returns the merged entries of directory name, sorted by name.
// Entries in upper layers shadow those below, whited-out names are left out,
// an opaque directory shows none of the lower layers' entries, and the
// whiteout and opaque markers themselves are never listed.
func (ufs *UnionFS) ReadDir(name string) ([]fs.DirEntry, error) {
	p := ufs.abs(name)
	ufs.mu.RLock()
	defer ufs.mu.RUnlock()
	n, err := ufs.resolve(p, true)
	if err != nil {
		return nil, pathErr("readdir", name, err)
	}
	if !n.info.IsDir() {
		return nil, &os.PathError{Op: "readdir", Path: name, Err: syscall.ENOTDIR}
	}
	return ufs.listDir(n)
}

// ReadFile returns the contents of file name from the layer it comes from.
func (ufs *UnionFS) ReadFile(name string) ([]byte, error) {
	p := ufs.abs(name)
	ufs.mu.RLock()
	defer ufs.mu.RUnlock()
	n, err := ufs.resolve(p, true)
	if err != nil {
		return nil, pathErr("read", name, err)
	}
	if n.info.IsDir() {
		return nil, &os.PathError{Op: "read", Path: name, Err: syscall.EISDIR}
	}
	f, err := ufs.layers[n.top()].fs.Open(n.path)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	return io.ReadAll(f)
}

// Sub returns a read-only fs.FS of the merged view rooted at dir.
func (ufs *UnionFS) Sub(dir string) (fs.FS, error) {
	return absfs.FilerToFS(ufs, ufs.abs(dir))
}

// Chdir sets the working directory that relative names are taken from. It
// must be a directory in the merged view.
func (ufs *UnionFS) Chdir(dir string) error {
	p := ufs.abs(dir)
	info, err := ufs.Stat(p)
	if err != nil {
		return pathErr("chdir", dir, err)
	}
	if !info.IsDir() {
		return &os.PathError{Op: "chdir", Path: dir, Err: syscall.ENOTDIR}
	}
	ufs.cwdMu.Lock()
	ufs.cwd = p
	ufs.cwdMu.Unlock()
	return nil
}

// Getwd returns the working directory, "/" unless Chdir changed it.
func (ufs *UnionFS) Getwd() (string, error) {
	return ufs.abs("."), nil
}

// TempDir returns "/tmp", a path inside the union. Nothing creates it: a
// caller that wants to use it should MkdirAll it first.
func (ufs *UnionFS) TempDir() string {
	return "/tmp"
}
