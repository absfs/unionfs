package unionfs_test

import (
	"fmt"
	"io"
	"log"
	"os"
	"path"
	"sort"

	"github.com/absfs/absfs"
	"github.com/absfs/memfs"
	"github.com/absfs/unionfs"
)

// write creates name in fs with the given contents.
func write(fs absfs.Filer, name, data string) {
	f, err := fs.OpenFile(name, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, 0644)
	if err != nil {
		log.Fatal(err)
	}
	if _, err := io.WriteString(f, data); err != nil {
		log.Fatal(err)
	}
	if err := f.Close(); err != nil {
		log.Fatal(err)
	}
}

// read returns the contents of name in fs.
func read(fs absfs.Filer, name string) string {
	b, err := fs.ReadFile(name)
	if err != nil {
		return "(" + err.Error() + ")"
	}
	return string(b)
}

func newMemFS() *memfs.FileSystem {
	fs, err := memfs.NewFS()
	if err != nil {
		log.Fatal(err)
	}
	return fs
}

// A writable layer over a read-only base: reads fall through, writes copy
// up, deletes leave whiteouts, and the base never changes.
func Example() {
	base := newMemFS()
	base.MkdirAll("/etc", 0755)
	write(base, "/etc/config.yml", "base: true\n")
	write(base, "/etc/hosts", "127.0.0.1 localhost\n")

	ufs := unionfs.New(
		unionfs.WithWritableLayer(newMemFS()),
		unionfs.WithReadOnlyLayer(base),
	)

	write(ufs, "/etc/config.yml", "base: false\n")   // copied up, then written
	write(ufs, "/etc/new.yml", "new: true\n")        // created in the upper
	if err := ufs.Remove("/etc/hosts"); err != nil { // whited out
		log.Fatal(err)
	}

	ents, _ := ufs.ReadDir("/etc")
	for _, e := range ents {
		fmt.Println(e.Name())
	}
	fmt.Print("union: ", read(ufs, "/etc/config.yml"))
	fmt.Print("base:  ", read(base, "/etc/config.yml"))
	fmt.Print("base:  ", read(base, "/etc/hosts"))
	// Output:
	// config.yml
	// new.yml
	// union: base: false
	// base:  base: true
	// base:  127.0.0.1 localhost
}

// Layers are searched in the order they were added: the first read-only
// layer added shadows the ones after it.
func Example_multipleLayers() {
	defaults, site := newMemFS(), newMemFS()
	write(defaults, "/port", "80")
	write(defaults, "/name", "default")
	write(site, "/name", "example.org")

	ufs := unionfs.New(
		unionfs.WithWritableLayer(newMemFS()),
		unionfs.WithReadOnlyLayer(site),     // searched first
		unionfs.WithReadOnlyLayer(defaults), // then this
	)
	fmt.Println(read(ufs, "/name"), read(ufs, "/port"))
	// Output: example.org 80
}

// Deleting a directory a lower layer has, then making it again, gives an
// empty, opaque directory: none of the old entries reappear.
func Example_opaqueDirectory() {
	base := newMemFS()
	base.MkdirAll("/build", 0755)
	write(base, "/build/old.o", "stale")
	upper := newMemFS()
	ufs := unionfs.New(unionfs.WithWritableLayer(upper), unionfs.WithReadOnlyLayer(base))

	if err := ufs.RemoveAll("/build"); err != nil {
		log.Fatal(err)
	}
	if err := ufs.Mkdir("/build", 0755); err != nil {
		log.Fatal(err)
	}
	ents, _ := ufs.ReadDir("/build")
	fmt.Println("entries:", len(ents))
	_, err := upper.Stat("/build/" + unionfs.OpaqueWhiteout)
	fmt.Println("opaque marker:", err == nil)
	// Output:
	// entries: 0
	// opaque marker: true
}

// The writable layer is a changeset against the layers below it. Walk it
// and classify each entry: a whiteout records a deletion, an opaque marker
// records that a directory replaced the one below, a reserved .wh..wh. name
// (a leftover from a failed operation) records nothing, and anything else
// was written (created, modified, or copied up because its mode or times
// changed).
func ExampleWhiteoutTarget() {
	base := newMemFS()
	base.MkdirAll("/src/gen", 0755)
	write(base, "/src/main.go", "package main")
	write(base, "/src/old.go", "package main")
	write(base, "/src/gen/a.go", "package gen")

	upper := newMemFS()
	ufs := unionfs.New(unionfs.WithWritableLayer(upper), unionfs.WithReadOnlyLayer(base))
	write(ufs, "/src/main.go", "package main // edited")
	ufs.Remove("/src/old.go")
	ufs.RemoveAll("/src/gen")
	ufs.Mkdir("/src/gen", 0755)
	write(ufs, "/src/gen/b.go", "package gen")
	// What a layer failure part way through a delete can leave behind.
	upper.MkdirAll("/src/.wh..wh.unionfs.1", 0755)
	write(upper, "/src/.wh..wh.unionfs.1/stale.go", "package main")

	var changes []string
	var walk func(dir string)
	walk = func(dir string) {
		ents, _ := upper.ReadDir(dir)
		for _, e := range ents {
			p := path.Join(dir, e.Name())
			switch deleted, isWhiteout := unionfs.WhiteoutTarget(p); {
			case unionfs.IsOpaqueMarker(p):
				changes = append(changes, "replace "+dir)
			case isWhiteout:
				changes = append(changes, "delete  "+deleted)
			case unionfs.IsWhiteout(p):
				// A reserved .wh..wh. name: not part of the changeset.
			case e.IsDir():
				walk(p)
			default:
				changes = append(changes, "write   "+p)
			}
		}
	}
	walk("/")
	sort.Strings(changes)
	for _, c := range changes {
		fmt.Println(c)
	}
	// Output:
	// delete  /src/old.go
	// replace /src/gen
	// write   /src/gen/b.go
	// write   /src/main.go
}

func ExampleWhiteoutName() {
	fmt.Println(unionfs.WhiteoutName("/src/old.go"))
	fmt.Println(unionfs.IsWhiteout("/src/.wh.old.go"), unionfs.IsOpaqueMarker("/src/.wh.old.go"))
	// Output:
	// /src/.wh.old.go
	// true false
}

// Each FileSystem view has its own working directory over the same union.
func ExampleUnionFS_FileSystem() {
	base := newMemFS()
	base.MkdirAll("/app", 0755)
	write(base, "/app/config.yml", "port: 80\n")
	ufs := unionfs.New(unionfs.WithWritableLayer(newMemFS()), unionfs.WithReadOnlyLayer(base))

	fs := ufs.FileSystem()
	if err := fs.Chdir("/app"); err != nil {
		log.Fatal(err)
	}
	fmt.Print(read(fs, "config.yml"))
	wd, _ := fs.Getwd()
	other, _ := ufs.FileSystem().Getwd()
	fmt.Println(wd, other)
	// Output:
	// port: 80
	// /app /
}
