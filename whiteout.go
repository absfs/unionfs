package unionfs

import (
	"path"
	"path/filepath"
	"sort"
	"strings"

	"github.com/absfs/absfs"
)

// IsWhiteout reports whether name, a base name or a path, names a whiteout
// marker: its last element starts with WhiteoutPrefix. The opaque marker
// (OpaqueWhiteout) is a whiteout by this test too; use IsOpaqueMarker or
// WhiteoutTarget to tell them apart.
//
// IsWhiteout is case-sensitive: it recognises markers as the union writes
// them. A union reserves the prefix in every ASCII case (".WH.", ".Wh."),
// since a host that folds case takes such a name for a marker: names that
// start with it never appear in the merged view and cannot be opened,
// changed, renamed or removed through it, and a client cannot create one.
// Only the lowercase prefix ever hides anything.
// Under WithMarkersOnlyInWritableLayer such names in a read-only layer are
// ordinary entries instead; see ReservedNames.
func IsWhiteout(name string) bool {
	return strings.HasPrefix(path.Base(name), WhiteoutPrefix)
}

// reservedForClient reports whether a client may not create an entry
// named base: one whose first four bytes are WhiteoutPrefix in any ASCII
// case. A host that folds case (macOS and Windows by default) takes
// ".WH.x" for the marker ".wh.x", so such a name would act as the union's
// own marker; the name is refused on every host, since one layer can span
// volumes that fold differently. Only ASCII case matters: '.' has no
// case, Unicode folds nothing but 'W' and 'H' to 'w' and 'h', and a probe of
// every code point in each position on APFS, normalisation included, found
// no other name the host takes for one starting with ".wh.".
func reservedForClient(base string) bool {
	if len(base) < len(WhiteoutPrefix) {
		return false
	}
	for i := 0; i < len(WhiteoutPrefix); i++ {
		c := base[i]
		if 'A' <= c && c <= 'Z' {
			c += 'a' - 'A'
		}
		if c != WhiteoutPrefix[i] {
			return false
		}
	}
	return true
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
// OCI opaque marker ".wh..wh..opq", metadata markers (see MetaTarget), and
// leftovers ".wh..wh.unionfs.<n>" that the union may leave when a layer
// fails part way through a delete.
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

// IsMetaMarker reports whether name, a base name or a path, names a
// metadata marker: its last element is MetaPrefix followed by a name. A
// metadata marker is a whiteout by IsWhiteout's test too, since every name in
// the reserved ".wh..wh." space is, but it hides nothing: WhiteoutTarget
// returns false for it.
func IsMetaMarker(name string) bool {
	_, ok := MetaTarget(name)
	return ok
}

// MetaTarget maps a metadata marker to the file whose metadata it records,
// in the same directory: "a/b/.wh..wh.meta.c" gives "a/b/c" and
// ".wh..wh.meta.c" gives "c". It returns ok false for anything else,
// including the bare prefix.
//
// When reading an upper layer as a changeset, every marker M with
// MetaTarget(M) = (P, true) records a metadata-only change of the regular
// file P, which a lower layer has and the upper does not: P's new
// permission bits, modification time and owner are M's own, and its
// contents are unchanged.
func MetaTarget(name string) (target string, ok bool) {
	dir, base := path.Split(name)
	if !strings.HasPrefix(base, MetaPrefix) || base == MetaPrefix {
		return "", false
	}
	return dir + strings.TrimPrefix(base, MetaPrefix), true
}

// MetaMarkerName returns the metadata marker that records a metadata-only
// change of name, in the same directory: "a/b/c" gives "a/b/.wh..wh.meta.c"
// and "c" gives ".wh..wh.meta.c". For a name whose last element is not
// empty, it is the inverse of MetaTarget.
func MetaMarkerName(name string) string {
	dir, base := path.Split(name)
	return dir + MetaPrefix + base
}

// opaquePath returns the opaque marker path inside directory dir.
func opaquePath(dir string) string {
	return path.Join(dir, OpaqueWhiteout)
}

// ReservedNames walks the tree under root in fsys, without following
// symbolic links, and returns the slash-separated path of every entry whose
// name starts with WhiteoutPrefix in any ASCII case (".wh.", ".WH."), sorted.
// It does not descend into such a directory: everything under it is reserved
// with it.
//
// Run it on a tree before using it as a read-only layer. By default a union
// takes every such name in any layer for a marker (a whiteout, opaque
// marker, metadata marker or reserved name): it never appears in the merged
// view, and a whiteout among them hides a name in the layers below. A tree
// for which ReservedNames returns nothing is represented exactly; for one
// that has such names and is not a captured changeset, use
// WithMarkersOnlyInWritableLayer, or another backend. In a writable layer
// the names are the union's own markers.
func ReservedNames(fsys absfs.FileSystem, root string) ([]string, error) {
	var out []string
	var walk func(dir string) error
	walk = func(dir string) error {
		ents, err := fsys.ReadDir(dir)
		if err != nil {
			return err
		}
		for _, e := range ents {
			name := e.Name()
			if name == "." || name == ".." {
				continue
			}
			p := path.Join(dir, name)
			switch {
			case reservedForClient(name):
				out = append(out, p)
			case e.IsDir():
				if err := walk(p); err != nil {
					return err
				}
			}
		}
		return nil
	}
	if err := walk(path.Clean(filepath.ToSlash(root))); err != nil {
		return nil, err
	}
	sort.Strings(out)
	return out, nil
}
