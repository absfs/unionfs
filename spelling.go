package unionfs

import (
	"io/fs"
	"os"
	"path"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// A host may take two spellings for one name: "d" and "D" where it folds
// case (macOS and Windows by default), the composed and decomposed forms of
// "é" where it normalises Unicode (macOS, even on a case-sensitive volume).
// A lookup through either finds the entry, but the entry is stored under
// one of them, and a changeset reader matches markers to names byte for
// byte. So when the union writes a whiteout, a metadata marker or a copy-up
// for an entry a layer already has, it spells it as that layer stores the
// entry, not as the caller did: RemoveAll("/D") of a lower "d" writes
// ".wh.d". A name new to the merged view keeps the caller's spelling.

// storedName returns the spelling under which layer li stores the entry it
// resolves at path.Join(dir, name), which the caller knows it has. That is
// name itself unless the layer aliases spellings there. An ASCII name the
// layer does not also resolve with every letter's case swapped is stored as
// given, at the cost of one Lstat. Any other is looked up in dir's listing
// (see listingOf); a name that matches no entry there in any case (a
// Unicode normalisation alias) costs an Lstat of each entry of its type
// too, to find the one that is the same file. When the layer's spelling
// cannot be told, name is returned. The caller holds ufs.mu for writing.
func (ufs *UnionFS) storedName(li int, dir, name string) string {
	ascii := isASCII(name)
	if ascii {
		swapped := swapCase(name)
		if swapped == name {
			return name
		}
		if _, err := ufs.layerLstat(li, path.Join(dir, swapped)); err != nil {
			return name
		}
	}
	l, err := ufs.listingOf(li, dir)
	if err != nil || l.names[name] {
		return name
	}
	if n, ok := l.folded[strings.ToLower(name)]; ok {
		return n
	}
	if info, err := ufs.layerLstat(li, path.Join(dir, name)); !ascii && err == nil {
		if m := sameIn(l.ents, info); m != "" {
			return m
		}
	}
	return name
}

// listing is a layer directory's entries, as storedName read them.
type listing struct {
	li     int
	dir    string
	mtime  time.Time
	ents   []fs.DirEntry
	names  map[string]bool   // every entry's name
	folded map[string]string // a name lowercased: the first entry's
}

// listingSettle is how long after a directory last changed storedName
// waits before it keeps its listing: a change within the clock's
// resolution of the last one need not change the modification time.
const listingSettle = 2 * time.Second

// listingOf returns the entries of directory dir in layer li. The last
// listing read is kept, and used again while dir's modification time stays
// as it was, so that deleting, or changing the metadata of, every entry of
// a large lower directory one by one reads it once rather than once per
// entry. A listing of a directory changed within listingSettle is not
// kept.
func (ufs *UnionFS) listingOf(li int, dir string) (*listing, error) {
	info, err := ufs.layerLstat(li, dir)
	if err != nil {
		return nil, err
	}
	if l := ufs.spelled; l != nil && l.li == li && l.dir == dir && l.mtime.Equal(info.ModTime()) {
		return l, nil
	}
	ents, err := ufs.layers[li].fs.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	l := &listing{li: li, dir: dir, mtime: info.ModTime(), ents: ents,
		names: make(map[string]bool, len(ents)), folded: make(map[string]string)}
	for _, e := range ents {
		n := e.Name()
		l.names[n] = true
		if k := strings.ToLower(n); l.folded[k] == "" {
			l.folded[k] = n
		}
	}
	if time.Since(info.ModTime()) > listingSettle {
		ufs.spelled = l
	}
	return l, nil
}

// sameIn returns the name of the first of ents that is the same file as
// info, or "".
func sameIn(ents []fs.DirEntry, info os.FileInfo) string {
	for _, e := range ents {
		if e.Type() != info.Mode().Type() {
			continue
		}
		if ei, err := e.Info(); err == nil && SameFile(info, ei) {
			return e.Name()
		}
	}
	return ""
}

// nameIn returns the spelling under which layer li stores node n's last
// element, which li has: see storedName. A node resolved from a listing
// already has it for its top layer.
func (ufs *UnionFS) nameIn(n *node, li int) string {
	base := path.Base(n.path)
	if n.exact && li == n.top() {
		return base
	}
	return ufs.storedName(li, n.parent.path, base)
}

// respell settles the name of an entry the union has just created as
// dir/stored in the writable layer, for the entry the caller names
// dir/given. Where the writable layer takes given for stored, as a host
// folding the same way as the layer stored was read from does, the entry
// stays as it is and its path is returned. Otherwise (a writable layer that
// does not fold over one that does) it is renamed to given, as the union
// spelled every entry before, so that the caller's spelling keeps reaching
// it. With the lookup cache on, the cache's entries for given are dropped
// too: the writable layer dropped stored's.
func (ufs *UnionFS) respell(dir, stored, given string) (string, error) {
	s, g := path.Join(dir, stored), path.Join(dir, given)
	if stored == given {
		return s, nil
	}
	if ufs.lc != nil {
		defer ufs.touched(g)
	}
	if ufs.layerHas(0, g) {
		return s, nil
	}
	if err := ufs.upper().Rename(s, g); err != nil {
		return s, err
	}
	return g, nil
}

// isASCII reports whether s is all ASCII.
func isASCII(s string) bool {
	for i := 0; i < len(s); i++ {
		if s[i] >= utf8.RuneSelf {
			return false
		}
	}
	return true
}

// swapCase returns the ASCII string s with the case of every letter
// swapped.
func swapCase(s string) string {
	b := []byte(s)
	for i, c := range b {
		switch {
		case 'a' <= c && c <= 'z':
			b[i] = c - ('a' - 'A')
		case 'A' <= c && c <= 'Z':
			b[i] = c + ('a' - 'A')
		}
	}
	return string(b)
}

// aliasKey returns a key that two names a host may take for one another
// share: ASCII letters lowercased, and every run of non-ASCII characters,
// with an ASCII letter that a combining mark in the run follows, as one
// NUL (which no name holds). So "D" and "d" share one, as do the composed
// "é" and its decomposed "e" with U+0301. Names that are not aliases may
// share one too ("é" and "ü"): it only picks the names worth a lookup.
func aliasKey(name string) string {
	i := 0
	for i < len(name) && name[i] < utf8.RuneSelf && !('A' <= name[i] && name[i] <= 'Z') {
		i++
	}
	if i == len(name) {
		return name // the common case: its own key
	}
	b := make([]byte, 0, len(name))
	for i := 0; i < len(name); {
		c := name[i]
		if c < utf8.RuneSelf {
			if 'A' <= c && c <= 'Z' {
				c += 'a' - 'A'
			}
			b = append(b, c)
			i++
			continue
		}
		r, size := utf8.DecodeRuneInString(name[i:])
		i += size
		if n := len(b); n > 0 && unicode.Is(unicode.M, r) && 'a' <= b[n-1] && b[n-1] <= 'z' {
			b = b[:n-1]
		}
		if n := len(b); n == 0 || b[n-1] != 0 {
			b = append(b, 0)
		}
	}
	return string(b)
}
