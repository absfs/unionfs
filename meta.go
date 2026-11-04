package unionfs

import (
	"io/fs"
	"os"
	"path"
	"time"

	"github.com/absfs/absfs"
	"github.com/absfs/inode"
)

// metaInfo is the FileInfo of a regular file that a lower layer has and a
// metadata marker in a layer above changes: the file's own size, type and
// identity, with the marker's permission bits, modification time and owner.
type metaInfo struct {
	os.FileInfo             // the file's own, from the layer that has it
	meta        os.FileInfo // the marker's
}

// Mode returns the file's type with the marker's permission bits.
func (i metaInfo) Mode() os.FileMode {
	return i.FileInfo.Mode().Type() | i.meta.Mode()&chmodBits
}

// ModTime returns the marker's modification time.
func (i metaInfo) ModTime() time.Time { return i.meta.ModTime() }

// Sys returns the file's own record with the marker's owner, permission
// bits, modification time and change time, when both come from a layer that
// reports them: a host record (*syscall.Stat_t, on Unix) or an *inode.Inode
// (memfs). The access time, size, inode number and link count stay the
// file's own. Anything else is the file's own Sys value.
func (i metaInfo) Sys() any {
	file, meta := i.FileInfo.Sys(), i.meta.Sys()
	if f, ok := file.(*inode.Inode); ok {
		m, ok := meta.(*inode.Inode)
		if !ok || f == nil || m == nil {
			return file
		}
		out := &inode.Inode{Ino: f.Ino, Mode: i.Mode(), Nlink: f.Nlink, Size: f.Size, Uid: m.Uid, Gid: m.Gid}
		out.SetAtime(f.Atime())
		out.SetMtime(m.Mtime())
		out.SetCtime(m.Ctime())
		return out
	}
	if s, ok := mergeHostSys(file, meta); ok {
		return s
	}
	return file
}

// ownerOf returns the owner that info's Sys value records, if it records
// one.
func ownerOf(info os.FileInfo) (uid, gid int, ok bool) {
	if n, isInode := info.Sys().(*inode.Inode); isInode && n != nil {
		return int(n.Uid), int(n.Gid), true
	}
	return hostOwner(info.Sys())
}

// metaDirEntry is a listing entry for a file a metadata marker changes.
type metaDirEntry struct {
	fs.DirEntry             // the file's
	meta        fs.DirEntry // the marker's
}

// Info returns the file's info with the marker's metadata.
func (e metaDirEntry) Info() (fs.FileInfo, error) {
	info, err := e.DirEntry.Info()
	if err != nil {
		return nil, err
	}
	meta, err := e.meta.Info()
	if err != nil {
		return nil, err
	}
	return metaInfo{FileInfo: info, meta: meta}, nil
}

// metaFile is a lower file, open for reading, that a metadata marker
// changes: Stat reports the marker's metadata.
type metaFile struct {
	absfs.File
	meta os.FileInfo
}

// Stat returns the file's info with the marker's metadata.
func (f metaFile) Stat() (os.FileInfo, error) {
	info, err := f.File.Stat()
	if err != nil {
		return nil, err
	}
	return metaInfo{FileInfo: info, meta: f.meta}, nil
}

// findMeta looks for a metadata marker for the regular file n, which the
// k-th contributing layer of its directory dir supplied, in the layers of
// dir above that one, top first, and records the first it finds in n.
func (ufs *UnionFS) findMeta(dir *node, k int, n *node) error {
	if k == 0 {
		return nil
	}
	m := MetaMarkerName(n.path)
	for _, li := range dir.layers[:k] {
		if !ufs.honours(li) {
			continue
		}
		info, err := ufs.layerLstat(li, m)
		if err == nil && isMetaInfo(info) {
			n.attachMeta(info, li)
			return nil
		}
		if err != nil && !isAbsent(err) {
			return err
		}
	}
	return nil
}

// isMetaInfo reports whether info, of an entry at a metadata marker's path,
// is a marker: the union creates them as regular files. Anything else there,
// a symbolic link above all, is not one, and the union never changes its
// attributes, which a host would do to the link's target.
func isMetaInfo(info os.FileInfo) bool {
	return info.Mode().IsRegular()
}

// attachMeta records that the marker with info, in layer li, changes n.
func (n *node) attachMeta(info os.FileInfo, li int) {
	n.meta, n.metaLayer = info, li
	n.info = metaInfo{FileInfo: n.info, meta: info}
}

// upperMeta reports whether n's metadata marker is in the writable layer.
func (ufs *UnionFS) upperMeta(n *node) bool {
	return n.meta != nil && n.metaLayer == 0 && ufs.writableLayer != nil
}

// maxNameLen is the longest name, in bytes, that the hosts unionfs runs on
// accept in a directory (NAME_MAX on Linux, macOS and the BSDs).
const maxNameLen = 255

// metaFits reports whether the metadata marker for p has a name a host
// accepts. A regular file whose name is within len(MetaPrefix) bytes of the
// limit is copied up instead when its metadata changes, as it was before
// markers existed.
func metaFits(p string) bool {
	return len(MetaPrefix)+len(path.Base(p)) <= maxNameLen
}

// setMeta records a metadata-only change of the regular file n, which only
// lower layers have, in its marker in the writable layer: it creates the
// marker, with n's current metadata, if the writable layer has none, and
// then calls set with the marker's path to apply the change to it. A marker
// it created is removed again if set fails. A node from the lookup cache is
// checked against the layers before a marker is created (see current).
func (ufs *UnionFS) setMeta(n *node, set func(marker string) error) error {
	if ufs.upperMeta(n) {
		return set(MetaMarkerName(n.path))
	}
	if err := reservedErr(n); err != nil {
		return err
	}
	// A marker for a file the merged view hides would hold its stale
	// metadata.
	if err := ufs.current(n); err != nil {
		return err
	}
	if err := ufs.copyUpDir(n.parent); err != nil {
		return err
	}
	return ufs.keepDir(n.parent.path, func() error {
		// The marker is spelled after the name n's layer stores it under
		// (see storedName).
		stored := MetaPrefix + ufs.nameIn(n, n.top())
		if err := ufs.createMeta(path.Join(n.parent.path, stored), n.info); err != nil {
			return err
		}
		m, err := ufs.respell(n.parent.path, stored, MetaPrefix+path.Base(n.path))
		if err == nil {
			err = set(m)
		}
		if err != nil {
			_ = ufs.upper().Remove(m)
			return err
		}
		return nil
	})
}

// createMeta creates the metadata marker m in the writable layer with the
// metadata of info: its owner where the layers allow it, its permission
// bits and its modification time.
func (ufs *UnionFS) createMeta(m string, info os.FileInfo) error {
	up := ufs.upper()
	// Exclusive: whatever is at m is not a marker (see isMetaInfo), and
	// must neither be followed, if it is a link, nor taken over.
	f, err := up.OpenFile(m, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	err = f.Close()
	// The owner goes first: a host clears the setuid and setgid bits on
	// a chown by an unprivileged user. Like copy-up, a marker keeps the
	// lower owner only where the union may give it away.
	if uid, gid, ok := ownerOf(info); ok && err == nil {
		_ = up.Chown(m, uid, gid)
	}
	if err == nil {
		err = ufs.chmodUpper(m, 0, info.Mode())
	}
	if err == nil {
		err = up.Chtimes(m, info.ModTime(), info.ModTime())
	}
	if err != nil {
		_ = up.Remove(m)
	}
	return err
}

// dropMeta removes n's metadata marker from the writable layer, if it has
// one.
func (ufs *UnionFS) dropMeta(n *node) error {
	if !ufs.upperMeta(n) {
		return nil
	}
	if err := ufs.upper().Remove(MetaMarkerName(n.path)); err != nil && !isNotExist(err) {
		return err
	}
	n.meta = nil
	return nil
}
