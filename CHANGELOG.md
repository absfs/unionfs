# Changelog

All notable changes to the UnionFS project will be documented in this file.

## [Unreleased]

Fixes found running a union per workspace under C4's NFS workspace
prototype, and an audit of the same kind of bug across the package.
Intended as v1.1.0: the API only grows, but several behaviours change to
match POSIX, and callers relying on the old ones should read
[Changed](#changed) first.

### Security

These affect a union whose writable layer is a host directory (basefs over
osfs) that clients change through the union. basefs v1.0.0 lets a client
make a symbolic link whose relative target stays inside the layer where it
is made and climbs out of it once the link is renamed one level up; a host
follows such a link wherever the union hands it a path through it.

- The union's own bookkeeping acted through a link in the writable layer.
  `Remove` and `Rename`, of files and of directories, wrote their
  whiteout or opaque marker by opening the marker's path with `O_TRUNC`,
  so a link there truncated its target, outside the layer. `Chmod`,
  `Chown` and `Chtimes` of a file only a lower layer has took a link at
  its metadata marker's path for the marker and changed the link's
  target's mode, owner or times. `Lchown` of a link, on a writable layer without
  `Lchown`, fell back to `Chown` and changed the link's target's owner. On
  a host that folds case (macOS, and Windows by default) a client could
  plant such a link through the union itself, as `.WH.<name>` or
  `.WH..wh.meta.<name>`, which the host takes for the marker. Now an entry
  already at a whiteout's or opaque marker's path counts as that marker
  and is never opened; markers and copy-ups are created with `O_EXCL`;
  only a regular file is a metadata marker (anything else there makes the
  change fail with `EEXIST`); a directory the union expected in the
  writable layer that is something else fails with `ENOTDIR`; and `Lchown`
  of a link fails with `errors.ErrUnsupported` when the layer has no
  `Lchown`.
- The union let clients create names that a host folding case takes for
  its markers: `.WH.x` hid a lower `x`, `.WH.__dir_opaque` made a merged
  directory opaque, a regular `.WH..wh.meta.x` became `x`'s metadata, and
  creating `x` deleted the client's `.WH.x`. Creating, renaming onto or
  making a directory at a name starting with `.wh.` in any ASCII case now
  fails with `EPERM`, on every host.
- With `WithLookupCache`, the cache keyed names by the union's spelling,
  but a host can take two spellings for one name: `d` and `D` where it
  folds case, and the composed and decomposed forms of `é` on macOS, even
  on a case-sensitive volume. A client could remove a cached directory
  through one spelling and put an escaping link at the other; the cached
  directory was then walked through without being checked again, so
  `OpenFile` with `O_TRUNC`, `Chmod` and `Create` under it reached a file
  or directory outside the layer. Creating a link in the writable layer,
  or renaming anything there but a regular file, now empties the lookup
  cache. The lookup cache is new in this release, so no released version
  has this one.
- On a host that folds case, the union's existing markers were reachable
  through another spelling of their names: lookups decided that a name was
  reserved by its lowercase `.wh.` prefix only, and the host took `.WH.x`
  for the whiteout `.wh.x`. `Remove("/.WH.x")` deleted the whiteout and
  brought a deleted lower `x` back, `Rename` of `.WH.<x>` did the same,
  `Remove` of `.WH.__DIR_OPAQUE` exposed the lower entries of an opaque
  directory, and `Chmod` of `.WH..WH.META.<x>` changed `x`'s mode. A name
  starting with `.wh.` in any ASCII case is now reserved wherever markers
  count: it is missing from the merged view, and nothing can be read,
  changed, renamed or removed through it. Under
  `WithMarkersOnlyInWritableLayer` a read-only layer's such names stay
  ordinary read-only entries. Only the lowercase prefix is recognised as a
  marker, as before; the changeset format is unchanged.
- With `WithLookupCache` on a host that folds case, a write through a
  stale spelling made a hidden view permanent. After `Stat("/d/x")` of a
  lower file, `RemoveAll("/D")` and `Mkdir("/D")` made `/D` opaque, but
  the cached `/d` still showed the lower directory, and `OpenFile("/d/x",
  O_APPEND)` copied the hidden lower file up into the opaque directory,
  where it showed under every spelling for good. The same held for
  `Truncate`, `Rename`, `Chmod` and `Chtimes` through `/d`, for files
  created in it after `/D` was whited out (which brought the directory and
  its lower entries back), and for renaming a cached directory. Every
  copy-up, and every new metadata marker, now checks a node from the
  lookup cache against the layers first; when the merged view has changed
  there, the cache is emptied and the change fails, with `ENOENT` when the
  name is gone and `ESTALE` otherwise, so a retry acts on the current view.
- On Windows, where 8.3 short names are on (the default on system
  volumes), NTFS gives `.wh.x` a second name such as `WH~1.X` or
  `WHE0B6~1.X`, and the union's markers were reachable through it:
  `Remove`, `Rename` of or onto it, and `OpenFile` with `O_CREATE` deleted
  or replaced the whiteout and brought the lower `x` back, and `Remove`
  of the opaque marker's short name exposed a directory's lower entries.
  A name with the shape of a short name that a layer whose markers count
  resolves but does not list under that name now fails with `EPERM`, for
  every operation and whatever entry it aliases; an entry whose own name
  has that shape is unaffected.

The case-folding attacks are tested on macOS, and the short-name ones on a
Windows host directory; the rest of the Windows code paths have no
host-directory test of their own.

### Fixed

- `Remove` wrote a whiteout for every removed file, including ones only the
  writable layer had. It now writes one only when a lower layer still has
  the name.
- Removing a directory failed with "directory not empty" because of the
  whiteouts left inside its upper copy (so `rm -r` of any lower directory
  failed). `Remove` now refuses a directory whose merged view is not empty
  (`ENOTEMPTY`) and removes the whiteouts of one that is.
- A whited-out or opaque directory hid only its direct children: a
  deeper path (`/d/sub/f` after `RemoveAll("/d")`) still resolved from the
  lower layer. Paths are now resolved one element at a time, so a whiteout
  hides everything under it and an opaque directory hides the lower
  directory at every depth. A lower non-directory under an upper directory
  of the same name is hidden too.
- `Mkdir` and `MkdirAll` over a whited-out directory brought the old lower
  entries back. The new directory is now opaque.
- `Rename` of a lower directory copied up only the empty directory, losing
  its contents; renaming over a file the writable layer had failed on
  layers that do not replace on rename (memfs); there were no rename(2)
  checks. `Rename` now copies up a
  directory's whole merged tree before moving it, makes the moved directory
  opaque when a lower layer has the new name, replaces files and empty
  directories, and fails with `EISDIR`, `ENOTDIR`, `ENOTEMPTY`, `EINVAL`
  and `EBUSY` as rename(2) does.
- `RemoveAll` of a directory both layers had deleted only the upper copy,
  and the lower one showed through again. It now deletes the upper copy and
  writes one whiteout whenever a lower layer has the name.
- Symbolic links were followed inside a single layer, so an upper link to
  a lower-only file did not resolve; they are now resolved against the
  union. A target is walked element by element rather than cleaned as
  text, so a `..` after a linked directory leaves the directory that link
  resolves to, as on a host. Copy-up of a lower link (by `Rename` or
  `Lchown`) turned it into a regular file holding its target's contents;
  links are now copied as links.
- `Chmod` on a directory dropped its directory bit on layers that store the
  mode verbatim (memfs). Copy-up kept no mode on directories created for a
  parent path; parents are now copied up with their mode and times, and
  copying an entry up into a directory no longer changes that directory's
  modification time, so a metadata-only change to a lower file does not
  make its directory look modified in a changeset.
- Copying up below a read-only lower directory (0555) works, as the change
  does on a host: the union grants itself write permission on the
  copied-up directory just while it copies the entry in, then restores the
  lower mode. The same applies to the union's own markers: `Mkdir` with a
  mode its owner cannot write (0555) over a deleted lower directory still
  writes the opaque marker, and a `Rename` that replaces an emptied
  read-only directory deletes the replaced copy, whiteouts and all.
- A failed `Rename` could delete its target: when the move failed for any
  reason and the target was in the writable layer, the target was removed
  and the move retried. The target is now renamed aside and put back if
  the retry fails too.
- A failure part way through `Remove`, `RemoveAll` or `Rename` could bring
  deleted lower entries back (the markers inside a directory were deleted
  before the directory, and the whiteout for a modified lower file written
  after its upper copy was gone). Whiteouts are now written first and
  taken back on failure, a directory is renamed aside before it is deleted,
  and a moved directory's markers are removed only after the move.
- `Mkdir` and `MkdirAll` through a symbolic link to the parent left a
  cached miss for the caller's path in place.
- Opening a file with `O_TRUNC` copied its contents up only to discard
  them; `Truncate(0)` likewise. Neither reads the lower copy now.
- Relative names were always taken from `/`; views returned by
  `FileSystem()` lost their working directory for `RemoveAll`, `Truncate`,
  `ReadDir`, `ReadFile` and `Sub`.
- `Chtimes` copied a lower file up to change only its access time, which
  `os.FileInfo` cannot show. The macOS NFS client sends such a SETATTR
  after a program reads a file through `mmap`, so served through absnfs,
  `git status` and `git log` on a packed clone copied 3.7 MB of objects
  and packs up. `Chtimes` of a name only lower layers have now copies
  nothing and returns nil when `mtime` is zero or equals the lower entry's
  current modification time. Any other `mtime` copies up as before, and an
  entry the writable layer has still gets both times.
- A full Stat cache scanned every entry to find the oldest on each insert,
  so a working set larger than its bound (1000 with `WithStatCache`) made
  each lookup slower than no cache: 18.7 µs instead of 7.4 µs for an
  `Lstat` cycling over 5000 host files. It now evicts the oldest entry in
  constant time, and storing a path it already holds evicts nothing.
- `Chtimes` with a zero `mtime` on an entry the writable layer has passed
  the zero `Time` on, and a memfs writable layer stored it, giving the
  file a modification time in 1754. A zero `mtime` now leaves the
  modification time as it is, as `os.Chtimes` does.
- The union's bookkeeping could act through a symbolic link in the
  writable layer, and the lookup cache could walk through a directory
  that had become one; see [Security](#security).
- On a host that takes two spellings for one name (`d` and `D` where it
  folds case, the composed and decomposed `é` on macOS), the union spelled
  its markers and copy-ups as the caller did. `RemoveAll("/D")` of a lower
  `d` wrote `.wh.D`: lookups hid `d`, since the host folded the names, but
  `ReadDir` still listed it, and a changeset reader matching markers byte
  for byte, as the README tells it to, recorded the deletion of `D` while
  `d` survived. `Chmod("/D")` wrote `.wh..wh.meta.D`, a write through
  `/D` copied the file up as `D`, and `Rename("/D", "/e")` whited out
  `D`. Whiteouts, metadata markers and copy-ups for an entry a lower
  layer has are now spelled as that layer stores it; renaming onto an
  entry only lower layers have keeps its spelling, as a host does; a new
  name keeps the caller's, and creating one over a deleted lower name of
  another spelling keeps that name's whiteout rather than removing it.
  `ReadDir` now agrees with lookups where a marker or an entry in a layer
  above spells a lower name differently (written by earlier builds, or
  two read-only layers that disagree). Renaming a directory to another
  spelling of its own name failed with `ENOTEMPTY`; it now changes the
  spelling.

### Added

- `*UnionFS` implements `absfs.SymlinkFileSystem` directly, adding
  `Chdir`, `Getwd`, `TempDir` and `Truncate`.
- `IsWhiteout`, `IsOpaqueMarker`, `WhiteoutTarget` and `WhiteoutName`, to
  recognise marker files and map a whiteout to the name it deletes. Names
  starting with `.wh..wh.` are reserved metadata, as in AUFS and OCI:
  `WhiteoutTarget` returns false for them.
- Metadata markers: `Chmod`, `Chown`, `Lchown` and `Chtimes` (with a new
  modification time) of a regular file only lower layers have copy no
  data. They create or update an empty `.wh..wh.meta.<name>` file beside
  it in the writable layer whose own permission bits, modification time
  and owner are the file's; `Stat`, `Lstat`, `ReadDir` and open handles
  report them, and `FileInfo.Sys()` carries the owner, mode and
  modification and change times on host and memfs layers. A file whose
  name is longer than 242 bytes is copied up as before, since its marker's
  name would pass the hosts' 255-byte limit. The first open for writing, `Truncate` or `Rename` copies the
  data up with that metadata and removes the marker; `Remove` deletes it
  with the whiteout. A `chmod +x` of a large file, or git freshening a
  lower pack with its times, no longer copies it. `MetaPrefix`,
  `IsMetaMarker`, `MetaTarget` and `MetaMarkerName` name the markers; a
  changeset reader that skips `.wh..wh.` names, as before, sees no change
  for such a file.
- `WithLookupCache(ttl, maxEntries)` caches path resolution (which layer
  holds each name, which markers apply, which names are missing) but not
  attributes, so it stays exact under writes through open handles. A
  repeated `Lstat` costs one layer call instead of one or more per path
  element: through host directories, 1.6 to 1.8 µs instead of 6.3 to
  9.3 µs. Every change through the union drops the entries it affects;
  `CacheStats` reports its size. See PERFORMANCE.md.
- The Stat cache (`WithStatCache`, `WithCacheConfig`) holds `Lstat`
  results and misses too; before, it covered only `Stat`.
- `BenchmarkLstat`, `BenchmarkLstatWorkingSet`, and a test that counts the
  layer calls of an `Lstat`.
- `ReservedNames(fs, root)` lists the names starting with `.wh.` in a
  tree, which a union takes for markers in any layer, so a consumer can
  detect a lower it cannot represent.
- `WithMarkersOnlyInWritableLayer()` reads markers only in the writable
  layer: a read-only layer's `.wh.` names are ordinary, read-only entries
  instead of vanishing from the merged view, at the cost of read-only
  layers carrying no deletions. The default, markers in every layer, is
  unchanged.
- The README documents the whiteout format exactly and how to read a
  writable layer as a changeset.
- Tests over host directories (basefs over osfs), fault injection, a
  concurrent workload under the race detector, and runnable examples.
  Statement coverage of the package is 98%.

### Changed

These may affect existing callers:

- `Create`, `OpenFile` with `O_CREATE`, and `Mkdir` with a missing parent
  directory fail with `ENOENT`, as on any other filesystem; v1.0.0 created
  the parents silently. Use `MkdirAll` first.
- `RemoveAll` of a missing path returns nil, like `os.RemoveAll`.
- `Remove`, `RemoveAll` and `Rename` treat an empty name and a name ending
  in `.` or `..` as the os functions do, instead of acting on the working
  directory or its parent: `RemoveAll("")` returns nil and does nothing,
  `Remove("")` and `Rename` with an empty name fail with `ENOENT`, and a
  name ending in `.` or `..` fails with `EINVAL`.
- `ReadDir` sorts by name (byte order), as `absfs.Filer` specifies; v1.0.0
  sorted case-insensitively.
- Layer errors other than "not found" are reported. v1.0.0 read a failing
  layer as a missing file in listings and whiteout checks, which could show
  a lower file as deleted or a deleted one as present.
- Names starting with `.wh.` are refused (`EPERM`) and never resolve, so a
  marker file can no longer be read, listed or created as a regular file.
  Creating a name starting with `.wh.` in any other ASCII case (`.WH.x`)
  is refused too (see [Security](#security)), and an existing one does not
  resolve either: a host that folds case would take it for the union's
  marker. By default the same holds for such names in a read-only layer,
  and `ReservedNames` reports them; under
  `WithMarkersOnlyInWritableLayer` a read-only layer's are ordinary
  entries. Such a name already in the writable layer (v1.0.0 allowed
  creating one) is no longer reachable on any host, and is deleted with
  the union's markers when its directory is removed, or renamed while
  lower layers contribute to it: the moved copy does not keep it, since a
  host that folds case would take it for a marker of the new directory.
  Copy out any such file to keep before upgrading.
- Opening a directory for writing fails with `EISDIR`; `Readlink` of a
  non-link fails with `EINVAL` (memfs returned an empty target).
- `OpenFile` of an existing file with `O_CREATE` but no write flag is a
  read: it no longer copies the file up.
- A zero TTL in `WithStatCache` or `WithCacheConfig` never serves a cached
  entry; before, one could be served within the same clock tick.
- A metadata change to a lower regular file leaves a
  `.wh..wh.meta.<name>` marker in the writable layer instead of a copy of
  the file (see Added). A tool that reads the upper as a changeset and
  wants those changes must read the markers.

Internal:

- Operations that change the union hold an exclusive lock for their
  metadata work, so concurrent changes no longer interleave half-done
  copy-ups; lookups share a read lock.
- The broken stand-alone examples (written for afero) are replaced by
  `Example` functions that `go test` runs.

### Unchanged

- The on-disk format of deletions: `.wh.<name>` whiteout files and the
  `.wh.__dir_opaque` marker, both empty regular files. The opaque marker is
  not the `.wh..wh..opq` that AUFS and OCI image layers use.

## [1.0.0] - 2025-12-12

The notes below describe the initial release as it was planned; parts of
them (afero compatibility, the examples directory) predate the move to
absfs interfaces.

### Phase 1-6 Complete - Initial Production Release

#### Added - Core Functionality (Phases 1-4)

**Core Layer Management (Phase 1)**
- `Layer` type with read-only/writable designation
- `UnionFS` struct with layer stack management
- Top-to-bottom layer precedence for file resolution
- Thread-safe operations with RWMutex
- Functional options pattern for configuration

**Whiteout Support (Phase 2)**
- AUFS-style whiteout markers (`.wh.` prefix)
- Directory opaque markers (`.wh.__dir_opaque`)
- Automatic whiteout creation on file deletion
- Whiteout detection during path resolution
- Directory listing with whiteout filtering

**Copy-on-Write Operations (Phase 3)**
- Automatic file copying from read-only to writable layer
- Metadata preservation (permissions, timestamps)
- Configurable copy buffer size
- Support for files and directories
- Efficient streaming for large files

**Directory Operations (Phase 4)**
- Multi-layer directory merging
- Deduplication of entries across layers
- Sorted directory listings
- Support for nested directory creation (MkdirAll)
- Recursive directory removal (RemoveAll)

#### Added - Advanced Features (Phase 5)

**Symlink Handling**
- `Readlink()` for reading symbolic link targets
- `Symlink()` for creating symbolic links
- `LstatIfPossible()` for querying without following links
- Cross-layer symlink resolution
- Loop detection (max depth: 40 levels)
- Support for relative and absolute symlinks

**Performance Optimizations**
- Stat cache with configurable TTL (9x faster lookups)
- Negative cache for non-existent paths (7x faster)
- Automatic cache invalidation on write operations
- LRU eviction policy
- Cache statistics API
- Manual cache management methods

**Configuration Options**
- `WithStatCache()` - Enable caching with simple config
- `WithCacheConfig()` - Fine-grained cache control
- `WithCopyBufferSize()` - Adjust CoW buffer size
- Cache management: `InvalidateCache()`, `ClearCache()`, `CacheStats()`

#### Added - Testing & Documentation (Phase 6)

**Unit Tests**
- 25 comprehensive test cases
- 57.2% code coverage
- Tests for all major operations
- Edge case coverage
- Concurrent access tests

**Performance Benchmarks**
- 13 benchmark suites
- Stat operations with/without cache
- Copy-on-write performance
- Directory merging scalability
- Layer depth impact analysis
- Whiteout lookup performance

**Integration Tests**
- Complex multi-layer scenarios
- Docker-like layer hierarchies
- Cache invalidation verification
- Concurrent access patterns
- Metadata operations with CoW
- Cross-layer rename operations

**Documentation**
- Comprehensive package documentation (doc.go)
- Performance tuning guide (PERFORMANCE.md)
- Usage examples (examples/ directory)
- API reference with examples
- Best practices and anti-patterns

#### API

**Core Types**
```go
type UnionFS struct { ... }
type Layer struct { ... }
type Cache struct { ... }
```

**Main Functions**
```go
func New(opts ...Option) *UnionFS
func WithWritableLayer(fs afero.Fs) Option
func WithReadOnlyLayer(fs afero.Fs) Option
func WithStatCache(enabled bool, ttl time.Duration) Option
func WithCacheConfig(enabled bool, statTTL, negativeTTL time.Duration, maxEntries int) Option
func WithCopyBufferSize(size int) Option
```

**afero.Fs Interface Methods**
```go
func (ufs *UnionFS) Create(name string) (afero.File, error)
func (ufs *UnionFS) Mkdir(name string, perm os.FileMode) error
func (ufs *UnionFS) MkdirAll(name string, perm os.FileMode) error
func (ufs *UnionFS) Open(name string) (afero.File, error)
func (ufs *UnionFS) OpenFile(name string, flag int, perm os.FileMode) (afero.File, error)
func (ufs *UnionFS) Remove(name string) error
func (ufs *UnionFS) RemoveAll(name string) error
func (ufs *UnionFS) Rename(oldname, newname string) error
func (ufs *UnionFS) Stat(name string) (os.FileInfo, error)
func (ufs *UnionFS) Chmod(name string, mode os.FileMode) error
func (ufs *UnionFS) Chown(name string, uid, gid int) error
func (ufs *UnionFS) Chtimes(name string, atime, mtime time.Time) error
```

**Symlink Support**
```go
func (ufs *UnionFS) Symlink(oldname, newname string) error
func (ufs *UnionFS) Readlink(name string) (string, error)
func (ufs *UnionFS) LstatIfPossible(name string) (os.FileInfo, bool, error)
```

**Cache Management**
```go
func (ufs *UnionFS) InvalidateCache(path string)
func (ufs *UnionFS) InvalidateCacheTree(pathPrefix string)
func (ufs *UnionFS) ClearCache()
func (ufs *UnionFS) CacheStats() CacheStats
```

#### Files

**Source Code** (10 files, ~3,000 lines)
- `unionfs.go` - Core types and layer management (265 lines)
- `file_ops.go` - afero.Fs interface implementation (375 lines)
- `copyup.go` - Copy-on-write functionality (133 lines)
- `dir.go` - Directory merging (227 lines)
- `cache.go` - Performance caching (209 lines)
- `symlink.go` - Symbolic link support (156 lines)
- `doc.go` - Package documentation (158 lines)

**Tests** (3 files, ~1,300 lines)
- `unionfs_test.go` - Unit tests (536 lines)
- `integration_test.go` - Integration tests (447 lines)
- `benchmark_test.go` - Performance benchmarks (269 lines)

**Documentation**
- `README.md` - Project overview and implementation plan
- `PERFORMANCE.md` - Performance tuning guide
- `CHANGELOG.md` - This file

**Examples** (3 examples)
- `examples/basic/` - Simple usage demonstration
- `examples/multi-layer/` - Docker-style multi-layer setup
- `examples/testing/` - Testing with fixtures

#### Performance

Benchmark results on Intel Xeon @ 2.60GHz:

| Operation                   | Without Cache | With Cache | Improvement |
| --------------------------- | ------------- | ---------- | ----------- |
| Stat                        | 975 ns/op     | 104 ns/op  | **9.4x**    |
| Negative Lookup             | 1,091 ns/op   | 162 ns/op  | **6.7x**    |
| Read File (1KB)             | 1,790 ns/op   | -          | -           |
| Write File (1KB)            | 3,700 ns/op   | -          | -           |
| Copy-on-Write (10KB)        | 26,000 ns/op  | -          | -           |
| Directory Merge (150 files) | 130,000 ns/op | -          | -           |

Layer depth scaling:
- 2 layers: 1,632 ns/op (baseline)
- 5 layers: 3,980 ns/op (2.4x slower)
- 10 layers: 10,268 ns/op (6.3x slower)

#### Implementation Status

✅ **Phase 1**: Core Layer Management - Complete
✅ **Phase 2**: Whiteout Support - Complete
✅ **Phase 3**: Copy-on-Write Operations - Complete
✅ **Phase 4**: Directory Operations - Complete
✅ **Phase 5**: Advanced Features - Complete
- ✅ Symlink Handling
- ✅ Performance Optimizations (caching)
- ⚠️ Special Cases (partial - hard links, file locking pending)

✅ **Phase 6**: Testing and Documentation - Complete
- ✅ Unit Tests (25 tests)
- ✅ Integration Tests (9 tests)
- ✅ Performance Benchmarks (13 benchmarks)
- ✅ API Documentation
- ✅ Performance Guide

#### Known Limitations

1. **Hard Links**: Not supported across layers
2. **File Locking**: Behavior is filesystem-dependent
3. **Memory-mapped Files**: Limited support depending on underlying filesystem
4. **Symlinks**: MemMapFs doesn't support symlinks (test skipped)
5. **Multiple Writable Layers**: Only one writable layer supported
6. **Layer Count**: Performance degrades with >10 layers

#### Dependencies

- `github.com/spf13/afero` v1.15.0 - Filesystem abstraction
- `golang.org/x/text` v0.28.0 - Text processing (transitive)
- Go 1.23+ - Language runtime

#### Compatibility

- **Go Version**: 1.23.0+
- **Platforms**: Linux, macOS, Windows
- **Architecture**: amd64, arm64
- **Interface**: Fully compatible with `afero.Fs`

## Future Enhancements

Potential additions for future releases:

- Multiple writable layers support
- Layer compaction and squashing
- Diff generation between layer states
- Layer import/export (tar/zip)
- Network-backed remote layers
- Encryption at layer boundary
- Compression per layer
- Layer deduplication
- Snapshot management
- Garbage collection

## License

MIT License - see LICENSE file for details
