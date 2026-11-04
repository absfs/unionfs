package unionfs

import (
	"errors"
	"os"
	"path"
	"path/filepath"
	"runtime"
	"sort"
	"syscall"
	"testing"
	"time"

	"github.com/absfs/absfs"
	"github.com/absfs/basefs"
	"github.com/absfs/osfs"
)

// The composed and decomposed spellings of "é".
const (
	nfc = "é"
	nfd = "é"
)

// spellUnion returns a union over layers that fold case and preserve it
// (see foldedLayers), whose lower stores the directory d holding x, the
// empty directory m, the files f and e, and the link k to f.
func spellUnion(t *testing.T, kind string, cache []Option) (ufs *UnionFS, upper, lower absfs.SymlinkFileSystem) {
	t.Helper()
	upper, lower = foldedLayers(t, kind)
	mustMkdirAll(t, lower, "/d")
	mustMkdirAll(t, lower, "/m")
	put(t, lower, "/d/x", "x")
	put(t, lower, "/f", "f")
	put(t, lower, "/e", "e")
	must(t, lower.Symlink("f", "/k"))
	ufs = New(append([]Option{WithWritableLayer(upper), WithReadOnlyLayer(lower)}, cache...)...)
	// Both spellings cached, where the cache is on.
	for _, p := range []string{"/d/x", "/D/X", "/f", "/F", "/e", "/E", "/m", "/M"} {
		lstat(t, ufs, p)
	}
	return ufs, upper, lower
}

// tree returns every path in fsys, as the layer stores it, sorted.
func tree(t *testing.T, fsys absfs.FileSystem) []string {
	t.Helper()
	out := []string{}
	var walk func(dir string)
	walk = func(dir string) {
		ents, err := fsys.ReadDir(dir)
		must(t, err)
		for _, e := range ents {
			p := path.Join(dir, e.Name())
			out = append(out, p)
			if e.IsDir() {
				walk(p)
			}
		}
	}
	walk("/")
	sort.Strings(out)
	return out
}

// A whiteout for an entry a lower layer has is spelled as the lower stores
// it, whatever spelling the caller used: a changeset reader matches it to
// the lower name byte for byte, and so does the union's listing.
func TestStoredSpellingWhiteouts(t *testing.T) {
	for _, kind := range foldedKinds {
		withCache(t, kind, func(t *testing.T, cache []Option) {
			ufs, upper, _ := spellUnion(t, kind, cache)
			must(t, ufs.RemoveAll("/D"))
			must(t, ufs.Remove("/F"))
			must(t, ufs.Remove("/M"))
			wantNames(t, tree(t, upper), "/.wh.d", "/.wh.f", "/.wh.m")
			wantNames(t, list(t, ufs, "/"), "e", "k")
			wantNames(t, changeset(t, upper), "D /d", "D /f", "D /m")
			for _, p := range []string{"/d", "/D", "/d/x", "/f", "/F", "/m", "/M"} {
				wantNotExist(t, ufs, p)
			}

			// Renamed away: the old name's whiteout is the stored one too.
			ufs, upper, _ = spellUnion(t, kind, cache)
			must(t, ufs.Rename("/E", "/g"))
			must(t, ufs.Rename("/D", "/h"))
			wantNames(t, tree(t, upper), "/.wh.d", "/.wh.e", "/g", "/h", "/h/x")
			wantNames(t, list(t, ufs, "/"), "f", "g", "h", "k", "m")
			wantNames(t, changeset(t, upper), "D /d", "D /e", "W /g", "W /h", "W /h/x")
			wantNotExist(t, ufs, "/e")
			wantNotExist(t, ufs, "/D")
		})
	}
}

// Metadata markers and copies of lower entries are spelled as the lower
// stores them.
func TestStoredSpellingCopyUp(t *testing.T) {
	for _, kind := range foldedKinds {
		withCache(t, kind, func(t *testing.T, cache []Option) {
			ufs, upper, _ := spellUnion(t, kind, cache)
			must(t, ufs.Chmod("/F", 0600))
			must(t, ufs.Chmod("/F", 0640)) // the same marker
			wantNames(t, tree(t, upper), "/.wh..wh.meta.f")
			wantMode(t, "/f", lstat(t, ufs, "/f"), 0640)
			ents, err := ufs.ReadDir("/")
			must(t, err)
			for _, e := range ents {
				if e.Name() == "f" {
					info, err := e.Info()
					must(t, err)
					wantMode(t, "listed f", info, 0640)
				}
			}

			f, err := ufs.OpenFile("/E", os.O_WRONLY|os.O_APPEND, 0)
			must(t, err)
			_, err = f.Write([]byte("+"))
			must(t, err)
			must(t, f.Close())
			put(t, ufs, "/D/X", "new x")
			must(t, ufs.Chmod("/M", 0700))
			must(t, ufs.Lchown("/K", os.Getuid(), os.Getgid()))
			wantNames(t, tree(t, upper), "/.wh..wh.meta.f", "/d", "/d/x", "/e", "/k", "/m")
			wantNames(t, changeset(t, upper), "W /.wh..wh.meta.f", "W /d", "W /d/x", "W /e", "W /k", "W /m")
			wantNames(t, list(t, ufs, "/"), "d", "e", "f", "k", "m")
			if get(t, ufs, "/e") != "e+" || get(t, ufs, "/d/x") != "new x" || get(t, ufs, "/K") != "f" {
				t.Fatal("contents")
			}
			// The copy replaces the marker.
			must(t, ufs.Truncate("/F", 0))
			wantNames(t, tree(t, upper), "/d", "/d/x", "/e", "/f", "/k", "/m")
			wantMode(t, "/f", lstat(t, ufs, "/F"), 0640)
		})
	}
}

// Renaming onto an entry only lower layers have keeps the entry's
// spelling, as a host replacing an entry does. Renaming an entry to another
// spelling of its own name changes the spelling: the old name is whited
// out and the new one written.
func TestStoredSpellingRenameTargets(t *testing.T) {
	for _, kind := range foldedKinds {
		withCache(t, kind, func(t *testing.T, cache []Option) {
			ufs, upper, _ := spellUnion(t, kind, cache)
			put(t, ufs, "/new", "n")
			must(t, ufs.Rename("/new", "/E"))
			must(t, ufs.Mkdir("/n", 0755))
			must(t, ufs.Rename("/n", "/M"))
			wantNames(t, tree(t, upper), "/e", "/m", "/m/"+OpaqueWhiteout)
			wantNames(t, list(t, ufs, "/"), "d", "e", "f", "k", "m")
			if get(t, ufs, "/E") != "n" {
				t.Fatal("contents")
			}

			ufs, upper, _ = spellUnion(t, kind, cache)
			must(t, ufs.Rename("/f", "/F"))
			must(t, ufs.Rename("/d", "/D"))
			wantNames(t, tree(t, upper), "/.wh.d", "/.wh.f", "/D", "/D/"+OpaqueWhiteout, "/D/x", "/F")
			wantNames(t, changeset(t, upper), "D /d", "D /f", "R /D", "W /D", "W /D/x", "W /F")
			wantNames(t, list(t, ufs, "/"), "D", "F", "e", "k", "m")
			if get(t, ufs, "/f") != "f" || get(t, ufs, "/d/x") != "x" {
				t.Fatal("contents")
			}
		})
	}
}

// A name new to the merged view keeps the caller's spelling, also where it
// spells a deleted lower name otherwise: the lower name's whiteout stays,
// and a changeset records both.
func TestStoredSpellingRecreated(t *testing.T) {
	for _, kind := range foldedKinds {
		withCache(t, kind, func(t *testing.T, cache []Option) {
			ufs, upper, _ := spellUnion(t, kind, cache)
			// Without the other spellings cached: see
			// TestLookupCacheStaleSpellingNeverPersisted.
			ufs.ClearCache()
			must(t, ufs.Remove("/f"))
			put(t, ufs, "/F", "new")
			must(t, ufs.RemoveAll("/d"))
			must(t, ufs.Mkdir("/D", 0755))
			must(t, ufs.Remove("/e"))
			must(t, ufs.Symlink("F", "/E"))
			wantNames(t, tree(t, upper), "/.wh.d", "/.wh.e", "/.wh.f", "/D", "/D/"+OpaqueWhiteout, "/E", "/F")
			wantNames(t, changeset(t, upper), "D /d", "D /e", "D /f", "R /D", "W /D", "W /E", "W /F")
			wantNames(t, list(t, ufs, "/"), "D", "E", "F", "k", "m")
			wantNames(t, list(t, ufs, "/d"))
			if get(t, ufs, "/f") != "new" || get(t, ufs, "/e") != "new" {
				t.Fatal("contents")
			}
			// Gone again: the one whiteout.
			must(t, ufs.Remove("/F"))
			must(t, ufs.Remove("/D"))
			must(t, ufs.Remove("/E"))
			wantNames(t, tree(t, upper), "/.wh.d", "/.wh.e", "/.wh.f")
			// The same spelling again: the whiteout goes.
			put(t, ufs, "/f", "again")
			wantNames(t, tree(t, upper), "/.wh.d", "/.wh.e", "/f")
		})
	}
}

// Markers spelled otherwise than the lower name, as the union wrote them
// before, or in a layer below: the listing agrees with lookups.
func TestStoredSpellingListingAgreesWithLookup(t *testing.T) {
	for _, kind := range foldedKinds {
		withCache(t, kind, func(t *testing.T, cache []Option) {
			ufs, upper, _ := spellUnion(t, kind, cache)
			put(t, upper, "/.wh.D", "")
			put(t, upper, "/.wh..wh.meta.F", "")
			must(t, upper.Chmod("/.wh..wh.meta.F", 0600))
			mustMkdirAll(t, upper, "/M")
			put(t, upper, "/M/y", "y")
			ufs.ClearCache()
			wantNames(t, list(t, ufs, "/"), "M", "e", "f", "k")
			wantNotExist(t, ufs, "/d")
			wantMode(t, "/f", lstat(t, ufs, "/f"), 0600)
			ents, err := ufs.ReadDir("/")
			must(t, err)
			for _, e := range ents {
				info, err := e.Info()
				must(t, err)
				if e.Name() == "f" {
					wantMode(t, "listed f", info, 0600)
				}
			}
			wantNames(t, list(t, ufs, "/m"), "y")
		})
	}

	// Two lower layers spelling one name differently: the upper one's
	// entry, as a lookup finds.
	top, bottom := newFoldFS(t), newFoldFS(t)
	put(t, top, "/a", "top")
	put(t, bottom, "/A", "bottom")
	put(t, bottom, "/b", "b")
	ufs := New(WithWritableLayer(newFoldFS(t)), WithReadOnlyLayer(top), WithReadOnlyLayer(bottom))
	wantNames(t, list(t, ufs, "/"), "a", "b")
	if get(t, ufs, "/A") != "top" {
		t.Fatal("contents")
	}
	must(t, ufs.Remove("/A"))
	wantNames(t, list(t, ufs, "/"), "b")
}

// A writable layer that does not fold over a lower that does: entries keep
// the caller's spelling, as before, so that it keeps reaching them.
func TestStoredSpellingWritableLayerDoesNotFold(t *testing.T) {
	for _, cached := range []bool{false, true} {
		upper := mustNewMemFS().(absfs.SymlinkFileSystem)
		lower := newFoldFS(t)
		mustMkdirAll(t, lower, "/d")
		put(t, lower, "/d/x", "x")
		for _, p := range []string{"/f", "/e", "/g"} {
			put(t, lower, p, p)
		}
		opts := []Option{WithWritableLayer(upper), WithReadOnlyLayer(lower)}
		if cached {
			opts = append(opts, WithLookupCache(0, 0))
		}
		ufs := New(opts...)
		must(t, ufs.Remove("/F"))
		must(t, ufs.Chmod("/E", 0600))
		put(t, ufs, "/D/X", "new")
		must(t, ufs.Rename("/G", "/h"))
		wantNames(t, tree(t, upper), "/.wh..wh.meta.E", "/.wh.F", "/.wh.G", "/D", "/D/X", "/h")
		wantNotExist(t, ufs, "/F")
		wantMode(t, "/E", lstat(t, ufs, "/E"), 0600)
		if get(t, ufs, "/D/X") != "new" {
			t.Fatal("contents")
		}
	}
}

// A writable layer that fails to give an entry the caller's spelling: the
// change fails, and leaves nothing behind.
func TestStoredSpellingRespellFails(t *testing.T) {
	lower := newFoldFS(t)
	mustMkdirAll(t, lower, "/d")
	put(t, lower, "/d/x", "x")
	put(t, lower, "/f", "f")
	put(t, lower, "/e", "e")
	must(t, lower.Symlink("f", "/k"))
	upper := newFaultFS(t)
	ufs := New(WithWritableLayer(upper), WithReadOnlyLayer(lower))
	for _, tc := range []struct {
		fail   string
		change func() error
	}{
		{"/.wh.f", func() error { return ufs.Remove("/F") }},
		{"/.wh.f", func() error { return ufs.Rename("/F", "/g") }},
		{"/.wh..wh.meta.e", func() error { return ufs.Chmod("/E", 0600) }},
		{"/e", func() error { return ufs.Truncate("/E", 0) }},
		{"/k", func() error { return ufs.Lchown("/K", os.Getuid(), os.Getgid()) }},
		{"/d", func() error { return ufs.Truncate("/D/x", 0) }},
	} {
		upper.failOn("rename", tc.fail)
		if err := tc.change(); !errors.Is(err, errInjected) {
			t.Fatalf("respelling %s: err = %v", tc.fail, err)
		}
		upper.clear()
		if got := tree(t, upper); len(got) != 0 && !(len(got) == 1 && got[0] == "/F") {
			t.Fatalf("respelling %s left %q", tc.fail, got)
		}
		_ = upper.RemoveAll("/F")
	}
	wantNames(t, list(t, ufs, "/"), "d", "e", "f", "k")

	// A listing whose lookup of an aliased name fails reports it.
	put(t, upper, "/.wh.D", "")
	upper.failOn("lstat", "/d")
	if _, err := ufs.ReadDir("/"); !errors.Is(err, errInjected) {
		t.Fatalf("ReadDir: %v", err)
	}
}

// Names with no letters, names the layer cannot list, and names it stores
// as given.
func TestStoredName(t *testing.T) {
	lower := newFaultFS(t)
	put(t, lower, "/1", "1")
	put(t, lower, "/"+nfc, "e")
	put(t, lower, "/a", "a")
	put(t, lower, "/A", "A")
	ufs := New(WithWritableLayer(newFoldFS(t)), WithReadOnlyLayer(lower))
	for _, name := range []string{"1", "a", "A", nfc} {
		if got := ufs.storedName(1, "/", name); got != name {
			t.Fatalf("storedName(%q) = %q", name, got)
		}
	}
	for _, op := range []string{"readdir", "lstat"} {
		lower.failOn(op, "/")
		if got := ufs.storedName(1, "/", nfc); got != nfc {
			t.Fatalf("storedName(%q) = %q", nfc, got)
		}
		lower.clear()
	}

	// A normalisation alias, told by identity.
	fold := newFoldFS(t)
	mustMkdirAll(t, fold, "/z")
	put(t, fold, "/"+nfd, "nfd")
	put(t, fold, "/other"+nfc, "o")
	ufs = New(WithWritableLayer(newFoldFS(t)), WithReadOnlyLayer(fold))
	if got := ufs.storedName(1, "/", nfc); got != nfd {
		t.Fatalf("storedName(%q) = %q", nfc, got)
	}
	if got := ufs.storedName(1, "/", "Missing"+nfc); got != "Missing"+nfc {
		t.Fatalf("storedName = %q", got)
	}
}

func TestAliasKey(t *testing.T) {
	for _, tc := range []struct {
		a, b string
		same bool
	}{
		{"D", "d", true},
		{"ReadMe.TXT", "readme.txt", true},
		{nfc, nfd, true},
		{"caf" + nfc + ".txt", "CAF" + nfd + ".TXT", true},
		{"가", "가", true}, // Hangul, composed and not
		{"d", "e", false},
		{"e", nfd, false},
		{"a" + nfc, nfc, false},
	} {
		if got := aliasKey(tc.a) == aliasKey(tc.b); got != tc.same {
			t.Errorf("aliasKey(%q) == aliasKey(%q): %v, want %v", tc.a, tc.b, got, tc.same)
		}
	}
}

// On a macOS host, which takes the composed and decomposed forms of a name
// for one another even on a case-sensitive volume, markers and copies are
// spelled with the lower's bytes.
func TestStoredSpellingHostNormalisation(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("host-directory layers are exercised on Unix")
	}
	root, err := filepath.EvalSymlinks(t.TempDir())
	must(t, err)
	for _, d := range []string{"upper", "lower"} {
		must(t, os.Mkdir(filepath.Join(root, d), 0755))
	}
	lowerDir := filepath.Join(root, "lower")
	for _, name := range []string{"f" + nfd, "g" + nfd, "h" + nfd} {
		must(t, os.WriteFile(filepath.Join(lowerDir, name), []byte(name), 0644))
	}
	must(t, os.Mkdir(filepath.Join(lowerDir, "d"+nfd), 0755))
	must(t, os.WriteFile(filepath.Join(lowerDir, "d"+nfd, "x"), nil, 0644))
	if _, err := os.Lstat(filepath.Join(lowerDir, "f"+nfc)); err != nil {
		t.Skip("the host does not normalise names here")
	}
	host, err := osfs.NewFS()
	must(t, err)
	lower, err := basefs.NewFS(host, lowerDir)
	must(t, err)
	for _, cached := range []bool{false, true} {
		// A fresh layer over a fresh directory: basefs holds its root
		// open, so it would not see one made in place of the old.
		must(t, os.RemoveAll(filepath.Join(root, "upper")))
		must(t, os.Mkdir(filepath.Join(root, "upper"), 0755))
		upper, err := basefs.NewFS(host, filepath.Join(root, "upper"))
		must(t, err)
		opts := []Option{WithWritableLayer(upper), WithReadOnlyLayer(lower)}
		if cached {
			opts = append(opts, WithLookupCache(0, 0))
		}
		ufs := New(opts...)
		must(t, ufs.Remove("/f"+nfc))
		must(t, ufs.Chmod("/g"+nfc, 0600))
		put(t, ufs, "/h"+nfc, "new")
		put(t, ufs, "/d"+nfc+"/x", "new x")
		ents, err := os.ReadDir(filepath.Join(root, "upper"))
		must(t, err)
		var got []string
		for _, e := range ents {
			got = append(got, e.Name())
		}
		sort.Strings(got)
		wantNames(t, got, ".wh..wh.meta.g"+nfd, ".wh.f"+nfd, "d"+nfd, "h"+nfd)
		wantNames(t, list(t, ufs, "/"), "d"+nfd, "g"+nfd, "h"+nfd)
		wantNames(t, changeset(t, upper), "D /f"+nfd, "W /.wh..wh.meta.g"+nfd, "W /d"+nfd, "W /d"+nfd+"/x", "W /h"+nfd)
	}
	_ = syscall.ENOENT
}

// sameIn finds nothing for a file that is not among the entries.
func TestSameInNone(t *testing.T) {
	fsys := mustNewMemFS()
	put(t, fsys, "/a", "a")
	put(t, fsys, "/b", "b")
	ents, err := fsys.ReadDir("/")
	must(t, err)
	info, err := fsys.Stat("/b")
	must(t, err)
	if got := sameIn(ents[:1], info); got != "" {
		t.Fatalf("sameIn = %q", got)
	}
}

// The listing storedName reads is read again only when the directory's
// modification time changes.
func TestStoredNameListingKept(t *testing.T) {
	for _, kind := range foldedKinds {
		t.Run(kind, func(t *testing.T) {
			upper, lower := foldedLayers(t, kind)
			mustMkdirAll(t, lower, "/d")
			for _, n := range []string{"a", "b", "c", "e"} {
				put(t, lower, "/d/"+n, n)
			}
			old := changedTime.AddDate(-10, 0, 0)
			must(t, lower.Chtimes("/d", old, old))
			ufs := New(WithWritableLayer(upper), WithReadOnlyLayer(lower))
			must(t, ufs.Remove("/d/A"))
			kept := ufs.spelled
			if kept == nil || kept.dir != "/d" {
				t.Fatalf("listing not kept: %+v", kept)
			}
			must(t, ufs.Chmod("/d/C", 0600))
			if ufs.spelled != kept {
				t.Fatal("listing read again")
			}
			// Changed directly: read again.
			must(t, lower.Rename("/d/b", "/d/tmp"))
			must(t, lower.Rename("/d/tmp", "/d/B"))
			must(t, lower.Chtimes("/d", old.Add(time.Second), old.Add(time.Second)))
			must(t, ufs.Remove("/d/b"))
			if ufs.spelled == kept {
				t.Fatal("stale listing used")
			}
			must(t, ufs.Remove("/d/E"))
			wantNames(t, tree(t, upper), "/d", "/d/.wh..wh.meta.c", "/d/.wh.B", "/d/.wh.a", "/d/.wh.e")
		})
	}
}

// Renaming onto an entry the writable layer already has a copy of, which a
// lower layer has too, keeps the copy's spelling: the copy is the lower
// entry's, and a new spelling would leave the lower entry undeleted in a
// changeset.
func TestStoredSpellingRenameOntoCopy(t *testing.T) {
	for _, kind := range foldedKinds {
		withCache(t, kind, func(t *testing.T, cache []Option) {
			ufs, upper, _ := spellUnion(t, kind, cache)
			must(t, ufs.Remove("/d/x"))
			must(t, ufs.Mkdir("/n", 0755))
			must(t, ufs.Rename("/n", "/D"))
			must(t, ufs.Truncate("/e", 0))
			put(t, ufs, "/x", "new")
			must(t, ufs.Rename("/x", "/E"))
			wantNames(t, tree(t, upper), "/d", "/d/"+OpaqueWhiteout, "/e")
			wantNames(t, changeset(t, upper), "R /d", "W /d", "W /e")
			wantNames(t, list(t, ufs, "/"), "d", "e", "f", "k", "m")
			wantNames(t, list(t, ufs, "/D"))
			if get(t, ufs, "/E") != "new" {
				t.Fatal("contents")
			}
		})
	}
}

// The same in a directory reached through other spellings of its own and
// its parent's names, and for an entry only the writable layer has.
func TestStoredSpellingRenameOntoCopyNested(t *testing.T) {
	for _, kind := range foldedKinds {
		withCache(t, kind, func(t *testing.T, cache []Option) {
			for _, dst := range []string{"/A/B/S", "/a/B/S", "/a/b/S"} {
				upper, lower := foldedLayers(t, kind)
				mustMkdirAll(t, lower, "/a/b/s")
				put(t, lower, "/a/b/s/y", "y")
				ufs := New(append([]Option{WithWritableLayer(upper), WithReadOnlyLayer(lower)}, cache...)...)
				must(t, ufs.Remove("/a/b/s/y"))
				must(t, ufs.Mkdir("/a/n", 0755))
				must(t, ufs.Rename("/a/n", dst))
				put(t, ufs, "/a/new", "1")
				put(t, ufs, "/a/x", "2")
				must(t, ufs.Rename("/a/x", "/A/NEW"))
				wantNames(t, tree(t, upper), "/a", "/a/b", "/a/b/s", "/a/b/s/"+OpaqueWhiteout, "/a/new")
				wantNames(t, changeset(t, upper), "R /a/b/s", "W /a", "W /a/b", "W /a/b/s", "W /a/new")
				wantNames(t, list(t, ufs, "/a/b"), "s")
				wantNames(t, list(t, ufs, "/a/b/s"))
				if get(t, ufs, "/a/NEW") != "2" {
					t.Fatal("contents")
				}
			}
		})
	}
}
