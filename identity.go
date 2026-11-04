package unionfs

import (
	"io/fs"
	"os"
	"reflect"

	"github.com/absfs/inode"
)

// A layer need not hand back the os package's own FileInfo, which is all
// os.SameFile can compare: basefs wraps the infos Stat and Lstat return to
// report the caller's name, and the union wraps those of files a metadata
// marker changes. A wrapper keeps the wrapped info's Sys value, though, and
// that is where SameFile looks for an identity.

// infoUnwrapper is a FileInfo that wraps another and hands it back. The
// union's own wrappers are: SameFile looks through them, and through any
// other FileInfo with this method.
type infoUnwrapper interface {
	Unwrap() fs.FileInfo
}

// SameFile reports whether a and b describe the same file. Unlike
// os.SameFile, it also tells for infos a layer wraps, as basefs does and as
// the union does for a file a metadata marker changes, and for memfs
// infos: it compares the *inode.Inode memfs reports, or the device and
// inode numbers of a host stat record on Unix, or else uses os.SameFile.
// It looks through wrappers with an Unwrap() fs.FileInfo method first, so
// that two Lstats of a file with a metadata marker are the same file even
// though each Sys value is a fresh merged record. It reports false when it
// cannot tell, as for a wrapped host info on Windows, which records no
// file ID.
//
// Comparing infos of different layers is safe: two memfs layers can hold
// inodes with equal numbers, so memfs inodes are compared by pointer, and
// host files on different devices differ in their device number.
func SameFile(a, b fs.FileInfo) bool {
	same, _ := sameEntry(a, b)
	return same
}

// sameEntry reports whether a and b, the infos of two names, describe the
// same file, and whether it could tell: known is false when neither
// carries an identity it can compare (see SameFile), and same is then
// false.
func sameEntry(a, b fs.FileInfo) (same, known bool) {
	a, b = unwrapInfo(a), unwrapInfo(b)
	if a == nil || b == nil {
		return false, false
	}
	x, xok := a.Sys().(*inode.Inode)
	y, yok := b.Sys().(*inode.Inode)
	if xok || yok {
		if xok && yok && x != nil && y != nil {
			return x == y, true
		}
		return false, false
	}
	if os.SameFile(a, b) {
		return true, true
	}
	if same, known := hostSameFile(a.Sys(), b.Sys()); known {
		return same, true
	}
	// Two of the os package's own infos that os.SameFile tells apart are
	// different files; otherwise nothing here says.
	return false, isOSInfo(a) && isOSInfo(b)
}

// unwrapInfo returns the innermost info that info wraps, through the
// union's metadata wrapper and any infoUnwrapper.
func unwrapInfo(info fs.FileInfo) fs.FileInfo {
	for i := 0; i < 64 && info != nil; i++ {
		u, ok := info.(infoUnwrapper)
		if !ok {
			break
		}
		info = u.Unwrap()
	}
	return info
}

// isOSInfo reports whether info is the os package's own, which os.SameFile
// compares. Were the type to change its name, an identity the os package
// alone could tell would go unknown, and Rename would compare spellings
// instead (see sameNode).
func isOSInfo(info fs.FileInfo) bool {
	return reflect.TypeOf(info).String() == "*os.fileStat"
}

// Unwrap returns the file's own info, without the marker's changes.
func (i metaInfo) Unwrap() fs.FileInfo { return i.FileInfo }

// Unwrap returns the info under its own name.
func (i namedInfo) Unwrap() fs.FileInfo { return i.FileInfo }

// sameNode reports whether nodes a and b, which two names resolved to, are
// one entry of their top layer. Where the layer's infos tell (see
// sameEntry), they decide. Where they do not, as for a layer that wraps
// its infos and reports no identity in them (basefs on Windows), a and b
// are one entry when their directories are and the layer stores both
// names under one spelling. The caller holds ufs.mu for writing.
func (ufs *UnionFS) sameNode(a, b *node) bool {
	if a == b || a.path == b.path {
		return true
	}
	if a.parent == nil || b.parent == nil || a.top() != b.top() {
		return false
	}
	if same, known := sameEntry(a.info, b.info); known {
		return same
	}
	return ufs.nameIn(a, a.top()) == ufs.nameIn(b, b.top()) && ufs.sameNode(a.parent, b.parent)
}
