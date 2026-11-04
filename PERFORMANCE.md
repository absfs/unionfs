# UnionFS Performance Tuning Guide

This guide provides best practices and configuration options for optimizing UnionFS performance in various scenarios.

## Table of Contents

- [Performance Characteristics](#performance-characteristics)
- [Caching](#caching)
- [Layer Organization](#layer-organization)
- [Copy-on-Write Optimization](#copy-on-write-optimization)
- [Directory Operations](#directory-operations)
- [Benchmarking](#benchmarking)
- [Best Practices](#best-practices)

## Performance Characteristics

### Lookup Performance

A path is resolved one element at a time, as a kernel overlay does. For
each element the union asks only the layers that still contribute to the
directory it is in:

- every contributing layer gets one `Lstat` of the name;
- a layer that has the name as a directory, and is not the last one,
  also gets one probe for the opaque marker inside it;
- a layer that lacks the name, and is not the last one, gets one probe for
  its whiteout;
- a regular file found below the top contributing layer gets one probe for
  a metadata marker in each contributing layer above it.

Once the walk reaches a directory that only one layer has, every further
element costs a single `Lstat` in that layer. In the common workspace shape
(a writable upper over one big lower), a lookup deep in a tree the upper
has never touched costs about one call per element, plus three for the
first element that is missing from the upper.

Measured on memfs layers (Apple M-series, `go test -bench . -benchtime
2000x`); host-directory layers add a system call per probe:

| Benchmark                  | ns/op  |
| -------------------------- | ------ |
| Stat, lower file, no cache | ~450   |
| Stat, cached               | ~55    |
| Missing path, no cache     | ~510   |
| Missing path, cached       | ~75    |
| Lookup through 2 layers    | ~650   |
| Lookup through 5 layers    | ~1,200 |
| Lookup through 10 layers   | ~2,400 |

**Recommendation**: Keep layer count under 10 for optimal performance.

### Lstat, and the lookup cache

`Lstat` is what an NFS server calls most (GETATTR, LOOKUP). Without a
cache it walks the path as above: 5 layer calls for `/d001/f001.txt` and 7
for `/.git/objects/pack/p.idx` in a lower the upper has not touched. With
`WithLookupCache`, a repeated lookup reads only the final element's
attributes from its layer, one call (two for a file a metadata marker
changes); with the Stat cache, which now holds `Lstat` results too, none.
`TestLstatLayerCalls` counts these.

`BenchmarkLstat` (`go test -run XXX -bench BenchmarkLstat -benchtime
20000x`, Apple M-series, ns/op, an empty upper over the lower):

| Layers | Path                       | Lower alone | No cache, before | No cache | Lookup cache | Stat cache, before | Stat cache |
| ------ | -------------------------- | ----------- | ---------------- | -------- | ------------ | ------------------ | ---------- |
| memfs  | `/d001/f001.txt`           | 75          | 590              | 610      | 300          | 580                | 75         |
| memfs  | `/.git/objects/pack/p.idx` | 85          | 935              | 975      | 490          | 940                | 90         |
| osfs   | `/d001/f001.txt`           | 1,290       | 6,350            | 6,110    | 1,590        | 6,110              | 75         |
| osfs   | `/.git/objects/pack/p.idx` | 1,350       | 9,290            | 9,850    | 1,810        | 9,190              | 90         |

"Before" is the same benchmark on the code before the lookup cache and the
`Lstat` entries in the Stat cache (the Stat cache did not cover `Lstat`
then). osfs is basefs over host directories.

The lookup cache caches structure, not attributes: which layer holds each
name, which markers apply, which names are missing. It stays exact under
writes through open handles, which the Stat cache does not, so it suits an
NFS server. Changes made through the union drop what they affect; changes
made to a layer directly show once the entries expire, or after
`InvalidateCache`, `InvalidateCacheTree` or `ClearCache` (pass the topmost
path the direct change created or removed: a directory added to the upper
also changes where every name under it is found).

A Stat cache holds at most `maxEntries` results (1000 with
`WithStatCache`), and `Stat` and `Lstat` results share that bound. A
working set larger than the bound, looked up in turn, misses on every call.
`BenchmarkLstatWorkingSet` cycles `Lstat` over 5000 lower files in one
osfs directory (ns/op):

| No cache | Stat cache, before | Stat cache | Lookup cache |
| -------- | ------------------ | ---------- | ------------ |
| 7,400    | 18,700             | 7,500      | 2,200        |

"Before" is the eviction this release replaced, which scanned the whole
cache for its oldest entry on every insert once the cache was full; a
full cache now evicts in constant time, so a working set that does not fit
costs what no cache does. The lookup cache's default bound is 100000 names.

### Cache Performance

Negative lookups (non-existent files), from the table above:
- **Without cache**: ~510 ns
- **With cache**: ~75 ns (**~7x faster**)

**Recommendation**: Always enable caching for production workloads.

## Caching

There are two caches. The lookup cache (`WithLookupCache`) caches path
resolution and never serves attributes; the Stat cache (`WithStatCache`,
`WithCacheConfig`) serves whole `Stat` and `Lstat` results, so it is
faster but can serve a stale size after writes through an open handle.

### Enabling the Lookup Cache

```go
ufs := unionfs.New(
    unionfs.WithWritableLayer(overlay),
    unionfs.WithReadOnlyLayer(baseLayer),
    unionfs.WithLookupCache(time.Hour, 0), // TTL, max names (0: 100000)
)
```

### Enabling the Stat Cache

```go
ufs := unionfs.New(
    unionfs.WithWritableLayer(overlay),
    unionfs.WithReadOnlyLayer(baseLayer),
    unionfs.WithStatCache(true, 5*time.Minute), // Enable with 5min TTL
)
```

### Custom Cache Configuration

For fine-grained control:

```go
ufs := unionfs.New(
    unionfs.WithWritableLayer(overlay),
    unionfs.WithReadOnlyLayer(baseLayer),
    unionfs.WithCacheConfig(
        true,                    // enabled
        10*time.Minute,         // stat cache TTL
        1*time.Minute,          // negative cache TTL (shorter)
        5000,                   // max cache entries
    ),
)
```

### Cache TTL Guidelines

| Workload Type            | Stat TTL  | Negative TTL | Rationale                         |
| ------------------------ | --------- | ------------ | --------------------------------- |
| Read-heavy, static files | 15-60 min | 5-10 min     | Files rarely change               |
| Mixed read/write         | 5-10 min  | 1-2 min      | Balance freshness and performance |
| Write-heavy              | 1-2 min   | 30 sec       | Need fresh data                   |
| Development              | 30 sec    | 10 sec       | Files change frequently           |

### Cache Invalidation

The cache is automatically invalidated on write operations:
- `Create`, `OpenFile` (write mode), `Mkdir`, `MkdirAll`, `Symlink`
- `Remove`, `RemoveAll`, `Rename`
- `Chmod`, `Chown`, `Lchown`, `Chtimes`, `Truncate`

It is not invalidated by changes made to a layer directly, by writes
through an open file handle after `OpenFile` returned it, or by changes
seen through another symbolic link to the same file; see the Stat cache
caveat in the [README](README.md#caveats).

Manual invalidation when needed:

```go
// Invalidate single path
ufs.InvalidateCache("/path/to/file")

// Invalidate entire subtree
ufs.InvalidateCacheTree("/path/to/dir")

// Clear all cache
ufs.ClearCache()

// Check cache statistics
stats := ufs.CacheStats()
fmt.Printf("Cache size: %d entries\n", stats.StatCacheSize)
```

### When NOT to Use Caching

- **External modifications**: If files are modified outside UnionFS (directly in layers)
- **Very short-lived filesystems**: Setup/teardown overhead may exceed benefits
- **Extremely memory-constrained environments**: Cache uses ~100-200 bytes per entry

## Layer Organization

### Layer Count

Lookups grow with the number of layers that contribute to each directory
on the path, about linearly (see the table above).

**Recommendations**:
1. Merge infrequently changing layers when possible
2. Place frequently accessed files in higher layers
3. Consider layer squashing for production deployments

### Layer Ordering Strategy

Place layers in this order for best performance:

1. **Writable overlay** (top) - Active modifications
2. **Application-specific files** - Frequently accessed
3. **Dependencies** - Moderately accessed
4. **System files** - Base, rarely accessed

**Example**:
```go
ufs := unionfs.New(
    WithWritableLayer(runtimeChanges),        // Layer 0 (top)
    WithReadOnlyLayer(appConfig),            // Layer 1 - frequently read
    WithReadOnlyLayer(appBinaries),          // Layer 2
    WithReadOnlyLayer(langRuntime),          // Layer 3
    WithReadOnlyLayer(systemLibraries),      // Layer 4
    WithReadOnlyLayer(baseOS),               // Layer 5 (bottom)
)
```

### File Placement

- **Hot files** (accessed frequently): Upper layers
- **Cold files** (accessed rarely): Lower layers
- **Large files**: Lower layers (avoid CoW overhead)

## Copy-on-Write Optimization

### CoW Performance Impact

A file is copied up the first time it is opened for writing, truncated or
renamed: its contents (unless the open truncates it, or Truncate cuts it
to zero), mode and modification time. Changing its mode, owner or times
copies nothing: it writes a metadata marker.
Its directory, and the directories above it, are copied up first, as empty
directories with their modes and times. Each entry copied into a
directory costs one `Lstat` and one `Chtimes` of that directory (two
`Chmod`s more when it is read-only), so that the copy-up leaves the
directory's mode and modification time as they were. Renaming a directory
that lower layers contribute to copies its whole merged tree up, so it
costs time proportional to the tree.

### Minimizing CoW Overhead

1. **Pre-populate writable layer** for files you know will be modified:
   ```go
   // Instead of relying on CoW, explicitly copy upfront
   data, _ := baseLayer.ReadFile("/config.txt")
   f, _ := overlay.Create("/config.txt")
   f.Write(data)
   f.Close()
   ```

2. **Adjust copy buffer size** for large files:
   ```go
   ufs := unionfs.New(
       WithWritableLayer(overlay),
       WithReadOnlyLayer(baseLayer),
       WithCopyBufferSize(128*1024), // 128KB buffer for large files
   )
   ```
   - Default: 32KB
   - For files >10MB: 128KB - 256KB
   - For files >100MB: 512KB - 1MB

3. **Metadata changes copy no data**:
   ```go
   // Both write one empty .wh..wh.meta.file marker in the writable
   // layer; the data stays in the lower until the file is written.
   ufs.Chmod("/file", 0600)
   ufs.Chtimes("/file", atime, mtime)
   ```

## Directory Operations

### Directory Merging Performance

Benchmark: 150 files across 3 layers = ~28,000 ns on memfs

This scales roughly as: `O(files × layers)`

**Optimization strategies**:

1. **Cache directory reads** at application level:
   ```go
   var dirCache map[string][]fs.DirEntry

   func getCachedDir(path string) []fs.DirEntry {
       if entries, ok := dirCache[path]; ok {
           return entries
       }
       entries, _ := ufs.ReadDir(path)
       dirCache[path] = entries
       return entries
   }
   ```

2. **Limit directory size** - Split large directories:
   - Bad: `/data/` with 10,000 files
   - Good: `/data/shard0/`, `/data/shard1/`, etc. with 1,000 files each

3. **Use opaque directories** for complete overrides:
   ```go
   // Remove the directory and make it again: the new one is opaque, so
   // the union never reads the lower layers' copies of it.
   ufs.RemoveAll("/data")
   ufs.Mkdir("/data", 0755)
   ```

### Whiteout Performance

Whiteout lookup: ~470 ns on memfs (a lookup that stops at the whiteout).

Whiteouts are efficient - don't hesitate to use `Remove()`/`RemoveAll()`.
`RemoveAll` of a tree a lower layer has writes a single whiteout, whatever
the tree's size.

Before it writes a whiteout, metadata marker or copy-up for an entry a
lower layer has, the union finds how that layer spells the entry's name
(see the README's changeset section). For an ASCII name on a host that
does not fold case this is one extra `Lstat`. On a host that folds case
(macOS, Windows), and for other names, it reads the lower directory's
listing; the last listing is kept while the directory's modification time
stays the same, unless the directory changed in the last two seconds. On
APFS, removing each of 5,000 lower files one by one cost 89 µs per
`Remove` with the fix and without it; in a lower directory changed within
the last two seconds, it cost 478 µs per `Remove` against 77 µs, since
each read the whole listing.

## Benchmarking

### Running Benchmarks

```bash
# Run all benchmarks
go test -bench=. -benchmem

# Run specific benchmark
go test -bench=BenchmarkStatWithCache -benchmem

# Compare before/after
go test -bench=. -benchmem > before.txt
# ... make changes ...
go test -bench=. -benchmem > after.txt
benchstat before.txt after.txt
```

### Key Metrics to Monitor

1. **Stat operations/sec** - Should be >1M ops/sec with caching
2. **Read throughput** - Dependent on underlying filesystem
3. **Write amplification** - CoW ratio (should be <20%)
4. **Cache hit rate** - Monitor `CacheStats()` in production

### Production Monitoring

```go
// Periodic cache stats logging
go func() {
    ticker := time.NewTicker(5 * time.Minute)
    for range ticker.C {
        stats := ufs.CacheStats()
        log.Printf("UnionFS cache: %d entries, TTL=%v",
            stats.StatCacheSize, stats.StatTTL)

        if stats.StatCacheSize > stats.MaxEntries*9/10 {
            log.Printf("UnionFS cache approaching capacity")
        }
    }
}()
```

## Best Practices

### General Guidelines

1. **Enable a cache** in production: the lookup cache when files are
   written through long-lived handles (an NFS server), the Stat cache too
   when they are not
   ```go
   WithLookupCache(time.Hour, 0)
   WithStatCache(true, 5*time.Minute)
   ```

2. **Limit layer depth** to ≤ 10 layers

3. **Place hot data in upper layers**

4. **Use appropriate buffer sizes** based on file sizes

5. **Monitor cache hit rates** and adjust TTL accordingly

### Use Case Specific Recommendations

#### Container Filesystems (Docker-like)

```go
ufs := unionfs.New(
    WithWritableLayer(container),
    WithReadOnlyLayer(appLayer),
    WithReadOnlyLayer(depsLayer),
    WithReadOnlyLayer(baseImage),
    WithStatCache(true, 15*time.Minute),     // Long TTL, images are immutable
    WithCopyBufferSize(128*1024),            // Large buffer for image files
)
```

#### Configuration Management

```go
ufs := unionfs.New(
    WithWritableLayer(userConfig),
    WithReadOnlyLayer(envConfig),
    WithReadOnlyLayer(defaultConfig),
    WithStatCache(true, 5*time.Minute),      // Moderate TTL
    WithCopyBufferSize(32*1024),             // Small buffer, configs are small
)
```

#### Testing Frameworks

```go
// Create fresh overlay for each test
func TestWithFixtures(t *testing.T) {
    overlay, _ := memfs.NewFS()
    ufs := unionfs.New(
        WithWritableLayer(overlay),
        WithReadOnlyLayer(fixtures),
        // No caching - tests are short-lived
    )
    // ... test code ...
    // Overlay is discarded after test
}
```

#### Build Systems

```go
ufs := unionfs.New(
    WithWritableLayer(buildDir),
    WithReadOnlyLayer(cacheLayer),           // Cached dependencies
    WithReadOnlyLayer(sourceLayer),
    WithStatCache(true, 30*time.Second),     // Short TTL, files change often
    WithCopyBufferSize(256*1024),            // Large buffer for binaries
)
```

### Memory Optimization

For memory-constrained environments:

```go
ufs := unionfs.New(
    WithWritableLayer(overlay),
    WithReadOnlyLayer(baseLayer),
    WithCacheConfig(true, 2*time.Minute, 30*time.Second, 500), // Smaller cache
    WithCopyBufferSize(16*1024),             // Smaller buffer
)
```

### Profiling

Use Go's profiling tools to identify bottlenecks:

```go
import _ "net/http/pprof"

go func() {
    log.Println(http.ListenAndServe("localhost:6060", nil))
}()
```

Then access:
- CPU profile: `http://localhost:6060/debug/pprof/profile?seconds=30`
- Heap profile: `http://localhost:6060/debug/pprof/heap`
- Goroutines: `http://localhost:6060/debug/pprof/goroutine`

## Common Performance Anti-Patterns

### ❌ Anti-Pattern 1: Too Many Layers

```go
// BAD: 20+ layers
for _, layer := range manyLayers {
    opts = append(opts, WithReadOnlyLayer(layer))
}
```

**Solution**: Merge or squash layers

### ❌ Anti-Pattern 2: No Caching in Production

```go
// BAD: No cache
ufs := unionfs.New(
    WithWritableLayer(overlay),
    WithReadOnlyLayer(baseLayer),
)
```

**Solution**: Always enable caching

### ❌ Anti-Pattern 3: Opening Large Lower Files for Writing

```go
// BAD: copies the whole 100MB file up, though nothing is written
f, _ := ufs.OpenFile("/huge-database.db", os.O_RDWR, 0)
```

Opening a file only a lower layer has for writing (`O_WRONLY`, `O_RDWR`
or `O_APPEND`, even with no write after it), truncating it to a non-zero
size, or renaming it copies the whole file up. `Chmod`, `Chown` and
`Chtimes` do not: they write an empty metadata marker.

**Solution**: Open for reading (`O_RDONLY`) unless you will write, and
pre-copy files you will modify often to the writable layer

### ❌ Anti-Pattern 4: Single Large Directory

```go
// BAD: 10,000 files in one directory
for i := 0; i < 10000; i++ {
    f, _ := ufs.Create(fmt.Sprintf("/data/file%d", i))
    f.Write(data)
    f.Close()
}
```

**Solution**: Shard into subdirectories

## Troubleshooting

### Problem: Slow File Lookups

**Symptoms**: High latency on `Stat()` calls

**Solutions**:
1. Enable caching: `WithLookupCache(time.Hour, 0)` or
   `WithStatCache(true, 5*time.Minute)`
2. If the paths looked up outnumber the Stat cache's bound, raise it with
   `WithCacheConfig` or use the lookup cache: a working set that does not
   fit misses every time
3. Reduce layer count
4. Move frequently accessed files to upper layers

### Problem: High Memory Usage

**Symptoms**: Growing memory consumption

**Solutions**:
1. Reduce cache size: `WithCacheConfig(..., maxEntries=500)`
2. Reduce cache TTL to allow faster expiration
3. Reduce copy buffer size

### Problem: Stale Data

**Symptoms**: Stale `Stat` or `Lstat` results (size, mode, modification
time or existence). The cache holds `Stat` and `Lstat` results only, never
file contents.

**Solutions**:
1. Reduce cache TTL
2. Manually invalidate: `ufs.InvalidateCache(path)`
3. Don't modify layers directly - always use UnionFS interface
4. After writing through a long-lived handle, call `InvalidateCache`, or
   leave the cache disabled

### Problem: Slow Writes

**Symptoms**: Write operations taking long

**Solutions**:
1. Increase copy buffer size for large files
2. Pre-populate writable layer to avoid CoW
3. Check underlying filesystem performance

## Version History

- **v1.0.0**: Initial performance tuning guide
- Added caching recommendations, benchmarks, and best practices
