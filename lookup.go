package unionfs

import (
	"errors"
	"io/fs"
	"os"
	"path"
	"sort"
	"strings"
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
//
// A regular file that only lower layers have may have a metadata marker in
// a layer above (see MetaTarget): meta is then the marker's info, metaLayer
// its layer, and info the file's own info with the marker's metadata.
type node struct {
	path      string
	info      os.FileInfo // from layers[0]
	layers    []int
	parent    *node       // nil for the root
	meta      os.FileInfo // the metadata marker's info, or nil
	metaLayer int         // the layer that holds meta
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

// layerProbe reports whether p exists in layer i, without following links.
// Unlike layerHas it reports failures other than absence: a whiteout or
// opaque marker that cannot be checked must not read as missing.
func (ufs *UnionFS) layerProbe(i int, p string) (bool, error) {
	_, err := ufs.layerLstat(i, p)
	switch {
	case err == nil:
		return true, nil
	case isAbsent(err):
		return false, nil
	}
	return false, err
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

// isAbsent reports whether a layer error from probing for a marker means
// the layer has no such marker. A marker's name is longer than the name it
// is for, so a host may refuse it as too long (ENAMETOOLONG) where the
// name itself is fine; a layer cannot hold a name it refuses, so such a
// marker does not exist there.
func isAbsent(err error) bool {
	return isNotExist(err) || errors.Is(err, syscall.ENAMETOOLONG)
}

func isSymlink(info os.FileInfo) bool {
	return info.Mode()&os.ModeSymlink != 0
}

// cachedRoot resolves "/" as root does, from the lookup cache when it is
// on.
func (ufs *UnionFS) cachedRoot() (*node, error) {
	if ufs.lc == nil {
		return ufs.root()
	}
	if r := ufs.lc.getRoot(); r != nil {
		return r, nil
	}
	r, err := ufs.root()
	if err == nil {
		ufs.lc.putRoot(r)
	}
	return r, err
}

// lookup resolves name inside directory dir as child does, from the lookup
// cache when it is on. It reports whether the result came from the cache.
func (ufs *UnionFS) lookup(dir *node, name string) (*node, bool, error) {
	if ufs.lc == nil {
		n, err := ufs.child(dir, name)
		return n, false, err
	}
	if n, ok := ufs.lc.get(dir, name); ok {
		if n == nil {
			return nil, true, &os.PathError{Op: "lstat", Path: path.Join(dir.path, name), Err: os.ErrNotExist}
		}
		return n, true, nil
	}
	n, err := ufs.child(dir, name)
	switch {
	case err == nil:
		ufs.lc.put(dir, name, n)
	case os.IsNotExist(err):
		ufs.lc.put(dir, name, nil)
	}
	return n, false, err
}

// finish returns the node a walk ended at, with its info. Without the
// lookup cache that is cur itself, given the root's info if it is the root.
// With it, cur may be shared, so finish returns a copy, and unless cur was
// just resolved from the layers it reads cur's info afresh: the cache keeps
// which layer holds a name, not its attributes. A cached node whose layer
// has lost it, or holds something of another type now, is dropped from the
// cache and resolved again.
func (ufs *UnionFS) finish(cur *node, fresh bool) (*node, error) {
	if ufs.lc == nil {
		if cur.info == nil {
			info, err := ufs.layers[0].fs.Stat("/")
			if err != nil {
				return nil, err
			}
			cur.info = info
		}
		return cur, nil
	}
	out := *cur
	if fresh && cur.info != nil {
		return &out, nil
	}
	if cur.parent == nil {
		info, err := ufs.layers[0].fs.Stat("/")
		if err != nil {
			return nil, err
		}
		out.info = info
		return &out, nil
	}
	info, err := ufs.layerLstat(cur.top(), cur.path)
	if err == nil && info.Mode().Type() == cur.info.Mode().Type() {
		out.info = info
		if cur.meta == nil {
			return &out, nil
		}
		var meta os.FileInfo
		if meta, err = ufs.layerLstat(cur.metaLayer, MetaMarkerName(cur.path)); err == nil {
			out.attachMeta(meta, cur.metaLayer)
			return &out, nil
		}
	}
	if err != nil && !isAbsent(err) {
		return nil, err
	}
	ufs.lc.invalidate(cur.path)
	return ufs.child(cur.parent, path.Base(cur.path))
}

// root resolves "/" without its info: walking through the root needs only
// its layers, and walk reads the info when a path ends there.
func (ufs *UnionFS) root() (*node, error) {
	if len(ufs.layers) == 0 {
		return nil, &os.PathError{Op: "stat", Path: "/", Err: os.ErrNotExist}
	}
	n := &node{path: "/"}
	for i := range ufs.layers {
		n.layers = append(n.layers, i)
		if i == len(ufs.layers)-1 || !ufs.honours(i) {
			continue
		}
		opaque, err := ufs.layerProbe(i, opaquePath("/"))
		if err != nil {
			return nil, err
		}
		if opaque {
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
	reserved := IsWhiteout(name)
	if reserved && !ufs.upperMarkersOnly {
		return nil, &os.PathError{Op: "lstat", Path: p, Err: os.ErrNotExist}
	}
	var out *node
	for k, li := range dir.layers {
		if reserved && ufs.honours(li) {
			// A marker, where markers count, is never an entry.
			continue
		}
		// A marker in the bottom contributing layer hides nothing, and
		// neither does one in a layer whose markers do not count.
		hidesBelow := k < len(dir.layers)-1 && ufs.honours(li)
		info, err := ufs.layerLstat(li, p)
		if err == nil {
			if out == nil {
				out = &node{path: p, info: info, layers: []int{li}, parent: dir}
				if !info.IsDir() {
					if info.Mode().IsRegular() && !reserved {
						if err := ufs.findMeta(dir, k, out); err != nil {
							return nil, err
						}
					}
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
			if !hidesBelow {
				continue
			}
			opaque, err := ufs.layerProbe(li, opaquePath(p))
			if err != nil {
				return nil, err
			}
			if opaque {
				break
			}
			continue
		}
		if !isNotExist(err) {
			return nil, err
		}
		if !hidesBelow {
			continue
		}
		whited, err := ufs.layerProbe(li, WhiteoutName(p))
		if err != nil {
			return nil, err
		}
		if whited {
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
	return ufs.walk(nil, p, follow, &links)
}

// walk resolves the slash-separated path p element by element, from the
// root when p is absolute or start is nil, and from directory start
// otherwise. p need not be clean: a symbolic link's target is walked as
// stored, so a ".." steps to the parent of the directory reached so far,
// which is where a link before it resolved to, as POSIX requires, not to
// wherever cleaning the text would lead. links counts the links followed.
func (ufs *UnionFS) walk(start *node, p string, follow bool, links *int) (*node, error) {
	cur := start
	if cur == nil {
		r, err := ufs.cachedRoot()
		if err != nil {
			return nil, err
		}
		cur = r
	}
	if path.IsAbs(p) {
		cur = rootOf(cur)
	}
	fresh := false // cur was just resolved from the layers
	elems := strings.Split(p, "/")
	for len(elems) > 0 {
		name := elems[0]
		elems = elems[1:]
		// Only the root is reached without its info, and it is a
		// directory.
		if cur.info != nil && !cur.info.IsDir() {
			return nil, &os.PathError{Op: "lstat", Path: p, Err: syscall.ENOTDIR}
		}
		switch name {
		case "", ".":
			continue
		case "..":
			if cur.parent != nil {
				cur, fresh = cur.parent, false
			}
			continue
		}
		next, cached, err := ufs.lookup(cur, name)
		if err != nil {
			return nil, err
		}
		if !isSymlink(next.info) || !(follow || len(elems) > 0) {
			cur, fresh = next, !cached
			continue
		}
		*links++
		if *links > maxSymlinks {
			return nil, &os.PathError{Op: "lstat", Path: p, Err: syscall.ELOOP}
		}
		target, err := ufs.layerReadlink(next.top(), next.path)
		if err != nil {
			return nil, err
		}
		if target == "" {
			return nil, &os.PathError{Op: "lstat", Path: next.path, Err: os.ErrNotExist}
		}
		if path.IsAbs(target) {
			cur, fresh = rootOf(cur), false
		}
		elems = append(strings.Split(target, "/"), elems...)
	}
	return ufs.finish(cur, fresh)
}

// rootOf returns the root node n was reached from.
func rootOf(n *node) *node {
	for n.parent != nil {
		n = n.parent
	}
	return n
}

// resolveParent resolves the directory that would hold p, following
// symbolic links, and returns it with p's last element. It fails with
// ENOTDIR when that is not a directory. For "/" it fails with EINVAL.
func (ufs *UnionFS) resolveParent(op, p string) (*node, string, error) {
	if p == "/" {
		return nil, "", &os.PathError{Op: op, Path: p, Err: syscall.EINVAL}
	}
	links := 0
	return ufs.parentFrom(op, nil, p, &links)
}

// parentFrom resolves the directory that would hold the possibly unclean
// path p, walked from directory start as walk does, and returns it with p's
// last element. It fails with ENOTDIR when that is not a directory, with
// ENOENT for an empty p (an empty link target), and with EEXIST when p
// ends in "." or ".." and resolves, since it then names an existing
// directory. start, when given, is a directory.
func (ufs *UnionFS) parentFrom(op string, start *node, p string, links *int) (*node, string, error) {
	if p == "" {
		return nil, "", &os.PathError{Op: op, Path: p, Err: os.ErrNotExist}
	}
	trimmed := strings.TrimRight(p, "/")
	i := strings.LastIndex(trimmed, "/")
	dirPart, base := trimmed[:i+1], trimmed[i+1:]
	if base == "" || base == "." || base == ".." {
		if _, err := ufs.walk(start, p, true, links); err != nil {
			return nil, "", err
		}
		return nil, "", &os.PathError{Op: op, Path: p, Err: os.ErrExist}
	}
	// A non-empty dirPart ends in a slash, so walk itself refuses a
	// non-directory at its end; an empty one is start or the root.
	dir, err := ufs.walk(start, dirPart, true, links)
	if err != nil {
		return nil, "", err
	}
	return dir, base, nil
}

// lowerHas reports whether a layer below the writable one would show name
// inside dir if the writable layer had neither it nor its whiteout: that is,
// whether deleting name needs a whiteout, and creating a directory there
// needs an opaque marker.
func (ufs *UnionFS) lowerHas(dir *node, name string) (bool, error) {
	p := path.Join(dir.path, name)
	for k, li := range dir.layers {
		if li == 0 && ufs.writableLayer != nil {
			continue
		}
		found, err := ufs.layerProbe(li, p)
		if err != nil || found {
			return found, err
		}
		if k == len(dir.layers)-1 || !ufs.honours(li) {
			continue
		}
		whited, err := ufs.layerProbe(li, WhiteoutName(p))
		if err != nil || whited {
			return false, err
		}
	}
	return false, nil
}

// honours reports whether the markers in layer li count: in every layer by
// default, only in the writable layer with WithMarkersOnlyInWritableLayer.
func (ufs *UnionFS) honours(li int) bool {
	return !ufs.upperMarkersOnly || li == 0 && ufs.writableLayer != nil
}

// reservedErr refuses a change to n when n, or the directory it is in, has
// a name in the reserved space: only a read-only layer can have one, under
// WithMarkersOnlyInWritableLayer, and the writable layer cannot hold it.
func reservedErr(n *node) error {
	if IsWhiteout(path.Base(n.path)) {
		return &os.PathError{Op: "copyup", Path: n.path, Err: syscall.EPERM}
	}
	return nil
}

// listDir returns the merged entries of directory dir, sorted by name.
// Entries from upper layers shadow those below; a whiteout in a layer hides
// its name in the layers below it; whiteouts and opaque markers themselves
// are never listed.
func (ufs *UnionFS) listDir(dir *node) ([]fs.DirEntry, error) {
	seen := make(map[string]bool)
	metas := make(map[string]fs.DirEntry) // metadata markers in the layers above
	var out []fs.DirEntry
	for k, li := range dir.layers {
		ents, err := ufs.layers[li].fs.ReadDir(dir.path)
		if err != nil {
			// A lower layer that lost the directory since it was resolved
			// contributes nothing; any other failure would make the
			// listing silently incomplete, so it is reported.
			if k > 0 && isNotExist(err) {
				continue
			}
			return nil, err
		}
		var hidden []string
		var marked []fs.DirEntry
		for _, e := range ents {
			name := e.Name()
			if name == "." || name == ".." {
				continue
			}
			if IsWhiteout(name) && ufs.honours(li) {
				if t, ok := WhiteoutTarget(name); ok {
					hidden = append(hidden, t)
				} else if IsMetaMarker(name) {
					marked = append(marked, e)
				}
				continue
			}
			if seen[name] {
				continue
			}
			seen[name] = true
			if m, ok := metas[name]; ok && e.Type().IsRegular() {
				e = metaDirEntry{DirEntry: e, meta: m}
			}
			out = append(out, e)
		}
		for _, h := range hidden {
			seen[h] = true
		}
		for _, m := range marked {
			t, _ := MetaTarget(m.Name())
			if _, ok := metas[t]; !ok {
				metas[t] = m
			}
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
