package unionfs

import (
	"path"
	"strings"
)

// IsWhiteout reports whether name, a base name or a path, names a whiteout
// marker: its last element starts with WhiteoutPrefix. The opaque marker
// (OpaqueWhiteout) is a whiteout by this test too; use IsOpaqueMarker or
// WhiteoutTarget to tell them apart.
//
// Whiteout markers never appear in a union's merged view, and a union refuses
// to create a file, directory or symlink whose name starts with the prefix.
func IsWhiteout(name string) bool {
	return strings.HasPrefix(path.Base(name), WhiteoutPrefix)
}

// IsOpaqueMarker reports whether name, a base name or a path, is the opaque
// directory marker OpaqueWhiteout. A directory in an upper layer that holds
// this marker replaces the directory of the same path in every lower layer:
// none of the lower directory's entries show through it.
func IsOpaqueMarker(name string) bool {
	return path.Base(name) == OpaqueWhiteout
}

// WhiteoutTarget maps a whiteout marker to the name it deletes, in the same
// directory: "a/b/.wh.c" gives "a/b/c" and ".wh.c" gives "c". It returns ok
// false for anything that is not a whiteout, for the opaque marker, for the
// bare prefix ".wh.", and for names starting with the prefix twice
// (".wh..wh."), which are reserved for metadata as in AUFS and OCI: the
// OCI opaque marker ".wh..wh..opq", and leftovers ".wh..wh.unionfs.<n>"
// that the union may leave when a layer fails part way through a delete.
//
// When reading an upper layer as a changeset, every whiteout W with
// WhiteoutTarget(W) = (P, true) records the deletion of P.
func WhiteoutTarget(name string) (target string, ok bool) {
	dir, base := path.Split(name)
	if !strings.HasPrefix(base, WhiteoutPrefix) || base == OpaqueWhiteout ||
		strings.HasPrefix(base, WhiteoutPrefix+WhiteoutPrefix) {
		return "", false
	}
	deleted := strings.TrimPrefix(base, WhiteoutPrefix)
	if deleted == "" {
		return "", false
	}
	return dir + deleted, true
}

// WhiteoutName returns the whiteout marker that deletes name, in the same
// directory: "a/b/c" gives "a/b/.wh.c" and "c" gives ".wh.c". For a name
// whose last element is not empty and does not itself start with
// WhiteoutPrefix, it is the inverse of WhiteoutTarget.
func WhiteoutName(name string) string {
	dir, base := path.Split(name)
	return dir + WhiteoutPrefix + base
}

// opaquePath returns the opaque marker path inside directory dir.
func opaquePath(dir string) string {
	return path.Join(dir, OpaqueWhiteout)
}
