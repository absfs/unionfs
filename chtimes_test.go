package unionfs

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

// changedTime is a modification time no test file has, for Chtimes calls
// meant to copy up. time.Now() can equal a just-created file's time on a
// coarse clock (Windows), and an unchanged mtime copies nothing up.
var changedTime = time.Date(2030, 1, 2, 3, 4, 5, 0, time.UTC)

// An access-time-only Chtimes of a name only a lower layer has writes
// nothing: mtime equal to the lower's, or zero. Anything else writes a
// metadata marker for a file, or copies a directory up.
func TestChtimesAccessTimeOnly(t *testing.T) {
	lowerM := time.Date(2021, 1, 2, 3, 4, 5, 6, time.UTC)
	atime := time.Date(2024, 7, 8, 9, 10, 11, 0, time.UTC)
	for _, tc := range []struct {
		name   string
		target string // path changed
		mtime  func() time.Time
		copied []string // paths the upper must have afterwards
		wantM  time.Time
		marker string // the metadata marker the upper must have, if any
	}{
		{"FileSameMtime", "/d/a", func() time.Time { return lowerM }, nil, lowerM, ""},
		{"FileSameMtimeOtherZone", "/d/a", func() time.Time { return lowerM.In(time.FixedZone("x", 3600)) }, nil, lowerM, ""},
		{"FileZeroMtime", "/d/a", func() time.Time { return time.Time{} }, nil, lowerM, ""},
		{"DirSameMtime", "/d", func() time.Time { return lowerM }, nil, lowerM, ""},
		{"DeepFileSameMtime", "/d/sub/c", func() time.Time { return lowerM }, nil, lowerM, ""},
		{"LinkTargetSameMtime", "/link", func() time.Time { return lowerM }, nil, lowerM, ""},
		{"FileMtimeOneNanosecondLater", "/d/a", func() time.Time { return lowerM.Add(time.Nanosecond) },
			[]string{"/d"}, lowerM.Add(time.Nanosecond), "/d/.wh..wh.meta.a"},
		{"FileNewMtime", "/d/a", func() time.Time { return atime },
			[]string{"/d"}, atime, "/d/.wh..wh.meta.a"},
		{"DirNewMtime", "/d", func() time.Time { return atime }, []string{"/d"}, atime, ""},
		{"LinkTargetNewMtime", "/link", func() time.Time { return atime },
			[]string{"/d"}, atime, "/d/.wh..wh.meta.a"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ufs, upper, lower := newTestUnion(t)
			lowerTree(t, lower)
			symlink(t, lower, "d/a", "/link")
			for _, p := range []string{"/d/a", "/d/sub/c", "/d"} {
				must(t, lower.Chtimes(p, lowerM, lowerM))
			}
			must(t, ufs.Chtimes(tc.target, atime, tc.mtime()))
			for _, p := range []string{"/d", "/d/a", "/d/sub", "/d/sub/c", "/link"} {
				want := false
				for _, c := range tc.copied {
					want = want || c == p
				}
				if got := exists(upper, p); got != want {
					t.Errorf("upper has %s = %v, want %v", p, got, want)
				}
			}
			info, err := ufs.Stat(tc.target)
			must(t, err)
			if !info.ModTime().Equal(tc.wantM) {
				t.Errorf("mtime %v, want %v", info.ModTime(), tc.wantM)
			}
			if s := get(t, ufs, "/d/a"); s != "la" {
				t.Errorf("contents %q", s)
			}
			if tc.marker == "" {
				wantNames(t, upperMarkers(t, upper))
			} else {
				wantNames(t, upperMarkers(t, upper), tc.marker)
			}
		})
	}
}

// A file the writable layer has gets both times, even when mtime is
// unchanged.
func TestChtimesUpperAppliesBoth(t *testing.T) {
	ufs, upper, lower := newTestUnion(t)
	lowerTree(t, lower)
	lowerM := time.Date(2021, 1, 2, 3, 4, 5, 0, time.UTC)
	must(t, lower.Chtimes("/d/a", lowerM, lowerM))
	put(t, upper, "/new", "n")
	newM := time.Date(2022, 1, 1, 0, 0, 0, 0, time.UTC)
	must(t, ufs.Chtimes("/new", newM, newM))
	if info, _ := upper.Stat("/new"); !info.ModTime().Equal(newM) {
		t.Fatalf("upper mtime %v", info.ModTime())
	}

	// A copied-up file: same mtime as now, new atime, still goes to the upper.
	f, err := ufs.OpenFile("/d/a", os.O_RDWR, 0)
	must(t, err)
	must(t, f.Close())
	must(t, ufs.Chtimes("/d/a", lowerM, newM))
	if !exists(upper, "/d/a") {
		t.Fatal("not copied up")
	}
	must(t, ufs.Chtimes("/d/a", newM, newM))
	if info, _ := ufs.Stat("/d/a"); !info.ModTime().Equal(newM) {
		t.Fatalf("mtime %v", info.ModTime())
	}

	// A zero mtime leaves the upper entry's mtime as it is, as it does a
	// lower entry's, even on a writable layer that would store the zero
	// Time itself (memfs).
	for _, p := range []string{"/new", "/d/a"} {
		must(t, ufs.Chtimes(p, time.Date(2024, 1, 1, 0, 0, 0, 0, time.UTC), time.Time{}))
		if info, _ := ufs.Stat(p); !info.ModTime().Equal(newM) {
			t.Errorf("%s: zero mtime gave %v, want %v", p, info.ModTime(), newM)
		}
		if info, _ := upper.Stat(p); !info.ModTime().Equal(newM) {
			t.Errorf("%s: upper mtime %v, want %v", p, info.ModTime(), newM)
		}
	}
}

// The no-copy-up path keeps the other Chtimes outcomes: a missing name and
// a union without a writable layer still fail.
func TestChtimesAccessTimeOnlyErrors(t *testing.T) {
	ufs, _, lower := newTestUnion(t)
	lowerTree(t, lower)
	if err := ufs.Chtimes("/missing", time.Now(), time.Time{}); !os.IsNotExist(err) {
		t.Fatalf("missing: %v", err)
	}
	info, err := lower.Stat("/d/a")
	must(t, err)
	ro := New(WithReadOnlyLayer(lower))
	if err := ro.Chtimes("/d/a", time.Now(), info.ModTime()); err != ErrNoWritableLayer {
		t.Fatalf("read-only union: %v", err)
	}
}

// Through host directories, as absnfs serves a SETATTR of the access time
// alone: Chtimes with the mtime the union reports copies nothing and leaves
// the lower file's times alone; a new mtime copies up; an upper file gets
// both times on the host.
func TestChtimesAccessTimeOnlyOsfs(t *testing.T) {
	ufs, upperDir, lowerDir := osLayers(t)
	lowerM := time.Date(2021, 1, 2, 3, 4, 5, 123456789, time.UTC)
	lowerA := time.Date(2020, 6, 1, 0, 0, 0, 0, time.UTC)
	must(t, os.MkdirAll(filepath.Join(lowerDir, "objects", "pack"), 0755))
	for _, p := range []string{"objects/pack/p.pack", "objects/ab"} {
		must(t, os.WriteFile(filepath.Join(lowerDir, p), []byte("data"), 0444))
		must(t, os.Chtimes(filepath.Join(lowerDir, p), lowerA, lowerM))
	}
	must(t, os.Symlink("objects/ab", filepath.Join(lowerDir, "HEADLINK")))
	must(t, os.Chtimes(filepath.Join(lowerDir, "objects"), lowerA, lowerM))

	read := time.Date(2024, 7, 8, 9, 10, 11, 0, time.UTC)
	for _, p := range []string{"/objects/pack/p.pack", "/objects/ab", "/objects", "/objects/pack", "/HEADLINK"} {
		info, err := ufs.Stat(p)
		must(t, err)
		if !info.IsDir() && get(t, ufs, p) != "data" {
			t.Fatalf("read %s", p)
		}
		must(t, ufs.Chtimes(p, read, info.ModTime()))
		must(t, ufs.Chtimes(p, read, time.Time{}))
	}
	wantNames(t, hostTree(t, upperDir))
	host, err := os.Stat(filepath.Join(lowerDir, "objects/pack/p.pack"))
	must(t, err)
	if !host.ModTime().Equal(lowerM) {
		t.Fatalf("lower mtime %v", host.ModTime())
	}
	// The host may record the reads above itself; the union never writes
	// the requested access time to a lower layer.
	if a, ok := hostAtime(host); ok && a.Equal(read) {
		t.Fatalf("lower atime set to %v", a)
	}
	// Through the union, a FileInfo's Sys value still reports the lower
	// file's own access time: the one set above, or one the host recorded
	// for the reads.
	for p, file := range map[string]string{
		"/objects/pack/p.pack": "objects/pack/p.pack",
		"/HEADLINK":            "objects/ab",
	} {
		info, err := ufs.Stat(p)
		must(t, err)
		a, ok := hostAtime(info)
		if !ok {
			continue
		}
		onDisk, err := os.Stat(filepath.Join(lowerDir, file))
		must(t, err)
		diskA, _ := hostAtime(onDisk)
		if a.Equal(read) || !(a.Equal(lowerA) || a.Equal(diskA)) {
			t.Errorf("%s: union Sys atime %v, want the lower's (%v or %v)", p, a, lowerA, diskA)
		}
	}

	// A new mtime writes a metadata marker and copies no data; the first
	// open for writing copies the data up with the marker's mtime.
	newM := time.Date(2025, 1, 1, 0, 0, 0, 0, time.UTC)
	must(t, ufs.Chtimes("/objects/ab", read, newM))
	wantNames(t, hostTree(t, upperDir), "/objects/", "/objects/.wh..wh.meta.ab")
	must(t, ufs.Chmod("/objects/ab", 0644)) // the lower's 0444 refuses writes
	f, err := ufs.OpenFile("/objects/ab", os.O_WRONLY|os.O_APPEND, 0)
	must(t, err)
	must(t, f.Close())
	wantNames(t, hostTree(t, upperDir), "/objects/", "/objects/ab")
	b, err := os.ReadFile(filepath.Join(upperDir, "objects/ab"))
	must(t, err)
	if string(b) != "data" {
		t.Fatalf("copied contents %q", b)
	}
	up, err := os.Stat(filepath.Join(upperDir, "objects/ab"))
	must(t, err)
	if !up.ModTime().Equal(newM) {
		t.Fatalf("upper mtime %v", up.ModTime())
	}

	// Now in the upper, an access-time change with the same mtime applies
	// both times.
	must(t, ufs.Chtimes("/objects/ab", read, newM))
	up, err = os.Stat(filepath.Join(upperDir, "objects/ab"))
	must(t, err)
	if !up.ModTime().Equal(newM) {
		t.Fatalf("upper mtime %v", up.ModTime())
	}
	if a, ok := hostAtime(up); ok && !a.Equal(read) {
		t.Fatalf("upper atime %v, want %v", a, read)
	}
	if _, ok := hostAtime(up); !ok {
		t.Log("host access time not readable here; atime checks skipped")
	}
}
