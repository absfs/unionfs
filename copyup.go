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
	if n.parent != nil {
		if err := ufs.ensureUpperDir(n.parent); err != nil {
			return err
		}
	}
	up := ufs.upper()
	if err := up.Mkdir(n.path, 0700); err != nil {
		return fmt.Errorf("unionfs: copy up directory %s: %w", n.path, err)
	}
	if err := ufs.chmodUpper(n.path, n.info.Mode(), n.info.Mode()); err != nil {
		return fmt.Errorf("unionfs: copy up directory %s: %w", n.path, err)
	}
	_ = up.Chtimes(n.path, n.info.ModTime(), n.info.ModTime())
	n.layers = append([]int{0}, n.layers...)
	return nil
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
	up := ufs.upper()
	mode := n.info.Mode()

	if isSymlink(n.info) {
		target, err := ufs.layerReadlink(n.top(), n.path)
		if err != nil {
			return err
		}
		s, ok := up.(symlinker)
		if !ok {
			return &os.PathError{Op: "copyup", Path: n.path, Err: errors.ErrUnsupported}
		}
		if err := s.Symlink(target, n.path); err != nil {
			return err
		}
		n.layers = []int{0}
		return nil
	}
	if !mode.IsRegular() {
		return &os.PathError{Op: "copyup", Path: n.path, Err: syscall.ENOTSUP}
	}

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
	n.layers = []int{0}
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
// writable layer and then removes the whiteouts and opaque markers inside
// it, so the upper copy alone holds everything the merged view showed.
// Rename uses it to move a directory that lower layers contribute to.
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
	return ufs.clearMarkers(d.path)
}

// clearMarkers removes the whiteouts and the opaque marker held directly in
// directory dir of the writable layer.
func (ufs *UnionFS) clearMarkers(dir string) error {
	ents, err := ufs.upper().ReadDir(dir)
	if err != nil {
		return err
	}
	for _, e := range ents {
		if !IsWhiteout(e.Name()) {
			continue
		}
		if err := ufs.upper().Remove(path.Join(dir, e.Name())); err != nil && !isNotExist(err) {
			return err
		}
	}
	return nil
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
