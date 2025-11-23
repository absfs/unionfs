# UnionFS - Multi-Layer Filesystem Composition

A layered filesystem implementation for Go providing Docker-style overlay filesystem capabilities with copy-on-write support.

## Overview

UnionFS enables the composition of multiple filesystem layers into a single unified view. This is similar to how Docker and other container systems build images through layering, where each layer can add, modify, or delete files from lower layers.

**Key Features:**
- Multiple filesystem layer composition
- Copy-on-write (CoW) semantics for modifications
- Whiteout support for deletions across layers
- Read-only base layers with writable overlay
- Efficient file lookup through layer precedence
- Full `afero.Fs` interface compatibility

**Use Cases:**
- Container filesystem implementations
- Immutable base configurations with user overrides
- Multi-tenant systems with shared base + per-tenant customization
- Build systems with cached base layers
- Testing with fixture data + test-specific modifications

## Architecture

```
┌─────────────────────────────────────────────────────────────┐
│                      UnionFS Interface                       │
│                     (afero.Fs compatible)                    │
└─────────────────────────────────────────────────────────────┘
                              │
                              ▼
┌─────────────────────────────────────────────────────────────┐
│                    Layer Resolution Logic                    │
│  - Path lookup through layers (top to bottom)                │
│  - Whiteout detection (.wh.filename markers)                 │
│  - Copy-on-write for modifications                           │
│  - Directory merging across layers                           │
└─────────────────────────────────────────────────────────────┘
                              │
        ┌─────────────────────┼─────────────────────┐
        ▼                     ▼                     ▼
┌──────────────┐     ┌──────────────┐     ┌──────────────┐
│  Layer 2     │     │  Layer 1     │     │  Layer 0     │
│ (Writable)   │     │ (Read-only)  │     │ (Read-only)  │
│              │     │              │     │              │
│ /app/config/ │     │ /app/bin/    │     │ /usr/lib/    │
│   custom.yml │     │   myapp      │     │   libc.so    │
│ .wh.old.yml  │     │ /app/config/ │     │ /usr/bin/    │
│              │     │   default.yml│     │   bash       │
└──────────────┘     └──────────────┘     └──────────────┘
  (Top layer)        (Middle layer)       (Base layer)

Logical View:
/usr/lib/libc.so     → Layer 0
/usr/bin/bash        → Layer 0
/app/bin/myapp       → Layer 1
/app/config/default.yml → Layer 1
/app/config/custom.yml  → Layer 2 (added)
/app/config/old.yml     → (deleted via whiteout in Layer 2)
```

## Implementation Plan

### Phase 1: Core Layer Management

**Layer Stack**
- Define `Layer` type wrapping `afero.Fs` with metadata
- Implement layer ordering (top = highest precedence)
- Layer validation and initialization
- Support for read-only vs writable layer designation

**Path Resolution**
- Traverse layers from top to bottom for file lookup
- First match wins for file operations
- Implement efficient path caching with invalidation
- Handle absolute vs relative path normalization

### Phase 2: Whiteout Support

**Deletion Semantics**
- Implement AUFS-style whiteout files (`.wh.filename`)
- Directory whiteout markers (`.wh.__dir_opaque`)
- Whiteout detection during path resolution
- Proper handling of directory deletion across layers

**File Operations with Whiteouts**
- Create whiteout on file deletion in overlay layer
- Remove whiteout when file is recreated
- Stat operations respect whiteouts
- Directory listing excludes whiteout entries

### Phase 3: Copy-on-Write Operations

**Write Operations**
- Detect writes to files in lower (read-only) layers
- Copy file to writable layer before modification
- Preserve file metadata (permissions, timestamps)
- Handle partial writes and seeks efficiently

**File Modification**
- OpenFile with write flags triggers CoW
- Truncate operations
- Chmod, Chown, Chtimes propagation
- Atomic operations where possible

### Phase 4: Directory Operations

**Directory Merging**
- Merge directory listings across all layers
- Apply whiteouts to merged results
- Handle duplicate entries (top layer wins)
- Efficient iteration with lazy evaluation

**Directory Mutations**
- MkdirAll across layer boundaries
- RemoveAll with whiteout creation
- Rename within and across layers
- Directory permission updates

### Phase 5: Advanced Features

**Symlink Handling**
- Resolve symlinks within layer context
- Cross-layer symlink resolution
- Detect and prevent symlink loops
- Relative vs absolute symlink handling

**Performance Optimizations**
- Negative lookup caching (non-existent paths)
- Stat cache with TTL or invalidation
- Lazy layer initialization
- Concurrent layer access where safe

**Special Cases**
- Hard link handling (may not span layers)
- File locking behavior across layers
- Memory-mapped file support
- Large file optimization

### Phase 6: Testing and Documentation

**Test Coverage**
- Unit tests for each layer operation
- Integration tests for multi-layer scenarios
- Property-based tests for filesystem invariants
- Benchmark suite for performance testing
- Compatibility tests with afero test suite

**Documentation**
- API documentation with examples
- Architecture decision records
- Performance tuning guide
- Migration guide from other union filesystem implementations

## API Design

### Basic Usage

```go
package main

import (
    "github.com/absfs/unionfs"
    "github.com/spf13/afero"
)

func main() {
    // Create base layer (read-only)
    baseLayer := afero.NewOsFs()

    // Create overlay layer (writable)
    overlayLayer := afero.NewMemMapFs()

    // Create union filesystem
    ufs := unionfs.New(
        unionfs.WithWritableLayer(overlayLayer),
        unionfs.WithReadOnlyLayer(baseLayer),
    )

    // Reads fall through to base layer if not in overlay
    data, err := afero.ReadFile(ufs, "/etc/config.yml")

    // Writes go to overlay layer
    err = afero.WriteFile(ufs, "/etc/custom.yml", []byte("key: value"), 0644)

    // Modifications trigger copy-on-write
    file, err := ufs.OpenFile("/etc/config.yml", os.O_RDWR, 0)
    file.Write([]byte("modified")) // Copies to overlay first
}
```

### Advanced Configuration

```go
// Multiple read-only layers with single writable layer
ufs := unionfs.New(
    unionfs.WithWritableLayer(overlayLayer),      // Top: writable
    unionfs.WithReadOnlyLayer(configLayer),       // Middle: configs
    unionfs.WithReadOnlyLayer(appLayer),          // Middle: app files
    unionfs.WithReadOnlyLayer(systemLayer),       // Bottom: system files
)

// Custom whiteout strategy
ufs := unionfs.New(
    unionfs.WithWritableLayer(overlayLayer),
    unionfs.WithReadOnlyLayer(baseLayer),
    unionfs.WithWhiteoutFormat(unionfs.OpaqueWhiteout),
)

// Performance tuning
ufs := unionfs.New(
    unionfs.WithWritableLayer(overlayLayer),
    unionfs.WithReadOnlyLayer(baseLayer),
    unionfs.WithStatCache(true, 5*time.Minute),
    unionfs.WithCopyBufferSize(128*1024),
)
```

### Container-Style Workflow

```go
// Docker-style layer building
func BuildContainerFS() afero.Fs {
    // Layer 0: Base OS
    baseOS := loadLayer("ubuntu:latest")

    // Layer 1: App dependencies
    deps := loadLayer("app-deps:1.0")

    // Layer 2: Application code
    app := loadLayer("myapp:2.3.4")

    // Layer 3: Runtime modifications
    runtime := afero.NewMemMapFs()

    return unionfs.New(
        unionfs.WithWritableLayer(runtime),
        unionfs.WithReadOnlyLayer(app),
        unionfs.WithReadOnlyLayer(deps),
        unionfs.WithReadOnlyLayer(baseOS),
    )
}
```

## Usage Examples

### Example 1: Configuration Override System

```go
// Base configuration + environment-specific overrides
func LoadConfig(env string) afero.Fs {
    baseConfig := afero.NewOsFs() // /etc/app/defaults/
    envConfig := afero.NewOsFs()  // /etc/app/env/production/
    userConfig := afero.NewMemMapFs() // Runtime overrides

    ufs := unionfs.New(
        unionfs.WithWritableLayer(userConfig),
        unionfs.WithReadOnlyLayer(envConfig),
        unionfs.WithReadOnlyLayer(baseConfig),
    )

    // Read config.yml - checks user, then env, then base
    config, _ := afero.ReadFile(ufs, "/config.yml")
    return ufs
}
```

### Example 2: Testing with Fixtures

```go
// Immutable test fixtures + test-specific modifications
func TestWithFixtures(t *testing.T) {
    fixtures := afero.NewReadOnlyFs(afero.NewOsFs())
    testOverlay := afero.NewMemMapFs()

    ufs := unionfs.New(
        unionfs.WithWritableLayer(testOverlay),
        unionfs.WithReadOnlyLayer(fixtures),
    )

    // Tests can modify files without affecting fixtures
    afero.WriteFile(ufs, "/data/test.json", []byte("{}"), 0644)

    // Cleanup is automatic - just discard testOverlay
}
```

### Example 3: Multi-Tenant Application

```go
// Shared application base + tenant-specific customization
func TenantFilesystem(tenantID string) afero.Fs {
    // Shared application files (read-only)
    appBase := afero.NewReadOnlyFs(afero.NewOsFs())

    // Tenant-specific storage (writable)
    tenantFS := getTenantStorage(tenantID)

    ufs := unionfs.New(
        unionfs.WithWritableLayer(tenantFS),
        unionfs.WithReadOnlyLayer(appBase),
    )

    return ufs
}
```

## Test Strategy

### Unit Tests
- Individual layer operations (read, write, stat, etc.)
- Whiteout creation and detection
- Copy-on-write triggering and execution
- Path resolution through layer stack
- Edge cases: empty layers, missing files, permissions

### Integration Tests
- Multi-layer file operations
- Directory merging across layers
- Complex rename and move operations
- Concurrent access patterns
- Whiteout interaction with directory operations

### Compatibility Tests
- Full afero.Fs interface compliance
- Drop-in replacement verification
- Edge case compatibility with standard library
- Cross-platform behavior (Windows, Linux, macOS)

### Performance Tests
- Lookup performance vs layer count
- Copy-on-write overhead measurement
- Directory merge scalability
- Cache effectiveness
- Memory usage profiling

### Property-Based Tests
- Filesystem invariants (e.g., after write, read succeeds)
- Layer precedence consistency
- Whiteout correctness
- CoW correctness (original unchanged)

## Performance Considerations

### Lookup Optimization
- **Stat caching**: Cache file existence and metadata with TTL
- **Negative caching**: Remember non-existent paths to avoid repeated layer scans
- **Path index**: Optional in-memory index of all files across layers
- **Lazy layer loading**: Don't scan layers until needed

### Copy-on-Write Efficiency
- **Sparse file support**: Preserve sparse files during copy
- **Streaming copy**: Use buffered I/O for large files
- **Metadata-only CoW**: Some operations only need metadata copy
- **Deferred copy**: Delay copy until actual write occurs

### Directory Operations
- **Lazy merging**: Only merge directories when iterated
- **Iterator chaining**: Stream results from multiple layers
- **Duplicate elimination**: Efficient dedup of directory entries
- **Parallel layer scan**: Concurrent ReadDir on independent layers

### Memory Usage
- **Stream-based operations**: Avoid loading entire files
- **Limited cache size**: Bounded stat cache with LRU eviction
- **Shared layer instances**: Reuse layer objects across union instances
- **Lazy whiteout detection**: Check for whiteouts on-demand

### Scaling Considerations
- **Layer count limits**: Performance degrades with many layers (>10-20)
- **File count**: Large directories may need special handling
- **Deep paths**: Nested directory structures increase lookup cost
- **Write amplification**: CoW can cause significant copying

## Related Projects

### Docker Overlay2
- **URL**: https://docs.docker.com/storage/storagedriver/overlayfs-driver/
- **Description**: Docker's default storage driver using OverlayFS
- **Key Ideas**: Whiteout files, opaque directories, efficient layering

### AUFS (Another UnionFS)
- **URL**: http://aufs.sourceforge.net/
- **Description**: Original union filesystem for Linux
- **Key Ideas**: Branch layering, whiteout markers, copy-on-write

### OverlayFS
- **URL**: https://www.kernel.org/doc/html/latest/filesystems/overlayfs.html
- **Description**: Linux kernel union filesystem
- **Key Ideas**: Upper/lower layers, workdir for atomic operations

### go-dockerclient overlayfs
- **URL**: https://github.com/fsouza/go-dockerclient
- **Description**: Docker client with overlay filesystem utilities
- **Key Ideas**: Container filesystem management patterns

### afero LayeredFs (inspiration)
- **URL**: https://github.com/spf13/afero
- **Description**: Afero's experimental layered filesystem
- **Key Ideas**: Layer composition, base + overlay pattern

### containerd snapshotter
- **URL**: https://github.com/containerd/containerd/tree/main/snapshots
- **Description**: Container snapshot management
- **Key Ideas**: Snapshot chains, parent-child relationships

## Implementation Notes

### Whiteout Format
Following AUFS/Docker conventions:
- File deletion: `.wh.filename` in same directory
- Directory deletion: `.wh.__dir_opaque` marker
- Whiteout files are hidden from normal directory listings

### Layer Precedence
- Layers are searched top to bottom
- First match wins for file lookup
- Writes always go to topmost writable layer
- Only one writable layer supported initially

### Atomic Operations
- File creation: Direct write to writable layer
- File modification: CoW then modify
- File deletion: Create whiteout in writable layer
- Directory operations: May span multiple layers

### Error Handling
- Read errors in lower layers are masked if higher layer has file
- Write errors fail immediately
- Permission errors respect highest layer with file
- Path resolution errors bubble up

## Future Enhancements

- Multiple writable layers (advanced use cases)
- Layer compaction and squashing
- Efficient diff generation between layer states
- Layer import/export (tar/zip archives)
- Network-backed remote layers
- Encryption at layer boundary
- Compression per layer
- Layer deduplication
- Snapshot management
- Layer garbage collection

## Contributing

Contributions welcome! Please ensure:
- Full test coverage for new features
- Benchmark comparisons for performance changes
- Documentation updates
- Afero interface compatibility maintained

## License

MIT License - see LICENSE file for details
