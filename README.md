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
  into the upper. A target is walked element by element, as a kernel does,
  so a `..` after a linked directory leaves the directory that link
  resolves to. Loops fail with `ELOOP` after 40 links.
- **Copy-up on write, metadata markers on metadata changes.** Opening a
  lower file for write, `Truncate` and `Rename` copy it up; mode and
  modification time are kept; links are copied as links; `O_TRUNC` and
  `Truncate(0)` skip the contents; `O_CREATE` on an existing file without
  a write flag is a read and copies nothing, and so does a `Chtimes` that
  changes only a lower entry's access time. `Chmod`, `Chown`, `Lchown` and
  `Chtimes` of a lower regular file copy no data at all: they write an
  empty `.wh..wh.meta.<name>` marker whose own mode, mtime and owner are
  the file's new ones (see below). Copying up does not change the directories it
  passes through: their mode and modification time stay as the lower's,
  and a read-only lower ancestor (0555) does not stop a change to a file
  below it, just as on a host.
- **Deletes that leave no trace when they can.** `Remove` and `RemoveAll`
  write a whiteout only when a lower layer still has the name. A directory
  can be removed once its merged view is empty, and takes its whiteouts
  with it. `RemoveAll` of a lower tree writes one whiteout.
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
- **Changeset helpers:** `IsWhiteout`, `IsOpaqueMarker`, `IsMetaMarker`,
  `WhiteoutTarget`, `WhiteoutName`, `MetaTarget`, `MetaMarkerName`, and
  `ReservedNames` to find `.wh.` names in a tree before using it as a
  lower.
- **Optional caches.** A lookup cache (`WithLookupCache`) remembers which
  layer holds each name, so a repeated `Lstat` or `Stat` costs one layer
  call instead of several per path element, and never serves stale
  attributes. A Stat cache serves whole `Stat` and `Lstat` results, with
  negative entries, TTLs and invalidation.
- **Safe for concurrent use,** tested under the race detector.

## The upper layer as a changeset

All changes land in the writable layer. Whiteouts use the `.wh.<name>`
naming of AUFS and OCI/Docker image layers; the opaque marker does not (see
below). The format, exactly:

| On disk in the writable layer                    | Meaning                                                                                                                    |
| ------------------------------------------------ | -------------------------------------------------------------------------------------------------------------------------- |
| `dir/.wh.<name>`, an empty regular file          | `dir/<name>` was deleted, with everything under it                                                                         |
| `dir/.wh.__dir_opaque`, an empty regular file    | `dir` replaced the lower `dir`: none of the lower entries show                                                             |
| `dir/.wh..wh.meta.<name>`, an empty regular file | only the mode, times or owner of the lower file `dir/<name>` changed: to the marker's own                                  |
| any other file, link or directory                | created, modified, or copied up because a directory's or link's metadata changed, or because a file was opened for writing |
| `dir/.wh..wh.unionfs.<n>`, a file or directory   | nothing: a leftover from a failed operation, safe to delete                                                                |

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

### Metadata markers

`chmod +x` on a 2 GB lower file, or git "freshening" a pack by setting its
times, should not copy 2 GB. So `Chmod`, `Chown`, `Lchown` and `Chtimes`
(with a new modification time) of a regular file that only a lower layer
has write a marker instead of a copy: `dir/.wh..wh.meta.<name>`, a
zero-length regular file in the writable layer whose **own** permission
bits (all of them, so the execute bits read straight off it),
modification time and owner are the file's new metadata. There is no
`dir/<name>` in the upper. Through the union, `Stat`, `Lstat`, `ReadDir`
and an open handle's `Stat` report the lower file's size, contents and type
with the marker's mode, mtime and owner; `FileInfo.Sys()` carries the
marker's mode, owner, modification time and change time where both layers
report one (a host `*syscall.Stat_t`, a memfs `*inode.Inode`); its access
time stays the lower file's. Reads come from the lower.

- Further metadata changes update the same marker.
- The first open with a write flag (`O_WRONLY`, `O_RDWR`, `O_APPEND`,
  `O_TRUNC`, even if nothing is then written), `Truncate` or `Rename` copies
  the data up, gives the copy the marker's mode, mtime and owner, and
  removes the marker.
- `Remove` deletes the marker and writes the whiteout, as for any lower
  file.
- If the lower file changes underneath, the marker still applies to it.
- Directories and symbolic links are copied up as before: copying them
  costs no data.
- A file whose name is longer than 242 bytes is copied up as before too:
  its marker's name would pass the 255-byte limit hosts put on a name. So
  is a file whose marker the writable layer refuses as too long.
- A marker in a read-only layer applies to the layers below it, so a
  captured upper can be stacked as a lower.

A marker and an entry of the same name never coexist in the upper once an
operation has finished. `MetaTarget(".../.wh..wh.meta.x")` returns
`".../x"`; `MetaMarkerName` builds a marker name for writers.

**Reading a changeset with metadata markers.** A reader that predates
them skips every `.wh..wh.` name, as it already must, and so sees no change
for such a file: it misses the metadata change but nothing else. A reader
that knows them reports a metadata-only change to `MetaTarget(marker)`,
taking the new mode, mtime and owner from the marker itself.

**Names starting with `.wh.` in a read-only layer.** By default markers
count in every layer, as in AUFS and OCI image layers, so an upper
captured from one union can be stacked as a read-only layer of another
and keep its deletions. The price: a read-only layer's own file or
directory whose name starts with `.wh.` (a container-image test fixture,
say) is taken for a marker and silently missing from the merged view, and
a `.wh.<x>` among them hides `<x>` in the layers below. The union refuses
to create such names, so only a lower can have one. Two ways out:

- `unionfs.ReservedNames(fs, root)` lists every such name under `root` of
  any absfs filesystem, without following links, so a consumer can detect
  a lower it cannot represent before using it (and fall back, or refuse).
- `WithMarkersOnlyInWritableLayer()` reads markers only in the writable
  layer. In read-only layers `.wh.` names are ordinary entries: they show,
  can be read, listed and looked up, but cannot be changed, removed,
  renamed or created under (EPERM), because the writable layer has no way
  to represent them. Renaming a directory that holds them, at any depth,
  fails with EPERM too, since its copied-up tree could not hold them; such
  a directory can be removed only with `RemoveAll`, since `Remove` cannot
  empty it. The cost
  is that read-only layers can no longer carry whiteouts, opaque markers
  or metadata markers, so captured uppers cannot be stacked.

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
        deleted, isWhiteout := unionfs.WhiteoutTarget(p)
        changed, isMeta := unionfs.MetaTarget(p)
        switch {
        case unionfs.IsOpaqueMarker(p):
            replaceDir(dir) // drop every lower entry under dir; the upper's entries are the whole contents
        case isWhiteout:
            remove(deleted) // deleted, with everything under it
        case isMeta:
            info, _ := e.Info()
            setMeta(changed, info) // contents unchanged; mode, mtime and owner are the marker's own
        case unionfs.IsWhiteout(p):
            // other reserved .wh..wh. name: a leftover, not part of the changeset
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
- A regular file whose mode, owner or times alone changed is a metadata
  marker, not a copy. A file in the upper may still be byte-identical to
  the lower one: opened for writing without a write, say. Compare against
  the lower to tell.
- A renamed lower file appears as a whiteout at the old name and a file
  at the new one; a renamed lower directory as a whiteout and a complete
  copy with no markers inside it, except an opaque marker at its top when
  a lower layer also had the new name.
- Linux overlayfs records the same facts as character devices 0/0 and the
  `trusted.overlay.opaque` xattr. unionfs neither reads nor writes that
  form; a tool reading both kinds of upper has to handle both.

## Semantics in brief

| Operation                   | What happens                                                                                                                                                        |
| --------------------------- | ------------------------------------------------------------------------------------------------------------------------------------------------------------------- |
| `Stat`, `Open` (read)       | first layer with the name wins; directories merge down to a whiteout, an opaque marker or a non-directory                                                           |
| `ReadDir`                   | merged, sorted by name, whited-out names and markers left out                                                                                                       |
| `OpenFile` (write)          | lower file copied up first (with a metadata marker's changes, which it removes); new file created in the upper, its whiteout removed; a directory fails with EISDIR |
| `Mkdir`, `MkdirAll`         | in the upper; opaque when a lower layer has the (whited-out) name                                                                                                   |
| `Remove`                    | ENOTEMPTY unless the merged view is empty; whiteout only if a lower layer has the name                                                                              |
| `RemoveAll`                 | upper copy deleted, one whiteout if a lower layer has the name; nil for a missing path                                                                              |
| `Rename`                    | rename(2) rules; sources copied up; whiteout at the old name if a lower layer has it                                                                                |
| `Chmod`, `Chown`, `Chtimes` | follow links; a lower regular file gets a metadata marker, anything else is copied up; `Chtimes` of the access time alone writes nothing (below)                    |
| `Truncate`                  | follows links, copies up, truncates the upper copy                                                                                                                  |
| `Lchown`                    | does not follow a final link; a lower regular file gets a metadata marker, a link is copied up as a link                                                            |
| `Symlink`                   | in the upper; the target is stored as given and resolved against the union                                                                                          |

Errors are `*os.PathError` (or `*os.LinkError` for `Rename` and `Symlink`)
wrapping the usual `syscall` errors, so `os.IsNotExist`, `os.IsExist` and
`errors.Is(err, syscall.ENOTEMPTY)` work. Without a writable layer every
change fails with `ErrNoWritableLayer`.

## Caveats

- **Ownership is not copied up,** only mode and modification time. A
  metadata marker starts with the lower file's owner, and a copy made
  from a marker gets the marker's owner, only where the union may chown
  to it (as root, or to the union's own user and groups).
- **Access-time-only updates of lower entries are dropped,** not copied
  up. `Chtimes` of a name only lower layers have (a file, a link's target
  or a directory) writes nothing and returns nil when `mtime` is the zero
  `Time` (which `os.Chtimes` takes as "leave unchanged") or equals the
  lower entry's current modification time. `os.FileInfo` has no access
  time, so the change could not be seen through the union anyway; a host
  layer's `FileInfo.Sys()` keeps reporting the lower file's own access
  time. Any other `mtime`, even one nanosecond off, writes a metadata
  marker for a file (and copies a directory up), and an entry already in
  the writable layer gets both times, except that
  a zero `mtime` leaves its modification time alone there too. This matters
  served over NFS: the macOS client (NFSv3 and NFSv4) sends a SETATTR of
  the access time after a program reads a file through `mmap`, and absnfs
  turns it into `Chtimes` with the current `mtime`; before, every git
  object or pack that `git log` or `git status` read was copied up.
  Mounting with `noatime` still saves those round trips, but is no longer
  needed to avoid the copies.
- **Copy-up runs with the union's own permissions.** A lower directory
  that its owner cannot write is copied up with that mode. Changing a file
  inside or below it works, as on a host, because the union grants itself
  write permission on the copied-up directory just while it copies an
  entry up. Creating, removing or renaming an entry directly inside it
  fails with EACCES on a host filesystem, as it would on the host.
- **The union's bookkeeping never follows a link in the writable layer.**
  A host follows a symbolic link on open, chmod, chown and chtimes, so a
  link where the union writes a marker would carry the write to the
  link's target. An entry already at a whiteout's or opaque marker's path
  counts as that marker and is not opened; a link at a metadata marker's
  path is not a marker, and changing the file's metadata fails with
  `EEXIST`; copy-up creates its file exclusively; and a directory the
  union expected in the writable layer that is now something else fails
  with `ENOTDIR`. `Lchown` of a link fails with `errors.ErrUnsupported`
  when the writable layer has no `Lchown`, rather than following it.
- **Hosts that fold case** (macOS and Windows by default) take `.WH.x`,
  which the union lets a client create, for the marker `.wh.x`: such an
  entry hides `x`, or blocks metadata changes to it, as above.
- **A lower's own `.wh.` names vanish** by default, taken for markers.
  Check a lower with `ReservedNames`, or use
  `WithMarkersOnlyInWritableLayer` (see the changeset section).
- **Special files** (devices, pipes, sockets) cannot be copied up
  (ENOTSUP). **Hard links** are not tracked across layers.
- **Renaming a large lower directory copies the whole tree** into the
  upper first.
- **Open handles do not follow copy-up:** a file opened for reading from the
  lower keeps reading the lower copy after another handle writes it.
- **Not atomic against layer failures:** a change that fails part way (a
  full disk) can leave a completed copy-up behind, never a half-written
  file, and never brings back a deleted entry. A `Remove`, `RemoveAll` or
  `Rename` whose last step (deleting the old upper copy) fails has already
  taken effect in the merged view; it leaves a hidden
  `.wh..wh.unionfs.<n>` leftover and reports the error.
- **The caller's path is normalised lexically.** A trailing slash is
  dropped (`Stat("/file/")` succeeds on a regular file) and an empty name
  means the working directory, unlike `os.Stat`. The destructive calls
  follow the os package instead: `Remove("")` and `Rename` with an empty
  name fail with `ENOENT`, `RemoveAll("")` does nothing, and a name ending
  in `.` or `..` fails with `EINVAL` in all three. Link targets are not
  cleaned (see Features).
- **The lookup cache** (`WithLookupCache`) holds which layer has each name
  and which markers apply, never attributes, so it stays exact under
  writes through open handles. Changes through the union drop what they
  affect. Changes made to a layer directly show only once the entries
  expire, or after `InvalidateCache`, `InvalidateCacheTree` or
  `ClearCache` of the topmost path the change created or removed: until
  then a name added to a layer may stay missing, or stay hidden by a layer
  above. A name removed from its layer is noticed at once.
- **The Stat cache** holds `Stat` and `Lstat` results. It is dropped for
  paths changed through the union, by the path the caller gave and the
  path it resolved to. It is not dropped for changes made to a layer
  directly, for writes through an open handle (`OpenFile` drops the entry
  when it opens the file, so a `Stat` taken between the open and a
  `Write` keeps the old size), or for changes seen through another
  symbolic link, until the entry expires. Call `InvalidateCache` after
  such a change, or leave the Stat cache off (the default) when files are
  written through long-lived handles, as an NFS server does, and use the
  lookup cache instead.
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
    unionfs.WithLookupCache(time.Hour, 0),
    unionfs.WithStatCache(true, 5*time.Second),
    unionfs.WithCopyBufferSize(256*1024),
)
```

| Option                                              | Effect                                                            |
| --------------------------------------------------- | ----------------------------------------------------------------- |
| `WithWritableLayer(fs)`                             | the layer every change goes to; without it the union is read-only |
| `WithReadOnlyLayer(fs)`                             | adds a layer below those added so far                             |
| `WithLookupCache(ttl, maxEntries)`                  | caches path resolution, not attributes; off unless `ttl > 0`      |
| `WithStatCache(enabled, ttl)`                       | caches `Stat` and `Lstat`; misses for `ttl/2`                     |
| `WithCacheConfig(enabled, ttl, negTTL, maxEntries)` | the same, with every knob                                         |
| `WithMarkersOnlyInWritableLayer()`                  | `.wh.` names in read-only layers are plain entries, not markers   |
| `WithCopyBufferSize(n)`                             | copy-up buffer, 32 KiB by default                                 |

## Testing

```sh
go test -race ./...
go test -coverprofile=cover.out . && go tool cover -func=cover.out | tail -1
```

Statement coverage is about 98% for the package and the module,
including the composition example, which a smoke test runs. The tests run
the [fstesting](https://github.com/absfs/fstesting) suite over memfs
layers and over host directories (basefs over osfs), replay a developer's
session in a workspace and check the upper on disk marker by marker, check
permission handling against a host (read-only lower directories, renames
the host refuses), inject layer failures at each step of every multi-step
change, and run concurrent changes under the race detector.

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
