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

File lookups traverse layers from top to bottom:
- **Without caching**: ~900-1000 ns per Stat operation
- **With caching**: ~100-130 ns per Stat operation (**~9x faster**)

Layer depth impact (bottom layer lookup):
- 2 layers: ~1,600 ns
- 5 layers: ~4,000 ns
- 10 layers: ~10,300 ns

**Recommendation**: Keep layer count under 10 for optimal performance.

### Cache Performance

Negative lookups (non-existent files):
- **Without cache**: ~900-1100 ns
- **With cache**: ~130-160 ns (**~7x faster**)

**Recommendation**: Always enable caching for production workloads.

## Caching

### Enabling the Cache

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

| Workload Type | Stat TTL | Negative TTL | Rationale |
|--------------|----------|--------------|-----------|
| Read-heavy, static files | 15-60 min | 5-10 min | Files rarely change |
| Mixed read/write | 5-10 min | 1-2 min | Balance freshness and performance |
| Write-heavy | 1-2 min | 30 sec | Need fresh data |
| Development | 30 sec | 10 sec | Files change frequently |

### Cache Invalidation

The cache is automatically invalidated on write operations:
- `Create`, `OpenFile` (write mode), `Mkdir`, `MkdirAll`
- `Remove`, `RemoveAll`, `Rename`
- `Chmod`, `Chown`, `Chtimes`

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

Performance degrades linearly with layer depth:
```
2 layers:  baseline
5 layers:  2.5x slower
10 layers: 6.3x slower
```

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

Benchmark results:
- Simple read: ~1,800 ns
- Simple write: ~3,700 ns
- Copy-on-write: ~26,000 ns (**~7x slower than write**)

### Minimizing CoW Overhead

1. **Pre-populate writable layer** for files you know will be modified:
   ```go
   // Instead of relying on CoW, explicitly copy upfront
   data, _ := afero.ReadFile(baseLayer, "/config.txt")
   afero.WriteFile(overlay, "/config.txt", data, 0644)
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

3. **Batch metadata operations**:
   ```go
   // Bad: Multiple CoW operations
   ufs.Chmod("/file", 0600)
   ufs.Chtimes("/file", atime, mtime)

   // Better: Single CoW, then modify
   data, _ := afero.ReadFile(ufs, "/file")
   afero.WriteFile(ufs, "/file", data, 0600)
   ufs.Chtimes("/file", atime, mtime)
   ```

## Directory Operations

### Directory Merging Performance

Benchmark: 150 files across 3 layers = ~130,000 ns (~200 μs)

This scales roughly as: `O(files × layers)`

**Optimization strategies**:

1. **Cache directory reads** at application level:
   ```go
   var dirCache map[string][]os.FileInfo

   func getCachedDir(path string) []os.FileInfo {
       if entries, ok := dirCache[path]; ok {
           return entries
       }
       entries, _ := afero.ReadDir(ufs, path)
       dirCache[path] = entries
       return entries
   }
   ```

2. **Limit directory size** - Split large directories:
   - Bad: `/data/` with 10,000 files
   - Good: `/data/shard0/`, `/data/shard1/`, etc. with 1,000 files each

3. **Use opaque directories** for complete overrides:
   ```go
   // Create opaque marker to hide all lower layers
   overlay.Create("/data/.wh.__dir_opaque")
   ```

### Whiteout Performance

Whiteout lookup: ~730 ns (faster than normal stat)

Whiteouts are efficient - don't hesitate to use `Remove()`/`RemoveAll()`.

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

        if stats.StatCacheSize > stats.MaxEntries*0.9 {
            log.Warn("Cache approaching capacity")
        }
    }
}()
```

## Best Practices

### General Guidelines

1. **Always enable caching** in production
   ```go
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
    overlay := afero.NewMemMapFs()
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

### ❌ Anti-Pattern 3: Frequent CoW of Large Files

```go
// BAD: Triggers 100MB CoW every time
ufs.Chmod("/huge-database.db", 0600)
```

**Solution**: Pre-copy to writable layer or avoid modifications

### ❌ Anti-Pattern 4: Single Large Directory

```go
// BAD: 10,000 files in one directory
for i := 0; i < 10000; i++ {
    afero.WriteFile(ufs, fmt.Sprintf("/data/file%d", i), data, 0644)
}
```

**Solution**: Shard into subdirectories

## Troubleshooting

### Problem: Slow File Lookups

**Symptoms**: High latency on `Stat()` calls

**Solutions**:
1. Enable caching: `WithStatCache(true, 5*time.Minute)`
2. Reduce layer count
3. Move frequently accessed files to upper layers

### Problem: High Memory Usage

**Symptoms**: Growing memory consumption

**Solutions**:
1. Reduce cache size: `WithCacheConfig(..., maxEntries=500)`
2. Reduce cache TTL to allow faster expiration
3. Reduce copy buffer size

### Problem: Stale Data

**Symptoms**: Reading old file contents

**Solutions**:
1. Reduce cache TTL
2. Manually invalidate: `ufs.InvalidateCache(path)`
3. Don't modify layers directly - always use UnionFS interface

### Problem: Slow Writes

**Symptoms**: Write operations taking long

**Solutions**:
1. Increase copy buffer size for large files
2. Pre-populate writable layer to avoid CoW
3. Check underlying filesystem performance

## Version History

- **v1.0.0**: Initial performance tuning guide
- Added caching recommendations, benchmarks, and best practices
