package unionfs

import (
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"

	"github.com/absfs/absfs"
)

// TestDestructiveOpsRefuseDotNames checks that Remove, RemoveAll and Rename
// treat an empty name and a name ending in "." or ".." as the os calls do,
// rather than as the working directory or its parent.
func TestDestructiveOpsRefuseDotNames(t *testing.T) {
	ufs, upper, lower := newTestUnion(t)
	mustMkdirAll(t, lower, "/keep/sub")
	put(t, lower, "/keep/sub/f", "f")
	put(t, ufs, "/mine", "m")
	mustMkdirAll(t, ufs, "/empty")
	before := upperMarkers(t, upper)

	check := func() {
		t.Helper()
		wantNames(t, list(t, ufs, "/"), "empty", "keep", "mine")
		wantNames(t, list(t, ufs, "/keep/sub"), "f")
		wantNames(t, upperMarkers(t, upper), before...)
	}

	// As os.RemoveAll(""), RemoveAll("") does nothing.
	must(t, ufs.RemoveAll(""))
	check()
	for _, name := range []string{".", "./", "keep/..", "keep/sub/.", "/keep/sub/..", "keep/sub/../"} {
		wantErrIs(t, ufs.RemoveAll(name), syscall.EINVAL)
		wantErrIs(t, ufs.Remove(name), syscall.EINVAL)
		wantErrIs(t, ufs.Rename(name, "/moved"), syscall.EINVAL)
		wantErrIs(t, ufs.Rename("/mine", name), syscall.EINVAL)
	}
	check()
	wantErrIs(t, ufs.Remove(""), syscall.ENOENT)
	wantErrIs(t, ufs.Rename("", "/moved"), syscall.ENOENT)
	wantErrIs(t, ufs.Rename("/mine", ""), syscall.ENOENT)
	check()

	// The same from a working directory, which "." would otherwise name.
	must(t, ufs.Chdir("/empty"))
	must(t, ufs.RemoveAll(""))
	wantErrIs(t, ufs.RemoveAll("."), syscall.EINVAL)
	wantErrIs(t, ufs.Remove("."), syscall.EINVAL)
	wantErrIs(t, ufs.Remove(""), syscall.ENOENT)
	check()

	// The errors name the operation and the path as given.
	var pe *os.PathError
	if err := ufs.Remove("."); !errors.As(err, &pe) || pe.Op != "remove" || pe.Path != "." {
		t.Fatalf("Remove error = %#v", err)
	}
	if err := ufs.RemoveAll("."); !errors.As(err, &pe) || pe.Op != "removeall" || pe.Path != "." {
		t.Fatalf("RemoveAll error = %#v", err)
	}
	var le *os.LinkError
	if err := ufs.Rename("", "/x"); !errors.As(err, &le) || le.Op != "rename" {
		t.Fatalf("Rename error = %#v", err)
	}

	// Names that merely contain dots, or end in a slash, still work.
	must(t, ufs.Chdir("/"))
	put(t, ufs, "/a..b", "x")
	must(t, ufs.Remove("./a..b"))
	must(t, ufs.Remove("empty/"))
	must(t, ufs.Rename("keep/./sub", "keep/sub2"))
	wantNames(t, list(t, ufs, "/keep"), "sub2")
	must(t, ufs.RemoveAll("keep/../keep/"))
	wantNames(t, list(t, ufs, "/"), "mine")
	// RemoveAll("/") still empties the root.
	must(t, ufs.RemoveAll("/"))
	wantNames(t, list(t, ufs, "/"))
}

// dotDotTree builds, in fs, the tree that tells a physical ".." in a link
// target from a lexical one:
//
//	/x/deep/deeper/            a directory
//	/x/deep/target/f           "physical"
//	/d/target/f                "lexical"
//	/d/l2 -> ../x/deep/deeper
//	/d/l1 -> l2/../target/f
//
// Resolved as POSIX does, /d/l1 is /x/deep/target/f: the ".." applies to
// the directory l2 resolves to. Cleaned as text it would be /d/target/f.
func dotDotTree(t *testing.T, fs absfs.SymlinkFileSystem) {
	t.Helper()
	mustMkdirAll(t, fs, "/x/deep/deeper")
	mustMkdirAll(t, fs, "/x/deep/target")
	mustMkdirAll(t, fs, "/d/target")
	put(t, fs, "/x/deep/target/f", "physical")
	put(t, fs, "/d/target/f", "lexical")
	symlink(t, fs, "../x/deep/deeper", "/d/l2")
	symlink(t, fs, "l2/../target/f", "/d/l1")
}

// checkDotDot checks the union's view of dotDotTree, whichever layer holds
// it. hostLink, when given, makes the link whose target climbs above the
// layer's root in the writable layer instead of the union, which refuses it
// over a layer that does (basefs).
func checkDotDot(t *testing.T, ufs *UnionFS, hostLink func(target, p string)) {
	t.Helper()
	if got := get(t, ufs, "/d/l1"); got != "physical" {
		t.Fatalf("read through l2/../target/f = %q, want physical", got)
	}
	if got := get(t, ufs, "/d/l2/../target/f"); got != "lexical" {
		// The caller's own path is cleaned first, as os does with Go's
		// path functions: only link targets are walked element by element.
		t.Fatalf("read of /d/l2/../target/f = %q, want lexical", got)
	}

	// Creating through a dangling link with a ".." after a linked
	// directory creates the file where the link resolves.
	symlink(t, ufs, "l2/../target/new", "/d/l3")
	put(t, ufs, "/d/l3", "made")
	if got := get(t, ufs, "/x/deep/target/new"); got != "made" {
		t.Fatalf("created file = %q", got)
	}
	wantNotExist(t, ufs, "/d/target/new")

	// Links to "." and "..", and ".." above the root, which stays there.
	symlink(t, ufs, ".", "/d/self")
	symlink(t, ufs, "..", "/d/up")
	if hostLink != nil {
		hostLink("../../../..", "/d/l2up")
	} else {
		symlink(t, ufs, "../../../..", "/d/l2up")
	}
	wantNames(t, list(t, ufs, "/d/self"), "l1", "l2", "l2up", "l3", "self", "target", "up")
	wantNames(t, list(t, ufs, "/d/up"), "d", "x")
	wantNames(t, list(t, ufs, "/d/l2up"), "d", "x")
	info, err := ufs.Stat("/d/up")
	must(t, err)
	if !info.IsDir() || info.Name() != "up" {
		t.Fatalf("Stat of a link to the root = %v %v", info.Name(), info.Mode())
	}
	// ".." in a link inside a linked directory steps out of the target.
	symlink(t, ufs, "../target/f", "/x/deep/deeper/back")
	if got := get(t, ufs, "/d/l2/back"); got != "physical" {
		t.Fatalf("read through /d/l2/back = %q", got)
	}

	// ".." after a non-directory is ENOTDIR, as on a host.
	symlink(t, ufs, "target/f/..", "/d/notdir")
	if _, err := ufs.Stat("/d/notdir"); !errors.Is(err, syscall.ENOTDIR) {
		t.Fatalf("Stat through f/..: %v", err)
	}
	// A dangling link whose target ends in ".." names an existing
	// directory when it resolves; one whose parent is missing is ENOENT.
	symlink(t, ufs, "missing/..", "/d/gone")
	if _, err := ufs.Create("/d/gone"); !os.IsNotExist(err) {
		t.Fatalf("Create through missing/..: %v", err)
	}
}

func TestLinkTargetDotDotIsPhysical(t *testing.T) {
	t.Run("lower", func(t *testing.T) {
		ufs, _, lower := newTestUnion(t)
		dotDotTree(t, lower)
		checkDotDot(t, ufs, nil)
		// An absolute target with ".." above the root. basefs refuses to
		// store one, so only memfs layers check it.
		symlink(t, lower, "/../../d/target/f", "/absdots")
		if get(t, ufs, "/absdots") != "lexical" {
			t.Fatal("absolute target with .. above the root")
		}
	})
	t.Run("upper", func(t *testing.T) {
		ufs, _, _ := newTestUnion(t)
		dotDotTree(t, ufs)
		checkDotDot(t, ufs, nil)
	})
	t.Run("os", func(t *testing.T) {
		ufs, upperDir, lowerDir := osLayers(t)
		hostWrite(t, filepath.Join(lowerDir, "x", "deep", "target", "f"), "physical")
		hostWrite(t, filepath.Join(lowerDir, "d", "target", "f"), "lexical")
		must(t, os.MkdirAll(filepath.Join(lowerDir, "x", "deep", "deeper"), 0755))
		must(t, os.Symlink("../x/deep/deeper", filepath.Join(lowerDir, "d", "l2")))
		must(t, os.Symlink("l2/../target/f", filepath.Join(lowerDir, "d", "l1")))
		// The host agrees with the union.
		b, err := os.ReadFile(filepath.Join(lowerDir, "d", "l1"))
		must(t, err)
		if string(b) != "physical" {
			t.Fatalf("host read = %q", b)
		}
		checkDotDot(t, ufs, func(target, p string) {
			must(t, os.Symlink(target, filepath.Join(upperDir, filepath.FromSlash(p))))
			ufs.ClearCache()
		})
	})
}

// TestEmptyLinkTarget checks that a link with an empty target, which some
// layers can hold, resolves to nothing rather than to its directory.
func TestEmptyLinkTarget(t *testing.T) {
	ufs, _, lower := newTestUnion(t)
	mustMkdirAll(t, lower, "/d")
	if err := lower.Symlink("", "/d/empty"); err != nil {
		t.Skipf("memfs refuses an empty link target: %v", err)
	}
	if _, err := ufs.Stat("/d/empty"); !os.IsNotExist(err) {
		t.Fatalf("Stat through an empty target: %v", err)
	}
	if _, err := ufs.Create("/d/empty"); !os.IsNotExist(err) {
		t.Fatalf("Create through an empty target: %v", err)
	}
	if info, err := ufs.Lstat("/d/empty"); err != nil || !isSymlink(info) {
		t.Fatalf("Lstat of the link: %v %v", info, err)
	}
}

// TestCreateTargetGuards drives createTarget and parentFrom directly into
// the cases OpenFile's own resolve rules out unless layers change under the
// union: a chain of dangling links too long, a link that cannot be read,
// and a target naming an existing directory through "..".
func TestCreateTargetGuards(t *testing.T) {
	ufs, upper, _ := faultUnion(t)
	for i := 0; i <= maxSymlinks; i++ {
		symlink(t, upper, "l"+strconv.Itoa(i+1), "/l"+strconv.Itoa(i))
	}
	if _, _, err := ufs.createTarget("open", "/l0", true); !errors.Is(err, syscall.ELOOP) {
		t.Fatalf("long chain: %v", err)
	}
	if dir, base, err := ufs.createTarget("open", "/l40", true); err != nil || dir.path != "/" || base != "l41" {
		t.Fatalf("short chain: %v %q %v", dir, base, err)
	}

	upper.failOn("readlink", "/l40")
	_, _, err := ufs.createTarget("open", "/l40", true)
	wantInjected(t, "reading a dangling link", err)
	upper.clear()

	links := 0
	root, err := ufs.resolve("/", true)
	must(t, err)
	for _, p := range []string{"d/..", "d/.", "/", "d/sub/../"} {
		if _, _, err := ufs.parentFrom("open", root, p, &links); !os.IsExist(err) {
			t.Fatalf("parentFrom %q: %v", p, err)
		}
	}
	if _, _, err := ufs.parentFrom("open", root, "nothing/..", &links); !os.IsNotExist(err) {
		t.Fatalf("parentFrom of a missing parent: %v", err)
	}
	if _, _, err := ufs.parentFrom("open", root, "top/x", &links); !errors.Is(err, syscall.ENOTDIR) {
		t.Fatalf("parentFrom below a file: %v", err)
	}
}

// TestMkdirCopyUpFailure checks that a Mkdir whose parent cannot be copied
// up fails and leaves nothing behind.
func TestMkdirCopyUpFailure(t *testing.T) {
	ufs, upper, _ := faultUnion(t)
	upper.failOn("mkdir", "/d")
	wantInjected(t, "Mkdir", ufs.Mkdir("/d/new", 0755))
	wantInjected(t, "MkdirAll", ufs.MkdirAll("/d/sub/new", 0755))
	upper.clear()
	wantNotExist(t, ufs, "/d/new")
	wantNames(t, list(t, ufs, "/d/sub"), "c")
}
