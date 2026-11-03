package unionfs

import (
	"errors"
	"fmt"
	"io"
	"os"
	"path"
	"syscall"

	"github.com/absfs/absfs"
)

// chmodBits are the mode bits Chmod applies: permissions plus setuid,
// setgid and sticky.
const chmodBits = os.ModePerm | os.ModeSetuid | os.ModeSetgid | os.ModeSticky

// upper returns the writable layer's filesystem. Callers check that the
// union has a writable layer first.
func (ufs *UnionFS) upper() absfs.FileSystem {
	return ufs.layers[0].fs
}

// chmodUpper sets the permission bits of p in the writable layer to those of
// mode, keeping the file type of typ. Some layers (memfs) store the mode they
// are given verbatim, and would otherwise lose the directory bit.
func (ufs *UnionFS) chmodUpper(p string, typ, mode os.FileMode) error {
	return ufs.upper().Chmod(p, typ.Type()|mode&chmodBits)
}

// ensureUpperDir makes sure directory node n exists in the writable layer,
// copying it up, with its parents, from the layer it comes from. It updates
// n to record the writable layer as its top.
func (ufs *UnionFS) ensureUpperDir(n *node) error {
	if ufs.inUpper(n) {
		return nil
	}
	if ufs.layerHas(0, n.path) {
		n.layers = append([]int{0}, n.layers...)
		return nil
	}
	if err := ufs.ensureUpperDir(n.parent); err != nil {
		return err
	}
	err := ufs.keepDir(n.parent.path, func() error {
		up := ufs.upper()
		if err := up.Mkdir(n.path, 0700); err != nil {
			return err
		}
		if err := ufs.chmodUpper(n.path, n.info.Mode(), n.info.Mode()); err != nil {
			_ = up.Remove(n.path)
			return err
		}
		_ = up.Chtimes(n.path, n.info.ModTime(), n.info.ModTime())
		return nil
	})
	if err != nil {
		return fmt.Errorf("unionfs: copy up directory %s: %w", n.path, err)
	}
	n.layers = append([]int{0}, n.layers...)
	return nil
}

// keepDir runs fn, which copies an entry up into directory dir of the
// writable layer or tidies the union's own markers there, and then puts
// dir's mode and modification time back: such bookkeeping is not a change
// to the directory. A directory without owner write and search permission
// is given them while fn runs, so that copying up below a read-only
// directory works as changing an entry there does on a host.
func (ufs *UnionFS) keepDir(dir string, fn func() error) error {
	up := ufs.upper()
	info, err := ufs.layerLstat(0, dir)
	if err != nil {
		return err
	}
	// Copy the values out now: some layers (memfs) return a FileInfo that
	// tracks later changes.
	mode, mtime := info.Mode(), info.ModTime()
	grant := mode.Perm()&0300 != 0300
	if grant {
		if err := ufs.chmodUpper(dir, mode, mode|0300); err != nil {
			return err
		}
	}
	err = fn()
	if grant {
		if cerr := ufs.chmodUpper(dir, mode, mode); err == nil {
			err = cerr
		}
	}
	_ = up.Chtimes(dir, mtime, mtime)
	return err
}

// copyUp brings node n into the writable layer: directories as an empty
// directory with the same mode and time, everything else as copyUpFile does.
func (ufs *UnionFS) copyUp(n *node) error {
	if ufs.inUpper(n) {
		return nil
	}
	if n.info.IsDir() {
		return ufs.ensureUpperDir(n)
	}
	return ufs.copyUpFile(n, true)
}

// copyUpFile copies the non-directory node n into the writable layer with
// its mode and modification time. A symbolic link is copied as a link to the
// same target. A regular file's contents are copied when withData is set;
// otherwise it is created empty, for callers about to truncate it. On
// failure no partial copy is left behind.
func (ufs *UnionFS) copyUpFile(n *node, withData bool) error {
	if err := ufs.ensureUpperDir(n.parent); err != nil {
		return err
	}
	mode := n.info.Mode()
	var target string
	if isSymlink(n.info) {
		var err error
		if target, err = ufs.layerReadlink(n.top(), n.path); err != nil {
			return err
		}
		if _, ok := ufs.upper().(symlinker); !ok {
			return &os.PathError{Op: "copyup", Path: n.path, Err: errors.ErrUnsupported}
		}
	} else if !mode.IsRegular() {
		return &os.PathError{Op: "copyup", Path: n.path, Err: syscall.ENOTSUP}
	}
	err := ufs.keepDir(n.parent.path, func() error {
		if isSymlink(n.info) {
			return ufs.upper().(symlinker).Symlink(target, n.path)
		}
		return ufs.copyUpRegular(n, withData)
	})
	if err != nil {
		return err
	}
	n.layers = []int{0}
	return nil
}

// copyUpRegular copies the regular file n into the writable layer, its
// contents only when withData is set, and removes the copy if that fails.
func (ufs *UnionFS) copyUpRegular(n *node, withData bool) error {
	up := ufs.upper()
	mode := n.info.Mode()
	dst, err := up.OpenFile(n.path, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0600)
	if err != nil {
		return fmt.Errorf("unionfs: copy up %s: %w", n.path, err)
	}
	if withData {
		err = ufs.copyData(dst, n)
	}
	if cerr := dst.Close(); err == nil {
		err = cerr
	}
	if err == nil {
		err = ufs.chmodUpper(n.path, mode, mode)
	}
	if err != nil {
		_ = up.Remove(n.path)
		return fmt.Errorf("unionfs: copy up %s: %w", n.path, err)
	}
	_ = up.Chtimes(n.path, n.info.ModTime(), n.info.ModTime())
	return nil
}

// copyData copies n's contents from its layer into dst.
func (ufs *UnionFS) copyData(dst io.Writer, n *node) error {
	src, err := ufs.layers[n.top()].fs.Open(n.path)
	if err != nil {
		return err
	}
	defer src.Close()
	size := ufs.copyBufferSize
	if size <= 0 {
		size = defaultCopyBufferSize
	}
	_, err = io.CopyBuffer(dst, onlyReader{src}, make([]byte, size))
	return err
}

// onlyReader hides a source's WriterTo, so that io.CopyBuffer uses the
// configured buffer.
type onlyReader struct{ io.Reader }

// materialize copies the whole merged tree under directory d into the
// writable layer. Whiteouts and opaque markers in the copy stay in place, so
// the merged view does not change and a failure part way leaves only
// completed copy-ups behind; Rename strips them with stripMarkers once the
// tree has moved. Rename uses it to move a directory that lower layers
// contribute to.
func (ufs *UnionFS) materialize(d *node) error {
	if err := ufs.ensureUpperDir(d); err != nil {
		return err
	}
	ents, err := ufs.listDir(d)
	if err != nil {
		return err
	}
	for _, e := range ents {
		c, err := ufs.child(d, e.Name())
		if err != nil {
			return err
		}
		if c.info.IsDir() {
			err = ufs.materialize(c)
		} else {
			err = ufs.copyUp(c)
		}
		if err != nil {
			return err
		}
	}
	return nil
}

// stripMarkers removes every whiteout, opaque marker and internal leftover
// from the writable layer's tree under dir, keeping each directory's mode
// and modification time.
func (ufs *UnionFS) stripMarkers(dir string) error {
	ents, err := ufs.upper().ReadDir(dir)
	if err != nil {
		return err
	}
	return ufs.keepDir(dir, func() error {
		for _, e := range ents {
			p := path.Join(dir, e.Name())
			switch {
			case IsWhiteout(e.Name()):
				if err := ufs.upper().RemoveAll(p); err != nil && !isNotExist(err) {
					return err
				}
			case e.IsDir():
				if err := ufs.stripMarkers(p); err != nil {
					return err
				}
			}
		}
		return nil
	})
}

// clearMarkers removes the whiteouts, the opaque marker and any internal
// leftovers held directly in directory dir of the writable layer.
func (ufs *UnionFS) clearMarkers(dir string) error {
	ents, err := ufs.upper().ReadDir(dir)
	if err != nil {
		return err
	}
	return ufs.keepDir(dir, func() error {
		for _, e := range ents {
			if !IsWhiteout(e.Name()) {
				continue
			}
			if err := ufs.upper().RemoveAll(path.Join(dir, e.Name())); err != nil && !isNotExist(err) {
				return err
			}
		}
		return nil
	})
}

// asideName returns an unused name in directory dir of the writable layer
// for an entry the union is about to delete or is replacing. The name starts
// with internalPrefix, so the entry is hidden from the merged view and hides
// nothing itself, even if a failure leaves it behind.
func (ufs *UnionFS) asideName(dir string) (string, error) {
	for {
		ufs.asideSeq++
		p := path.Join(dir, fmt.Sprintf("%s%d", internalPrefix, ufs.asideSeq))
		found, err := ufs.layerProbe(0, p)
		if err != nil || !found {
			return p, err
		}
	}
}

// discard deletes the writable layer's tree at p by first renaming it
// aside, so that it leaves the merged view in one step: deleting it in
// place could expose lower entries its markers hid if the deletion stopped
// part way. When contents is false the tree is a directory holding only the
// union's markers, which are removed even if it is read-only. A failure
// after the rename leaves a hidden leftover and is reported.
func (ufs *UnionFS) discard(p string, contents bool) error {
	up := ufs.upper()
	tmp, err := ufs.asideName(path.Dir(p))
	if err != nil {
		return err
	}
	if err := up.Rename(p, tmp); err != nil {
		return err
	}
	return ufs.deleteAside(tmp, !contents)
}

// deleteAside deletes the writable layer's tree at aside, which the union
// renamed there to delete it. When markersOnly is set it is a directory
// holding only the union's markers, and is made writable first, so that a
// read-only one goes as rmdir(2) would let an empty one go.
func (ufs *UnionFS) deleteAside(aside string, markersOnly bool) error {
	if markersOnly {
		_ = ufs.chmodUpper(aside, os.ModeDir, 0700)
	}
	return ufs.upper().RemoveAll(aside)
}

// moveUpper renames from to to in the writable layer, replacing an existing
// to. Layers differ on replacing (memfs refuses to, and a host refuses a
// directory that still holds whiteouts), so when the rename fails and to
// exists, the target is renamed aside and the rename tried again; if that
// fails too the target is put back. On success it returns the aside name,
// if any, for the caller to delete. The target is never deleted before the
// move is known to have succeeded.
func (ufs *UnionFS) moveUpper(from, to string) (string, error) {
	up := ufs.upper()
	err := up.Rename(from, to)
	if err == nil || !ufs.layerHas(0, to) {
		return "", err
	}
	aside, aerr := ufs.asideName(path.Dir(to))
	if aerr != nil {
		return "", err
	}
	if up.Rename(to, aside) != nil {
		return "", err
	}
	if err := up.Rename(from, to); err != nil {
		_ = up.Rename(aside, to)
		return "", err
	}
	return aside, nil
}

// writeMarker creates the empty marker file p (a whiteout or an opaque
// marker) in the writable layer.
func (ufs *UnionFS) writeMarker(p string) error {
	f, err := ufs.upper().OpenFile(p, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0666)
	if err != nil {
		return err
	}
	return f.Close()
}

// dropWhiteout removes the whiteout for p from the writable layer, if any.
func (ufs *UnionFS) dropWhiteout(p string) error {
	wh := WhiteoutName(p)
	if found, err := ufs.layerProbe(0, wh); err != nil || !found {
		return err
	}
	if err := ufs.upper().Remove(wh); err != nil && !isNotExist(err) {
		return err
	}
	return nil
}
