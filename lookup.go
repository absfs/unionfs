package unionfs

import (
	"errors"
	"io/fs"
	"os"
	"path"
	"sort"
	"syscall"
)

// maxSymlinks bounds the number of symbolic links followed while resolving
// one path, as Linux's MAXSYMLINKS does.
const maxSymlinks = 40

// node is a resolved path in the merged view.
//
// path has no symbolic links in its directory elements: every link met on
// the way was resolved against the union, not inside a single layer, so the
// path can be handed to each contributing layer as it is.
//
// layers lists the indexes of the layers that contribute to the node, top
// first. A non-directory has exactly one. A directory has every layer, from
// the top one down, in which the same path is also a directory, stopping
// after a layer that holds a whiteout for it or an opaque marker inside it.
type node struct {
	path   string
	info   os.FileInfo // from layers[0]
	layers []int
	parent *node // nil for the root
}

// top returns the index of the layer the node's info came from.
func (n *node) top() int { return n.layers[0] }

// inUpper reports whether the node exists in the writable layer.
func (ufs *UnionFS) inUpper(n *node) bool {
	return ufs.writableLayer != nil && n.top() == 0
}

// lstater, readlinker and symlinker are the optional layer capabilities
// unionfs uses when a layer has them.
type lstater interface {
	Lstat(name string) (os.FileInfo, error)
}

type readlinker interface {
	Readlink(name string) (string, error)
}

type symlinker interface {
	Symlink(oldname, newname string) error
}

type lchowner interface {
	Lchown(name string, uid, gid int) error
}

// layerLstat stats p in layer i without following a final symbolic link,
// falling back to Stat for layers without Lstat.
func (ufs *UnionFS) layerLstat(i int, p string) (os.FileInfo, error) {
	lfs := ufs.layers[i].fs
	if l, ok := lfs.(lstater); ok {
		return l.Lstat(p)
	}
	return lfs.Stat(p)
}

// layerHas reports whether p exists in layer i, without following links.
func (ufs *UnionFS) layerHas(i int, p string) bool {
	_, err := ufs.layerLstat(i, p)
	return err == nil
}

// layerReadlink reads the symbolic link p in layer i.
func (ufs *UnionFS) layerReadlink(i int, p string) (string, error) {
	if r, ok := ufs.layers[i].fs.(readlinker); ok {
		return r.Readlink(p)
	}
	return "", &os.PathError{Op: "readlink", Path: p, Err: syscall.EINVAL}
}

// isNotExist reports whether a layer error means "no such entry here". A
// layer reports ENOTDIR when an element of the path is not a directory in it,
// which for a union means the same thing.
func isNotExist(err error) bool {
	return os.IsNotExist(err) || errors.Is(err, syscall.ENOTDIR)
}

func isSymlink(info os.FileInfo) bool {
	return info.Mode()&os.ModeSymlink != 0
}

// root resolves "/".
func (ufs *UnionFS) root() (*node, error) {
	if len(ufs.layers) == 0 {
		return nil, &os.PathError{Op: "stat", Path: "/", Err: os.ErrNotExist}
	}
	info, err := ufs.layers[0].fs.Stat("/")
	if err != nil {
		return nil, err
	}
	n := &node{path: "/", info: info}
	for i := range ufs.layers {
		n.layers = append(n.layers, i)
		if i < len(ufs.layers)-1 && ufs.layerHas(i, opaquePath("/")) {
			break
		}
	}
	return n, nil
}

// child resolves name inside directory dir without following a symbolic
// link at name. It returns an error satisfying os.IsNotExist when the merged
// view has no such entry.
func (ufs *UnionFS) child(dir *node, name string) (*node, error) {
	p := path.Join(dir.path, name)
	if IsWhiteout(name) {
		return nil, &os.PathError{Op: "lstat", Path: p, Err: os.ErrNotExist}
	}
	var out *node
	for k, li := range dir.layers {
		// A marker in the bottom contributing layer hides nothing.
		hidesBelow := k < len(dir.layers)-1
		info, err := ufs.layerLstat(li, p)
		if err == nil {
			if out == nil {
				out = &node{path: p, info: info, layers: []int{li}, parent: dir}
				if !info.IsDir() {
					return out, nil
				}
			} else {
				if !info.IsDir() {
					// A lower non-directory under a directory is hidden,
					// and so is everything below it.
					break
				}
				out.layers = append(out.layers, li)
			}
			if hidesBelow && ufs.layerHas(li, opaquePath(p)) {
				break
			}
			continue
		}
		if !isNotExist(err) {
			return nil, err
		}
		if hidesBelow && ufs.layerHas(li, WhiteoutName(p)) {
			break
		}
	}
	if out == nil {
		return nil, &os.PathError{Op: "lstat", Path: p, Err: os.ErrNotExist}
	}
	return out, nil
}

// resolve walks the absolute, clean path p through the merged view,
// following symbolic links in its directory elements, and in its last
// element too when follow is set. Links are resolved against the union.
func (ufs *UnionFS) resolve(p string, follow bool) (*node, error) {
	links := 0
	return ufs.walk(p, follow, &links)
}

func (ufs *UnionFS) walk(p string, follow bool, links *int) (*node, error) {
	cur, err := ufs.root()
	if err != nil {
		return nil, err
	}
	elems := splitPath(p)
	for i, name := range elems {
		if !cur.info.IsDir() {
			return nil, &os.PathError{Op: "lstat", Path: p, Err: syscall.ENOTDIR}
		}
		next, err := ufs.child(cur, name)
		if err != nil {
			return nil, err
		}
		last := i == len(elems)-1
		if isSymlink(next.info) && (follow || !last) {
			*links++
			if *links > maxSymlinks {
				return nil, &os.PathError{Op: "lstat", Path: p, Err: syscall.ELOOP}
			}
			target, err := ufs.layerReadlink(next.top(), next.path)
			if err != nil {
				return nil, err
			}
			if !path.IsAbs(target) {
				target = path.Join(cur.path, target)
			}
			rest := append([]string{target}, elems[i+1:]...)
			return ufs.walk(cleanPath(path.Join(rest...)), follow, links)
		}
		cur = next
	}
	return cur, nil
}

// resolveParent resolves the directory that would hold p, following
// symbolic links, and returns it with p's last element. It fails with
// ENOTDIR when that is not a directory. For "/" it fails with EINVAL.
func (ufs *UnionFS) resolveParent(op, p string) (*node, string, error) {
	if p == "/" {
		return nil, "", &os.PathError{Op: op, Path: p, Err: syscall.EINVAL}
	}
	dir, err := ufs.resolve(path.Dir(p), true)
	if err != nil {
		return nil, "", err
	}
	if !dir.info.IsDir() {
		return nil, "", &os.PathError{Op: op, Path: p, Err: syscall.ENOTDIR}
	}
	return dir, path.Base(p), nil
}

// lowerHas reports whether a layer below the writable one would show name
// inside dir if the writable layer had neither it nor its whiteout: that is,
// whether deleting name needs a whiteout, and creating a directory there
// needs an opaque marker.
func (ufs *UnionFS) lowerHas(dir *node, name string) bool {
	p := path.Join(dir.path, name)
	for k, li := range dir.layers {
		if li == 0 && ufs.writableLayer != nil {
			continue
		}
		if ufs.layerHas(li, p) {
			return true
		}
		if k < len(dir.layers)-1 && ufs.layerHas(li, WhiteoutName(p)) {
			return false
		}
	}
	return false
}

// listDir returns the merged entries of directory dir, sorted by name.
// Entries from upper layers shadow those below; a whiteout in a layer hides
// its name in the layers below it; whiteouts and opaque markers themselves
// are never listed.
func (ufs *UnionFS) listDir(dir *node) ([]fs.DirEntry, error) {
	seen := make(map[string]bool)
	var out []fs.DirEntry
	for k, li := range dir.layers {
		ents, err := ufs.layers[li].fs.ReadDir(dir.path)
		if err != nil {
			if k == 0 {
				return nil, err
			}
			continue
		}
		var hidden []string
		for _, e := range ents {
			name := e.Name()
			if name == "." || name == ".." {
				continue
			}
			if IsWhiteout(name) {
				if t, ok := WhiteoutTarget(name); ok {
					hidden = append(hidden, t)
				}
				continue
			}
			if seen[name] {
				continue
			}
			seen[name] = true
			out = append(out, e)
		}
		for _, h := range hidden {
			seen[h] = true
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name() < out[j].Name() })
	return out, nil
}

// namedInfo reports a FileInfo under another base name: Stat of a symbolic
// link reports the link's name with its target's attributes, as os.Stat does.
type namedInfo struct {
	os.FileInfo
	name string
}

func (i namedInfo) Name() string { return i.name }

// withName returns info reporting name as its base name.
func withName(info os.FileInfo, name string) os.FileInfo {
	if info.Name() == name {
		return info
	}
	return namedInfo{FileInfo: info, name: name}
}
