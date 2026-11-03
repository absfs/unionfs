# unionfs

[![Go Reference](https://pkg.go.dev/badge/github.com/absfs/unionfs.svg)](https://pkg.go.dev/github.com/absfs/unionfs)
[![Go Report Card](https://goreportcard.com/badge/github.com/absfs/unionfs)](https://goreportcard.com/report/github.com/absfs/unionfs)
[![CI](https://github.com/absfs/unionfs/actions/workflows/ci.yml/badge.svg)](https://github.com/absfs/unionfs/actions/workflows/ci.yml)
[![License](https://img.shields.io/badge/license-MIT-blue.svg)](LICENSE)

A union filesystem for Go: a writable layer over any number of read-only
layers, merged into one view, with copy-on-write and whiteouts. Every layer
is an [absfs](https://github.com/absfs/absfs) filesystem (in memory, a host
directory, a remote store), and the union is one too.

```go
ufs := unionfs.New(
    unionfs.WithWritableLayer(upper), // every change lands here
    unionfs.WithReadOnlyLayer(lower), // never modified
)
```

## Why

Give many users their own writable copy of one large tree without copying
it. Reads fall through to the shared lower layer; the first write to a file
copies just that file up; a delete leaves a small marker. The upper layer
ends up holding exactly what changed, marked with AUFS-style `.wh.<name>`
whiteouts, so it can be read back as a changeset.

The motivating case is [C4](https://github.com/Avalanche-io/c4)'s
workspace prototype: one NFSv3 export, served by
[absnfs](https://github.com/absfs/absnfs), whose
[switchfs](https://github.com/absfs/switchfs) routes `/<workspace>/...` to a
per-workspace union of an empty upper over a shared, read-only checkout.
Developers mount it as a normal user and run `git`, `go build` and their
editors in it; capturing a workspace is a walk of its upper.

Other uses: container-style image layers, immutable base configuration with
per-user overrides, test fixtures with per-test scratch layers.

## Install

```sh
go get github.com/absfs/unionfs
```

Requires Go 1.21 or later.

## Quick start

```go
package main

import (
    "fmt"
    "os"

    "github.com/absfs/memfs"
    "github.com/absfs/unionfs"
)

func main() {
    base, _ := memfs.NewFS()
    base.MkdirAll("/etc", 0755)
    f, _ := base.Create("/etc/config.yml")
    f.Write([]byte("base: true\n"))
    f.Close()

    upper, _ := memfs.NewFS()
    ufs := unionfs.New(
        unionfs.WithWritableLayer(upper),
        unionfs.WithReadOnlyLayer(base),
    )

    // Reads fall through to the base.
    data, _ := ufs.ReadFile("/etc/config.yml")
    fmt.Print(string(data)) // base: true

    // Writes copy the file up first; the base is untouched.
    f2, _ := ufs.OpenFile("/etc/config.yml", os.O_WRONLY|os.O_TRUNC, 0)
    f2.Write([]byte("base: false\n"))
    f2.Close()

    // Deletes leave a whiteout in the upper: /etc/.wh.config.yml
    ufs.Remove("/etc/config.yml")
    _, err := ufs.Stat("/etc/config.yml")
    fmt.Println(os.IsNotExist(err)) // true
}
```

Over host directories, as C4's prototype composes it:

```go
host, _ := osfs.NewFS()
lower, _ := basefs.NewFS(host, "/srv/checkout") // shared, read-only
upper, _ := basefs.NewFS(host, "/srv/ws/alice") // alice's changes
ws := unionfs.New(unionfs.WithWritableLayer(upper), unionfs.WithReadOnlyLayer(lower))

var _ absfs.SymlinkFileSystem = ws // serve it with absnfs, route it with switchfs
```

More, compiled and checked by `go test`, in
[example_test.go](example_test.go) and on
[pkg.go.dev](https://pkg.go.dev/github.com/absfs/unionfs#pkg-examples).

## Features

- **absfs.SymlinkFileSystem.** `*UnionFS` implements the whole interface:
  `Lstat`, `Readlink`, `Symlink`, `Lchown`, `Chdir`/`Getwd`, `Truncate`,
  `ReadDir`, `ReadFile`, `Sub`. `FileSystem()` and `SymlinkFileSystem()`
  return views with their own working directory.
- **Per-element path resolution,** as a kernel overlay does it. A whiteout
  hides everything under it; an opaque directory hides the lower
  directory's entries at every depth; a lower non-directory under an upper
  directory is hidden.
- **Symbolic links resolved against the union.** A link in the upper can
  point at a file only the lower has, and a link in the lower can point
  into the upper. Loops fail with `ELOOP` after 40 links.
- **Copy-up on every kind of change:** opening for write, `Truncate`,
  `Chmod`, `Chown`, `Lchown`, `Chtimes`. Mode and modification time are
  kept; links are copied as links; `O_TRUNC` and `Truncate(0)` skip the
  contents; `O_CREATE` on an existing file without a write flag is a read
  and copies nothing. Copying up does not change the directories it
  passes through: their mode and modification time stay as the lower's,
  and a read-only lower ancestor (0555) does not stop a change to a file
  below it, just as on a host.
- **Deletes that leave no trace when they can.** `Remove` and `RemoveAll`
  write a whiteout only when a lower layer still has the name. A directory
  removes when its merged view is empty, and takes its whiteouts with it.
  `RemoveAll` of a lower tree writes one whiteout.
- **Failures do not resurrect deleted files.** Whiteouts are written before
  the upper entry goes and taken back if it cannot; a directory is renamed
  aside before it is deleted; a `Rename` never deletes its target before
  the move has succeeded.
- **Recreating a deleted directory makes it opaque,** so the old lower
  entries do not come back.
- **POSIX `Rename`.** Files, links and directories, from either layer, over
  an existing file or empty directory, with rename(2)'s errors. A directory
  that lower layers contribute to is copied up whole and moved, leaving a
  self-contained copy and one whiteout.
- **Changeset helpers:** `IsWhiteout`, `IsOpaqueMarker`, `WhiteoutTarget`,
  `WhiteoutName`.
- **Optional Stat cache,** with negative entries, TTLs and invalidation.
- **Safe for concurrent use,** tested under the race detector.

## The upper layer as a changeset

All changes land in the writable layer. Whiteouts use the `.wh.<name>`
naming of AUFS and OCI/Docker image layers; the opaque marker does not (see
below). The format, exactly:

| On disk in the writable layer                 | Meaning                                                        |
| --------------------------------------------- | -------------------------------------------------------------- |
| `dir/.wh.<name>`, an empty regular file       | `dir/<name>` was deleted, with everything under it             |
| `dir/.wh.__dir_opaque`, an empty regular file | `dir` replaced the lower `dir`: none of the lower entries show |
| any other file, link or directory             | created, modified, or copied up because its metadata changed   |

| `dir/.wh..wh.unionfs.<n>`, a file or directory | nothing: a leftover from a failed operation, safe to delete     |

`WhiteoutPrefix` is `".wh."` and `OpaqueWhiteout` is `".wh.__dir_opaque"`.
A whiteout and an entry of the same name never coexist in the writable
layer once an operation has finished. Marker files never show in the
merged view, and the union refuses to create a name starting with `.wh.`
(EPERM).

Names starting with `.wh..wh.` are reserved for metadata, as in AUFS and
OCI: they are hidden but delete nothing, and `WhiteoutTarget` returns false
for them. The union uses `.wh..wh.unionfs.<n>` for an entry it is about to
delete; one is left behind only when a layer fails part way through
deleting it.

**The opaque marker differs from OCI and AUFS.** Their opaque marker is
`.wh..wh..opq`; this package's is `.wh.__dir_opaque`, unchanged since
v1.0.0. An extracted OCI layer used as a unionfs layer therefore has its
opaque directories merged rather than replaced, and a tool converting
between the two must rename the marker.

So reading an upper as a changeset against its lower is a walk:

```go
var walk func(dir string)
walk = func(dir string) {
    ents, _ := upper.ReadDir(dir)
    for _, e := range ents {
        p := path.Join(dir, e.Name())
        switch deleted, isWhiteout := unionfs.WhiteoutTarget(p); {
        case unionfs.IsOpaqueMarker(p):
            replaceDir(dir) // drop every lower entry under dir; the upper's entries are the whole contents
        case isWhiteout:
            remove(deleted) // deleted, with everything under it
        case unionfs.IsWhiteout(p):
            // reserved .wh..wh. name: a leftover, not part of the changeset
        case e.IsDir():
            walk(p)         // a directory may also be new: add it if the lower lacks it
        default:
            write(p)        // added or changed
        }
    }
}
walk("/")
```

Things to know when capturing:

- An upper directory without an opaque marker merges with the lower one:
  only its entries changed. With the marker, it is the whole new contents.
- A file in the upper may be byte-identical to the lower one: it was
  copied up because its mode, owner or times changed. Compare against the
  lower to tell.
- A renamed lower file appears as a whiteout at the old name and a file
  at the new one; a renamed lower directory as a whiteout and a complete,
  marker-free copy.
- Linux overlayfs records the same facts as character devices 0/0 and the
  `trusted.overlay.opaque` xattr. unionfs neither reads nor writes that
  form; a tool reading both kinds of upper has to handle both.

## Semantics in brief

| Operation                               | What happens                                                                                                   |
| --------------------------------------- | -------------------------------------------------------------------------------------------------------------- |
| `Stat`, `Open` (read)                   | first layer with the name wins; directories merge down to a whiteout, an opaque marker or a non-directory      |
| `ReadDir`                               | merged, sorted by name, whited-out names and markers left out                                                  |
| `OpenFile` (write)                      | lower file copied up first; new file created in the upper, its whiteout removed; a directory fails with EISDIR |
| `Mkdir`, `MkdirAll`                     | in the upper; opaque when a lower layer has the (whited-out) name                                              |
| `Remove`                                | ENOTEMPTY unless the merged view is empty; whiteout only if a lower layer has the name                         |
| `RemoveAll`                             | upper copy deleted, one whiteout if a lower layer has the name; nil for a missing path                         |
| `Rename`                                | rename(2) rules; sources copied up; whiteout at the old name if a lower layer has it                           |
| `Chmod`, `Chown`, `Chtimes`, `Truncate` | follow links, copy up, change the upper copy                                                                   |
| `Lchown`                                | does not follow a final link; copies a link up as a link                                                       |
| `Symlink`                               | in the upper; the target is stored as given and resolved against the union                                     |

Errors are `*os.PathError` (or `*os.LinkError` for `Rename` and `Symlink`)
wrapping the usual `syscall` errors, so `os.IsNotExist`, `os.IsExist` and
`errors.Is(err, syscall.ENOTEMPTY)` work. Without a writable layer every
change fails with `ErrNoWritableLayer`.

## Caveats

- **Ownership is not copied up,** only mode and modification time.
- **Copy-up runs with the union's own permissions.** A lower directory
  that its owner cannot write is copied up with that mode. Changing a file
  inside or below it works, as on a host, because the union grants itself
  write permission on the copied-up directory just while it copies an
  entry up. Creating, removing or renaming an entry directly inside it
  fails with EACCES on a host filesystem, as it would on the host.
- **Special files** (devices, pipes, sockets) cannot be copied up
  (ENOTSUP). **Hard links** are not tracked across layers.
- **Renaming a large lower directory copies the whole tree** into the
  upper first.
- **Open handles do not follow copy-up:** a file opened for reading from the
  lower keeps reading the lower copy after another handle writes it.
- **Not atomic against layer failures:** a change that fails part way (a
  full disk) can leave a completed copy-up behind, never a half-written
  file, and never brings back a deleted entry. A deletion whose last step
  fails can leave a hidden `.wh..wh.unionfs.<n>` leftover, and reports the
  error.
- **Paths are normalised lexically.** A trailing slash is dropped
  (`Stat("/file/")` succeeds on a regular file) and an empty name means the
  working directory, unlike `os.Stat`.
- **The Stat cache** is dropped for paths changed through the union, by
  the path the caller gave and the path it resolved to, but not for changes
  made to a layer directly, or seen through another symbolic link, until
  the entry expires.
- **Locking:** changes take an exclusive lock for their metadata work
  (including a copy-up), lookups share a read lock; file I/O through open
  handles takes none.

See [doc.go](doc.go) for the full description and
[PERFORMANCE.md](PERFORMANCE.md) for lookup costs, caching and tuning.

## Configuration

```go
ufs := unionfs.New(
    unionfs.WithWritableLayer(upper),
    unionfs.WithReadOnlyLayer(site),     // searched first
    unionfs.WithReadOnlyLayer(defaults), // then this
    unionfs.WithStatCache(true, 5*time.Second),
    unionfs.WithCopyBufferSize(256*1024),
)
```

| Option                                              | Effect                                                            |
| --------------------------------------------------- | ----------------------------------------------------------------- |
| `WithWritableLayer(fs)`                             | the layer every change goes to; without it the union is read-only |
| `WithReadOnlyLayer(fs)`                             | adds a layer below those added so far                             |
| `WithStatCache(enabled, ttl)`                       | caches `Stat`; misses for `ttl/2`                                 |
| `WithCacheConfig(enabled, ttl, negTTL, maxEntries)` | the same, with every knob                                         |
| `WithCopyBufferSize(n)`                             | copy-up buffer, 32 KiB by default                                 |

## Testing

```sh
go test -race ./...
go test -coverprofile=cover.out . && go tool cover -func=cover.out | tail -1
```

Statement coverage is about 97% for the package and 96% for the module,
including the composition example, which a smoke test runs. The tests run the [fstesting](https://github.com/absfs/fstesting) suite
over memfs layers and over host directories (basefs over osfs), replay a
developer's session in a workspace and check the upper on disk marker by
marker, check permission handling against a host (read-only lower
directories, renames the host refuses), inject layer failures at each step
of every multi-step change, and run concurrent changes under the race
detector.

## Related

- [absfs](https://github.com/absfs/absfs): the filesystem interfaces.
- [absnfs](https://github.com/absfs/absnfs): serves an absfs filesystem over NFSv3.
- [switchfs](https://github.com/absfs/switchfs): routes paths to filesystems.
- [memfs](https://github.com/absfs/memfs), [osfs](https://github.com/absfs/osfs),
  [basefs](https://github.com/absfs/basefs): layers in memory, on the host,
  and rooted at a host directory.
- [Docker's overlay driver](https://docs.docker.com/storage/storagedriver/overlayfs-driver/),
  [AUFS](https://aufs.sourceforge.net/) and
  [overlayfs](https://www.kernel.org/doc/html/latest/filesystems/overlayfs.html),
  whose semantics this follows.

## License

MIT, see [LICENSE](LICENSE).
